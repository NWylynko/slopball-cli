package conductor

import (
	"context"
	"fmt"
	"strings"

	"github.com/nwylynko/slopball-cli/canonical"
	"github.com/nwylynko/slopball-cli/controlplane"
	sbGit "github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/harness"
	"github.com/nwylynko/slopball-cli/logx"
)

// Role names as the session publishes them (firstrun.Plan.Agents).
const (
	RoleMerger       = "merger"
	RoleSetup        = "setup"
	RoleErrorWatcher = "error-watcher"
)

// SessionFleetSpec is everything needed to build the session's fleet from what
// the session itself published.
type SessionFleetSpec struct {
	Host    *canonical.Host
	ID      sbGit.Identity
	Control *controlplane.Client
	PIN     string

	// Fallback drives any role the session never named — this machine's own
	// harness. Nil is legal: that role then runs mechanically.
	Fallback *harness.Client
}

// SessionFleet is the built fleet plus what it ended up running, so callers can
// log "conducting with cursor" without re-deriving it.
type SessionFleet struct {
	Fleet *Fleet

	// Harnesses maps role name to the harness driving it. A role present with an
	// empty value runs mechanically; a role absent was not built at all.
	Harnesses map[string]string

	// LogsURL is the dev-server log stream the error-watcher reads, empty when
	// the session has none (and no watcher was built).
	LogsURL string

	// Logs is the held follower behind that URL, so a caller standing the fleet
	// down can close it rather than leaking a connection per re-election.
	Logs *RemoteLogSource

	// DevURL is the announced dev endpoint the error-watcher health-probes.
	// Empty disables that trigger; the log trigger is unaffected.
	DevURL string

	// States is the board every built role reports into, and Publisher is what
	// carries it to the control plane (plan 36 §2). The caller starts the
	// publisher beside its tick loop — not inside a tick, because the tick that
	// blocks for a 40-second AI resolve is exactly the one whose state has to
	// reach other people.
	States     *StateBoard
	Publisher  *StatePublisher
	Mechanical map[string]bool // role → runs without the session's chosen harness
}

// BuildSessionFleet assembles merger + setup + error-watcher from the fleet
// composition the session published (plan 29), falling back to this machine's
// harness only for roles the session never named.
//
// It exists because there are two places a conductor starts — the `slopball
// conductor` verb and the join daemon that takes the conductor lease over — and
// they MUST agree. They did not: the daemon hardcoded a lookup that defaults to
// claude, so a session that elected cursor got a second fleet scaffolding the
// same brief with the wrong agent, and lost the error-watcher on failover.
func BuildSessionFleet(ctx context.Context, spec SessionFleetSpec) SessionFleet {
	log := logx.New("conductor")
	agents := SessionRoleAgents(ctx, spec.Control, spec.PIN)
	// Every role's CLI runs in the session work tree. Left unset it would
	// inherit the slopball process's cwd — an unrelated directory that a
	// trust-gating CLI will sit and prompt about forever.
	brainFor := func(role string) *harness.Client {
		a, ok := agents[role]
		if !ok || a.Harness == "" {
			return withDir(spec.Fallback, spec.Host.Work)
		}
		if !harness.IsAvailable(a.Harness) {
			// Deliberately not a downgrade to this machine's own agent: a session
			// that chose cursor must not be silently conducted by claude.
			log.Warnf("the session runs %s with %s, which is not installed here — that role runs mechanically", role, a.Harness)
			return nil
		}
		return withDir(harness.Lookup(a.Harness, a.Model), spec.Host.Work)
	}

	built := SessionFleet{Harnesses: map[string]string{}, States: &StateBoard{}, Mechanical: map[string]bool{}}
	mechanical := func(role string, brain *harness.Client) {
		a, ok := agents[role]
		built.Mechanical[role] = ok && a.Harness != "" && brain == nil
	}
	mergerBrain := brainFor(RoleMerger)
	mechanical(RoleMerger, mergerBrain)
	merger := &Merger{
		Host: spec.Host, ID: spec.ID, Resolve: HarnessResolver(mergerBrain),
		Harness: HarnessNameOf(mergerBrain), Control: spec.Control, PIN: spec.PIN,
		States: built.States,
	}
	built.Harnesses[RoleMerger] = merger.Harness

	// The setup role runs wherever the conductor runs: for a cloud box that is
	// the elected laptop, where the harness login lives, and the scaffold lands
	// on the box's canonical (plan 28 §4).
	setupBrain := brainFor(RoleSetup)
	mechanical(RoleSetup, setupBrain)
	setup := &Setup{
		Host: spec.Host, ID: spec.ID, Agent: HarnessScaffolder(setupBrain),
		Harness: HarnessNameOf(setupBrain), Control: spec.Control, PIN: spec.PIN,
		States: built.States,
	}
	built.Harnesses[RoleSetup] = setup.Harness

	roles := []Role{merger, setup}
	if built.LogsURL = SessionLogsURL(ctx, spec.Control, spec.PIN, spec.Host); built.LogsURL != "" {
		watchBrain := brainFor(RoleErrorWatcher)
		mechanical(RoleErrorWatcher, watchBrain)
		// The dev endpoint is the health probe's target. It is announced from the
		// repo's committed PORT, so a session that has not scaffolded one yet
		// simply runs without the second trigger.
		built.DevURL = SessionDevURL(ctx, spec.Control, spec.PIN, spec.Host)
		// Resolved per connection, not captured: dev is a placed service and it
		// moves (plan 30), so a follower that re-dialled the address it first
		// saw would follow a member that no longer holds the lease.
		logs := &RemoteLogSource{
			URL: built.LogsURL,
			Resolve: func(ctx context.Context) (string, error) {
				if u := SessionLogsURL(ctx, spec.Control, spec.PIN, spec.Host); u != "" {
					return u, nil
				}
				return "", fmt.Errorf("session %s has no logs endpoint", spec.PIN)
			},
		}
		built.Logs = logs
		watcher := &ErrorWatcher{
			Host: spec.Host, Logs: logs,
			ID: spec.ID, Fix: HarnessFixer(watchBrain), Harness: HarnessNameOf(watchBrain),
			Control: spec.Control, PIN: spec.PIN,
			Health: HTTPHealth(built.DevURL),
			States: built.States,
		}
		roles = append(roles, watcher)
		built.Harnesses[RoleErrorWatcher] = watcher.Harness
	}
	built.Fleet = &Fleet{Roles: roles}
	// Every built role starts idle, so a dashboard shows the fleet the moment
	// it exists rather than only once something happens to it.
	for role := range built.Harnesses {
		built.States.Idle(role)
	}
	built.Publisher = &StatePublisher{
		Control: spec.Control, PIN: spec.PIN, Board: built.States,
		Record:     SessionRecord(ctx, spec.Control, spec.PIN, agents),
		Mechanical: built.Mechanical,
	}
	return built
}

// SessionRecord is the election a fleet runs under, re-asserted on every state
// publish. Only the elector runs a fleet, so reading it once is enough: a
// re-election builds a new fleet, and a new publisher with it.
//
// Exported because the wizard's synchronous scaffold (`scaffoldNow`) runs a
// publisher of its own before any fleet exists, and it has to assert the same
// record — PutConductor writes the whole row, so a publisher that invented one
// would blank the election it is publishing under.
func SessionRecord(ctx context.Context, client *controlplane.Client, pin string, agents map[string]controlplane.RoleAgent) controlplane.ConductorRecord {
	rec := controlplane.ConductorRecord{Active: true, Roles: agents}
	if client == nil || pin == "" {
		return rec
	}
	if sess, err := client.Session(ctx, pin); err == nil && sess.Conductor != nil {
		rec = *sess.Conductor
		if len(rec.Roles) == 0 {
			rec.Roles = agents
		}
	}
	return rec
}

// HasIntelligence reports whether any role got a harness. False means the fleet
// can only resolve merges mechanically — worth saying out loud, and worth
// handing a conductor lease back for.
func (s SessionFleet) HasIntelligence() bool {
	for _, h := range s.Harnesses {
		if h != "" {
			return true
		}
	}
	return false
}

// Summary is the one-line "who is running what" for the conductor's log —
// which agent drives each role, and which roles are not running at all.
func (s SessionFleet) Summary() string {
	var b strings.Builder
	for _, role := range []string{RoleMerger, RoleSetup, RoleErrorWatcher} {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString(role + "=")
		switch h, built := s.Harnesses[role]; {
		case !built:
			b.WriteString("off")
		case h == "":
			b.WriteString("mechanical")
		default:
			b.WriteString(h)
		}
	}
	return b.String()
}

// SessionRoleAgents reads the session's per-role fleet composition from the
// control plane. This is what lets a machine that elects itself conductor
// mid-session — or any second laptop — drive merger / error-watcher / setup
// with the agents the session chose, without being told (plan 29). A session
// that predates the record, or one nobody published, yields nil and the caller
// falls back to its own harness.
func SessionRoleAgents(ctx context.Context, client *controlplane.Client, pin string) map[string]controlplane.RoleAgent {
	if client == nil {
		return nil
	}
	sess, err := client.Session(ctx, pin)
	if err != nil || sess.Conductor == nil || len(sess.Conductor.Roles) == 0 {
		return nil
	}
	return sess.Conductor.Roles
}

// SessionLogsURL is where the dev server's logs are readable: the session's own
// endpoint when it has one, else derived from the git URL for older boxes.
func SessionLogsURL(ctx context.Context, client *controlplane.Client, pin string, host *canonical.Host) string {
	// Through EndpointURL, never the raw record: on the session network the
	// published address is `slop://<pin>/git/logs`, and handing that to
	// http.Get is what left the error-watcher silently blind for a whole
	// session while logging that it was watching (plan 40).
	if client != nil {
		if url, err := client.EndpointURL(ctx, pin, controlplane.EndpointLogs); err == nil && url != "" {
			return url
		}
	}
	if host == nil {
		return ""
	}
	return LogsURLFromRemote(host.RemoteURL())
}

// HarnessNameOf is the log-facing name of a possibly-nil client.
func HarnessNameOf(c *harness.Client) string {
	if c == nil {
		return ""
	}
	return string(c.Name)
}
