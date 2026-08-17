package placement

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/logx"
)

var log = logx.New("placement")

// Loop is the per-member half of automatic placement: renew what I hold,
// evaluate what is free, claim what I rank first for, and stand down what I
// have lost. Every member runs one — the host loop and the join daemon alike —
// and they converge because Rank is pure and the lease is exclusive.
//
// The control plane must not be able to cause churn. When it is unreachable the
// loop changes NOTHING: the owner keeps serving on its cached lease and nobody
// claims anything. Only a *reachable* control plane reporting an *expired*
// lease authorizes a takeover. With a 30s TTL against a 2s heartbeat, a blip is
// invisible — this is §4.2's false-positive guard, moved from "ask a human" to
// "the arbiter says the lease is still live".
type Loop struct {
	Control  *controlplane.Client
	PIN      string
	MemberID string
	Name     string
	Machine  string

	// TTLSeconds is how long a claim lasts. Zero → controlplane.DefaultLeaseTTL.
	TTLSeconds int

	// Start brings a service up on this machine; Stop takes it down. Both are
	// the existing machinery — canonical + gitserver, devserver.Supervisor, the
	// conductor fleet — wired in by the caller. A Start that fails means the
	// lease is released rather than held by a member that is not serving.
	Start func(ctx context.Context, service string) error
	Stop  func(ctx context.Context, service string) error

	// Serves reports whether THIS PROCESS can run a service at all. Rank answers
	// "who among the members is best placed?"; this answers the prior question,
	// which ranking cannot see: a cloud box booted --serve-only has no harness
	// login, so it must never take the conductor however well it ranks.
	//
	// Without it a box claimed the conductor lease, had no Start wired, and
	// served nothing — while the control plane reported the conductor placed and
	// healthy and no other member would claim a lease that was already held.
	// Nil means "anything I rank first for", which is what every caller did
	// before this existed.
	Serves func(service string) bool

	// OnChange reports every placement change on one line, because "where is
	// everything right now?" must have a one-glance answer once services move
	// on their own.
	OnChange func(service, detail string)

	// Outbox, when set, is where renews deposit Hold instead of POSTing
	// /leases/.../renew themselves (plan 43). The caller's cycle Flush is what
	// actually renews. Claims stay their own request.
	Outbox *controlplane.Outbox

	mu       sync.Mutex
	held     map[string]bool
	unplaced map[string]string
	// failed is what this member could not start, keyed by service: the reason,
	// and the session fingerprint it failed against. It is the whole anti-flap
	// rule — see StateFingerprint — and it is per-process state on purpose: the
	// control plane holds the FACT (Lease.StartFailure), this holds the decision
	// not to try again yet.
	failed map[string]startFailure
}

// startFailure is one service's last failed start on this machine.
type startFailure struct {
	fingerprint string
	reason      string
}

// HoldsService reports whether this member currently serves the service.
func (l *Loop) HoldsService(service string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held[service]
}

// Unplaced returns the services nobody can currently run, with the reason.
func (l *Loop) Unplaced() map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]string, len(l.unplaced))
	for k, v := range l.unplaced {
		out[k] = v
	}
	return out
}

// Tick performs one placement round. It returns the control-plane error when
// the snapshot could not be read — the caller logs it, and nothing moves.
func (l *Loop) Tick(ctx context.Context) error {
	if l.Control == nil || l.PIN == "" || l.MemberID == "" {
		return nil
	}
	sess, err := l.Control.Session(ctx, l.PIN)
	if err != nil {
		// Placement freezes. Deliberately no stops here: an owner that cannot
		// reach the arbiter is still the owner, and the session keeps running on
		// cached endpoints exactly as the mesh doctrine requires.
		return err
	}
	now := time.Now()
	var firstErr error
	for _, service := range controlplane.Services {
		if err := l.reconcile(ctx, sess, service, now); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (l *Loop) reconcile(ctx context.Context, sess controlplane.Session, service string, now time.Time) error {
	if l.HoldsService(service) {
		return l.renew(ctx, service)
	}
	if l.Serves != nil && !l.Serves(service) {
		// Not ours to run. Note who is (or that nobody is) and move on — silence
		// here is how a session ends up with an unplaced service nobody reports.
		l.noteUnplaced(sess, service, now)
		return nil
	}
	// The control plane saying this is ours while we are not serving it is a
	// restart, or a Start that failed earlier; ShouldClaim is the free-lease
	// race. Both end in take().
	mine := Holds(sess, service, l.MemberID, now)
	if !mine && !ShouldClaim(sess, service, l.MemberID, now) {
		l.noteUnplaced(sess, service, now)
		return nil
	}
	fp := StateFingerprint(sess)
	if reason, stuck := l.stillFailing(service, fp); stuck {
		// Nothing that produced the failure has moved, so trying again would
		// produce the same error and the same lease flap. The lease still goes
		// back — another member may well be able to run this.
		log.Debugf("%s: not retrying here, nothing changed since %q", service, reason)
		if mine {
			l.reportStartFailure(ctx, service, reason)
		}
		return nil
	}
	if mine {
		return l.take(ctx, service, fp, "resuming a lease this machine still holds")
	}
	return l.take(ctx, service, fp, l.claimReason(sess, service, now))
}

// stillFailing reports the remembered reason when this member already failed to
// start the service against exactly this session state.
func (l *Loop) stillFailing(service, fingerprint string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.failed[service]
	return f.reason, ok && f.fingerprint == fingerprint
}

// noteStartFailure records the failure and answers whether it is NEW — a
// different reason from the last one this machine reported for the service.
// Only a new reason is worth a line: the same sentence every five seconds is
// what buried the git error in session 2lmymb.
func (l *Loop) noteStartFailure(service, fingerprint, reason string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failed == nil {
		l.failed = map[string]startFailure{}
	}
	was := l.failed[service]
	l.failed[service] = startFailure{fingerprint: fingerprint, reason: reason}
	return was.reason != reason
}

// forgetStartFailure drops the memory once the service starts here.
func (l *Loop) forgetStartFailure(service string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failed, service)
}

// reportStartFailure hands the lease back AND says why, in one request. The
// pair is the point: a release with no reason is exactly the state the control
// plane was in while the conductor lease flapped 170 times.
func (l *Loop) reportStartFailure(ctx context.Context, service, reason string) {
	if err := l.Control.ReportStartFailure(ctx, l.PIN, controlplane.LeaseRequest{
		Service: service, MemberID: l.MemberID, Name: l.Name, Machine: l.Machine,
		StartFailReason: reason,
	}); err != nil {
		log.Debugf("reporting the %s start failure: %v", service, err)
	}
}

// oneLine flattens a command failure into something a dashboard cell can hold:
// what we were doing, then the sentence that says why it did not work.
//
// Cutting the string at a length bound is not enough and the incident proves
// it — a failed clone is "mirror canonical from <url>: git clone --mirror <url>
// <path>: Cloning into '<path>'… warning: templates not found… fatal: unable to
// access <url>: Failed to connect", and the only words a human needs are the
// last twelve. So the `fatal:`/`error:` line is carried explicitly and the
// context is what gets trimmed.
func oneLine(err error) string {
	lines := strings.Split(err.Error(), "\n")
	head := flattenLine(lines[0])
	why := ""
	for _, l := range lines[1:] {
		if t := flattenLine(l); strings.HasPrefix(t, "fatal:") || strings.HasPrefix(t, "error:") {
			why = t
			break
		}
	}
	if why == "" {
		return clampLine(head, controlplane.StartFailReasonMax)
	}
	why = clampLine(why, controlplane.StartFailReasonMax)
	room := controlplane.StartFailReasonMax - len(why) - len(" — ")
	if room <= 0 {
		return why
	}
	return clampLine(head, room) + " — " + why
}

func flattenLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func clampLine(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// On a rune boundary: a byte-sliced multi-byte path would make the whole
	// report a postgres error about invalid UTF-8, which is a worse outcome
	// than a slightly shorter sentence.
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max] + "…"
}

// claimReason names what freed the service, so the log line explains a takeover
// rather than just announcing one.
func (l *Loop) claimReason(sess controlplane.Session, service string, now time.Time) string {
	if prev, ok := sess.Leases[service]; ok && prev.Owner != "" && !prev.Live(now) {
		owner := prev.OwnerName
		if owner == "" {
			owner = prev.Owner
		}
		return "lease expired — " + owner + " went away"
	}
	return "unheld"
}

// take claims the lease first, then starts the service. In that order: holding
// the lease is what makes this member the one allowed to serve, and starting a
// second git server for a session that already has one is the failure mode
// worth avoiding. A Start that fails releases the lease again, so the next-best
// member gets its turn instead of the service being held by nobody serving it.
func (l *Loop) take(ctx context.Context, service, fingerprint, why string) error {
	if _, err := l.Control.ClaimLease(ctx, l.PIN, controlplane.LeaseRequest{
		Service: service, MemberID: l.MemberID, Name: l.Name, Machine: l.Machine,
		TTLSeconds: l.ttl(),
	}); err != nil {
		if errors.Is(err, controlplane.ErrLeaseHeld) {
			// Someone else won the race. Entirely normal: that is the mechanism
			// working, not an error worth escalating.
			log.Debugf("%s: %v", service, err)
			return nil
		}
		return err
	}
	if l.Start != nil {
		if err := l.Start(ctx, service); err != nil {
			reason := oneLine(err)
			// Once per distinct reason, not once per tick — and the same
			// sentence goes to the control plane, so the machine that cannot
			// run this service says so somewhere every other member can read.
			if l.noteStartFailure(service, fingerprint, reason) {
				log.Warnf("could not start %s here (%s) — handing the lease back so another member can", service, reason)
				l.changed(service, "cannot start here — "+reason)
			}
			l.reportStartFailure(ctx, service, reason)
			return err
		}
	}
	l.forgetStartFailure(service)
	l.mu.Lock()
	if l.held == nil {
		l.held = map[string]bool{}
	}
	l.held[service] = true
	delete(l.unplaced, service)
	l.mu.Unlock()
	log.Infof("took %s (%s)", service, why)
	l.changed(service, "now served here — "+why)
	return nil
}

// renew extends a lease we hold. Being told we no longer own it is the
// returning-laptop case: stand the service down rather than keep serving
// something the session has moved on from.
func (l *Loop) renew(ctx context.Context, service string) error {
	if l.Outbox != nil {
		// Deposit only — the member cycle's Flush renews. Loss is applied via
		// ApplyLost after Flush returns MemberSyncResult.Lost.
		l.Outbox.Hold(service)
		return nil
	}
	_, err := l.Control.RenewLease(ctx, l.PIN, controlplane.LeaseRequest{
		Service: service, MemberID: l.MemberID, Name: l.Name, Machine: l.Machine,
		TTLSeconds: l.ttl(),
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, controlplane.ErrLeaseHeld):
		log.Infof("lost %s — %v; standing down", service, err)
		l.standDown(ctx, service)
		l.changed(service, "moved to another member")
		return nil
	default:
		// Unreachable or transient: keep serving on the cached lease.
		return err
	}
}

// ApplyLost stands down services the last MemberSync reported as no longer
// owned by this member.
func (l *Loop) ApplyLost(ctx context.Context, lost []string) {
	for _, service := range lost {
		if !l.HoldsService(service) {
			continue
		}
		log.Infof("lost %s — member sync; standing down", service)
		l.standDown(ctx, service)
		l.changed(service, "moved to another member")
	}
}

func (l *Loop) standDown(ctx context.Context, service string) {
	if l.Stop != nil {
		if err := l.Stop(ctx, service); err != nil {
			log.Warnf("stopping %s: %v", service, err)
		}
	}
	l.mu.Lock()
	delete(l.held, service)
	l.mu.Unlock()
}

func (l *Loop) noteUnplaced(sess controlplane.Session, service string, now time.Time) {
	if lease, ok := sess.Leases[service]; ok && lease.Live(now) {
		l.mu.Lock()
		delete(l.unplaced, service)
		l.mu.Unlock()
		return
	}
	r := Rank(sess, service)
	if _, ok := r.Best(); ok {
		return // someone else is about to take it
	}
	l.mu.Lock()
	if l.unplaced == nil {
		l.unplaced = map[string]string{}
	}
	was := l.unplaced[service]
	l.unplaced[service] = r.Reason
	l.mu.Unlock()
	if was != r.Reason {
		log.Infof("%s unplaced: %s", service, r.Reason)
		l.changed(service, "unplaced — "+r.Reason)
	}
}

// ReleaseAll hands back everything this member holds — the graceful "I am
// leaving" path, so the next owner takes over without waiting out a full TTL.
func (l *Loop) ReleaseAll(ctx context.Context) {
	l.mu.Lock()
	services := make([]string, 0, len(l.held))
	for s, ok := range l.held {
		if ok {
			services = append(services, s)
		}
	}
	l.mu.Unlock()
	for _, s := range services {
		if err := l.Control.ReleaseLease(ctx, l.PIN, s); err != nil {
			log.Debugf("release %s: %v", s, err)
		}
		l.mu.Lock()
		delete(l.held, s)
		l.mu.Unlock()
	}
}

func (l *Loop) changed(service, detail string) {
	if l.OnChange != nil {
		l.OnChange(service, detail)
	}
}

func (l *Loop) ttl() int {
	if l.TTLSeconds > 0 {
		return l.TTLSeconds
	}
	return controlplane.DefaultLeaseTTL
}

// Adopt records that this member is already serving a service — the host at
// start-up, which took the PIN and is by definition the one serving canonical.
// It claims the lease and marks the service held without calling Start, since
// the service is already up.
func (l *Loop) Adopt(ctx context.Context, service string) error {
	if l.Control == nil || l.MemberID == "" {
		return nil
	}
	if _, err := l.Control.ClaimLease(ctx, l.PIN, controlplane.LeaseRequest{
		Service: service, MemberID: l.MemberID, Name: l.Name, Machine: l.Machine,
		TTLSeconds: l.ttl(),
	}); err != nil {
		return err
	}
	l.mu.Lock()
	if l.held == nil {
		l.held = map[string]bool{}
	}
	l.held[service] = true
	l.mu.Unlock()
	return nil
}
