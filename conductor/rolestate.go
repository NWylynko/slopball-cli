package conductor

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/logx"
)

// StateBoard is what the fleet's roles report into and the publisher reads out:
// idle/working plus one line of what is happening, per role (plan 36 §2).
//
// Every method is nil-safe, so a role built without a board — the emulator's
// bare fleet, a `--once` tick — keeps working and simply publishes nothing.
type StateBoard struct {
	mu     sync.Mutex
	states map[string]controlplane.RoleAgent
}

// Working marks a role as doing slow work, with the line the dashboard shows.
//
// Since measures the *work*, not the latest thing said about it: a role that is
// already working keeps its original Since when the activity changes. A running
// agent reports every step it takes, so restarting the clock per step would
// leave the dashboard reading "(3s)" forever — a role that keeps restarting,
// which is a worse lie than a frozen line. Only a role that was idle starts a
// new clock.
func (b *StateBoard) Working(role, activity string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	prev := b.get(role)
	if prev.State == controlplane.RoleWorking && prev.Activity == activity {
		return
	}
	since := time.Now().UTC()
	if prev.State == controlplane.RoleWorking && !prev.Since.IsZero() {
		since = prev.Since
	}
	b.set(role, controlplane.RoleAgent{
		State: controlplane.RoleWorking, Activity: activity, Since: since,
	})
}

// Idle marks a role as done. The activity line goes with it: a dashboard
// showing what a role *was* doing while it is idle reads as a stuck session.
func (b *StateBoard) Idle(role string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.get(role).State == controlplane.RoleIdle {
		return
	}
	b.set(role, controlplane.RoleAgent{State: controlplane.RoleIdle, Since: time.Now().UTC()})
}

// Snapshot is every role's current state, stamped with now as UpdatedAt — the
// freshness the reader ages out against.
func (b *StateBoard) Snapshot() map[string]controlplane.RoleAgent {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now().UTC()
	out := make(map[string]controlplane.RoleAgent, len(b.states))
	for role, st := range b.states {
		st.UpdatedAt = now
		out[role] = st
	}
	return out
}

func (b *StateBoard) get(role string) controlplane.RoleAgent { return b.states[role] }

func (b *StateBoard) set(role string, st controlplane.RoleAgent) {
	if b.states == nil {
		b.states = map[string]controlplane.RoleAgent{}
	}
	b.states[role] = st
}

// StatePublisher pushes the board into the control plane so every member's
// dashboard — not just the elector's — can see the fleet working.
//
// It exists because PutConductor's three call sites are all one-shot election
// paths: there was no conductor heartbeat to ride. It rides beside the fleet
// loop rather than inside a tick, because a tick that blocks for a 40-second AI
// resolve is exactly the tick whose state has to reach other people.
type StatePublisher struct {
	Control *controlplane.Client
	PIN     string
	Board   *StateBoard

	// Record is the election this fleet is running under, re-asserted on every
	// publish because PutConductor writes the whole record. Only the elector
	// runs a fleet, so it is the record's rightful author; a re-election builds
	// a new fleet and a new publisher with it.
	Record controlplane.ConductorRecord
	// Mechanical marks roles the session named a harness for but this machine
	// runs without it — published so every member's dashboard is honest.
	Mechanical map[string]bool
}

// Publish writes one sample. Fire-and-forget: a dashboard that misses a frame
// catches up on the next one, and nothing about the merge path depends on it.
func (p *StatePublisher) Publish(ctx context.Context) {
	if p == nil || p.Control == nil || p.PIN == "" {
		return
	}
	states := p.Board.Snapshot()
	if len(states) == 0 {
		return
	}
	rec := p.Record
	roles := make(map[string]controlplane.RoleAgent, len(rec.Roles)+len(states))
	for role, a := range rec.Roles {
		roles[role] = a
	}
	for role, st := range states {
		a := roles[role] // keep the harness/model the session chose
		a.State, a.Activity, a.Since, a.UpdatedAt = st.State, st.Activity, st.Since, st.UpdatedAt
		if p.Mechanical != nil {
			a.Mechanical = p.Mechanical[role]
		}
		roles[role] = a
	}
	rec.Roles = roles
	// Sampling stays at StatePublishInterval — the tick that blocks on a
	// 40-second resolve is exactly the one whose state has to reach other
	// people — but the send rides this process's member cycle (plan 43). On a
	// session with a fleet this used to be a second 2s writer on top of the
	// merger's.
	if out := p.Control.OutboxFor(p.PIN); out != nil {
		out.SetConductor(rec)
	} else if err := p.Control.PutConductor(ctx, p.PIN, rec); err != nil {
		statePubLog.Debugf("state publish failed: %v", err)
		return
	}
	// Plan 40: this loop used to append a `conductor.elected` event every 2s.
	// The volume was real information wearing the wrong label — it belongs in
	// the log stream, which is unbounded and free.
	statePubLog.Infof("state published — %s", describeStates(states))
}

// StatePublishInterval is the sampling cadence. It matches the fleet's own tick
// and the member heartbeat, and is well inside controlplane.RoleStaleAfter so a
// single missed publish never blanks a dashboard.
const StatePublishInterval = 2 * time.Second

// Run publishes every StatePublishInterval until ctx is done, then publishes a
// final idle-everything sample so a clean shutdown does not leave the last
// working state to age out on everyone else's screen.
func (p *StatePublisher) Run(ctx context.Context) {
	t := time.NewTicker(StatePublishInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			for role := range p.Board.Snapshot() {
				p.Board.Idle(role)
			}
			p.Publish(context.WithoutCancel(ctx))
			return
		case <-t.C:
			p.Publish(ctx)
		}
	}
}

var statePubLog = logx.New("conductor")

// describeStates renders one sample as "merger idle, setup working — scaffolding".
func describeStates(states map[string]controlplane.RoleAgent) string {
	roles := make([]string, 0, len(states))
	for role := range states {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	parts := make([]string, 0, len(roles))
	for _, role := range roles {
		line := role + " " + states[role].State
		if a := states[role].Activity; a != "" {
			line += " — " + a
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, ", ")
}
