// Package placement decides which member should run which of the session's
// three services (plan 30). It is pure: a function of the control-plane
// snapshot, with no I/O and no state.
//
// That purity is the point. Every member's daemon evaluates the same data and
// reaches the same answer, so they mostly agree without talking to each other —
// and when they don't, the lease's exclusivity makes the disagreement harmless.
// Nobody has to run a consensus protocol; they only have to race, and postgres
// settles it (plan 16's open question #6).
//
// Deliberately not a scheduler: three named services, one ranking function, no
// resource model, no bin-packing. If this grows a plugin interface, something
// has gone wrong.
package placement

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nwylynko/slopball-cli/controlplane"
)

// Result is the ranking for one service, best candidate first. An empty
// Eligible with a Reason is a *reported* unplaced state, never a silent nothing
// — a session where nobody can run the dev server should say so.
type Result struct {
	Service  string
	Eligible []controlplane.Member
	Reason   string
}

// Best returns the top-ranked member, or false when the service is unplaceable.
func (r Result) Best() (controlplane.Member, bool) {
	if len(r.Eligible) == 0 {
		return controlplane.Member{}, false
	}
	return r.Eligible[0], true
}

// Rank orders the members that could run this service, best first.
//
// Eligibility per service (the three portability stories of §4.3 / §10):
//   - git: any online admitted member — every client already keeps a full mirror (plan 11).
//   - dev: only members whose runtime matches the project's stack. This is the
//     genuinely hard one: code moves in seconds, runtime does not.
//   - conductor: only members reporting a harness CLI, and never a box — the
//     login stays on its owner's machine (§10).
//
// Preference: a box outranks every laptop for git and dev. That single rule is
// the entire "a box de-risks the session" story, expressed as data rather than
// as a special case in the code.
//
// Presence is not enough (plan 44): a left member keeps their secret but is not
// placeable — the filter is admitted && online, written out explicitly.
func Rank(sess controlplane.Session, service string) Result {
	res := Result{Service: service}
	var skipped []string
	for _, m := range sess.Members {
		// admitted && online — left (and pending) are never placeable, even if
		// a caller stuffed them into the snapshot (plan 44). Do not inherit this
		// from the presence check alone.
		if m.State != "" && m.State != controlplane.MemberAdmitted {
			continue
		}
		if !m.Online {
			continue
		}
		if ok, why := eligible(sess, m, service); ok {
			res.Eligible = append(res.Eligible, m)
		} else if why != "" {
			skipped = append(skipped, m.Name+" ("+why+")")
		}
	}
	sort.SliceStable(res.Eligible, func(i, j int) bool {
		return less(res.Eligible[i], res.Eligible[j], service)
	})
	if len(res.Eligible) == 0 {
		res.Reason = unplacedReason(sess, service, skipped)
	}
	return res
}

func eligible(sess controlplane.Session, m controlplane.Member, service string) (bool, string) {
	switch service {
	case controlplane.ServiceGit:
		return true, ""
	case controlplane.ServiceDev:
		want := stackRuntime(sess)
		if want == "" {
			return true, "" // the session never declared a stack; don't invent a constraint
		}
		if m.Capability == nil || !strings.EqualFold(m.Capability.Runtime, want) {
			return false, "no " + want
		}
		return true, ""
	case controlplane.ServiceConductor:
		if m.Role == controlplane.RoleBox {
			// §10: never copy a harness login onto the box. A box that reports a
			// harness is reporting one that was installed, not one that is logged
			// in as somebody — and that distinction is exactly the trust hole.
			return false, "box (harness login never lands on a box)"
		}
		if m.Harness == "" {
			return false, "no harness CLI"
		}
		return true, ""
	default:
		return false, "unknown service"
	}
}

// less orders two eligible members for a service. Every tie-break is total and
// data-derived so two daemons ranking the same snapshot cannot disagree.
func less(a, b controlplane.Member, service string) bool {
	if service != controlplane.ServiceConductor {
		// A box outranks any laptop for git and dev.
		if ab, bb := a.Role == controlplane.RoleBox, b.Role == controlplane.RoleBox; ab != bb {
			return ab
		}
	}
	if service == controlplane.ServiceGit {
		// Freshest replica first: it reconstructs canonical with the least loss.
		if a.MainMirrorHeight != b.MainMirrorHeight {
			return a.MainMirrorHeight > b.MainMirrorHeight
		}
	}
	if as, bs := score(a), score(b); as != bs {
		return as > bs
	}
	return a.ID < b.ID // last resort, but a total order — determinism over taste
}

func score(m controlplane.Member) int {
	if m.Capability == nil {
		return 0
	}
	return m.Capability.Score
}

func stackRuntime(sess controlplane.Session) string {
	if sess.Stack == nil {
		return ""
	}
	return sess.Stack.Runtime
}

func unplacedReason(sess controlplane.Session, service string, skipped []string) string {
	switch service {
	case controlplane.ServiceDev:
		want := stackRuntime(sess)
		if want == "" {
			want = "a runtime"
		}
		return fmt.Sprintf("no online member has %s%s", want, detail(skipped))
	case controlplane.ServiceConductor:
		return "no online member has a harness CLI logged in" + detail(skipped)
	default:
		return "no online members" + detail(skipped)
	}
}

func detail(skipped []string) string {
	if len(skipped) == 0 {
		return ""
	}
	return " — " + strings.Join(skipped, ", ")
}

// Holds reports whether memberID owns a live lease for the service. An owner
// renews what it holds rather than re-entering the race every tick.
func Holds(sess controlplane.Session, service, memberID string, now time.Time) bool {
	l, ok := sess.Leases[service]
	return ok && l.Owner == memberID && l.Live(now)
}

// ShouldClaim answers the one question each daemon asks per tick: is this
// service free, and am I the member that should take it?
//
// Free means unheld or expired. A LIVE lease is never claimable, however well
// this member ranks — that clause is what keeps plan 25's guarantee intact: a
// laptop cannot take hosting from a working box, with or without --takeover.
func ShouldClaim(sess controlplane.Session, service, memberID string, now time.Time) bool {
	if l, ok := sess.Leases[service]; ok && l.Live(now) {
		return false
	}
	best, ok := Rank(sess, service).Best()
	return ok && best.ID == memberID
}

// Describe renders one service's placement for the monitor and for log lines —
// "where is everything right now?" needs a one-glance answer once services can
// move on their own.
func Describe(sess controlplane.Session, service string, now time.Time) string {
	l, ok := sess.Leases[service]
	if ok && l.Live(now) {
		owner := l.OwnerName
		if owner == "" {
			owner = l.Owner
		}
		where := ""
		if l.Machine != "" {
			where = " (" + l.Machine + ")"
		}
		return fmt.Sprintf("%s%s  %ds left", owner, where, int(time.Until(l.ExpiresAt).Seconds()))
	}
	r := Rank(sess, service)
	if best, ok := r.Best(); ok {
		return fmt.Sprintf("unheld — %s next", best.Name)
	}
	return "unplaced — " + r.Reason
}
