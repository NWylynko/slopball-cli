package joindaemon

import (
	"context"
	"testing"
	"time"

	"github.com/nwylynko/slopball-cli/conductor"
	"github.com/nwylynko/slopball-cli/logx"
)

// ⚠️ This is the one test that lives in the PUBLIC module, and it is here
// because step 3 proved it could not live anywhere else. It stayed white-box in
// the monorepo through ticket 03 on the argument below; the moment the source
// moved, `package joindaemon` in the monorepo stopped compiling — `undefined:
// Joined` — which is the whole reason tickets 02-05 converted the other 48
// files. A white-box test cannot span a module boundary, so it follows its
// source or it dies.
//
// It is legal here by plan 49's mechanical rule — a public test may import
// nothing outside the public module, and this one imports only conductor and
// logx, which are both in it. It needs no cptest, no database and no
// credential, so public CI can run it.
//
// The original argument for keeping it white-box (plan

// 49 step 2, the spec's second preference, taken as a last resort).
//
// The assertion below is about the context `fleetLoop` builds, and the only way
// to observe that context is a Role that reports it. A role can be installed
// only by startConductor, which builds the fleet itself — so from outside the
// package the probe would have to be written into a live fleet's Roles slice
// while the loop is already ticking it, which is a data race, or a fleet-setter
// would have to be exported purely so a test could call it. Neither is worth it
// for a regression this cheap to keep here.
//
// It costs nothing at the split: this test touches only conductor and logx,
// both of which move into the public module with joindaemon, and it does not
// touch cptest — which is the actual reason the other tests in this package had
// to become external.

// The mirror loop runs on a 30s deadline because a git fetch that hangs must
// not wedge the heartbeat. The conductor fleet was ticking inside that same
// context, so every scaffold longer than 30s died with `signal: killed` — a
// create-next-app run is 30–90s, so it could never finish.
func TestFleetTickIsNotBoundedByTheMirrorLoopsDeadline(t *testing.T) {
	j := &Joined{stop: make(chan struct{}), log: logx.New("join")}
	defer j.Close()

	probe := &deadlineProbe{seen: make(chan time.Duration, 1)}
	j.fleet = &conductor.Fleet{Roles: []conductor.Role{probe}}

	go j.fleetLoop()

	select {
	case budget := <-probe.seen:
		// conductor.Setup gives one scaffold 10 minutes; the tick that carries it
		// must not hand it less.
		if budget < 10*time.Minute {
			t.Fatalf("the fleet ticks with %s of budget — a scaffold is killed mid-run; it must not inherit the mirror loop's deadline", budget)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the fleet never ticked")
	}
}

// deadlineProbe reports how much time the fleet's tick context allows. A
// context with no deadline reports the sentinel, which is what the fleet loop
// should hand a role that may run an agent for minutes.
type deadlineProbe struct{ seen chan time.Duration }

const noDeadline = 365 * 24 * time.Hour

func (p *deadlineProbe) Name() string { return "deadline-probe" }

func (p *deadlineProbe) Tick(ctx context.Context) error {
	budget := noDeadline
	if d, ok := ctx.Deadline(); ok {
		budget = time.Until(d)
	}
	select {
	case p.seen <- budget:
	default:
	}
	return nil
}
