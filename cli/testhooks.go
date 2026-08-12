package cli

import (
	"context"
	"sync"

	"github.com/nwylynko/slopball-cli/box"
	"github.com/nwylynko/slopball-cli/conductor"
	"github.com/spf13/cobra"
)

// TestHooks is the whole of this package's test seam, deliberately one door
// rather than a scatter of package-level variables.
//
// It is exported because the tests that use it live in a different module
// (plan 49: the client is public, every test stays in the monorepo), and an
// external test package can only reach exported identifiers. Three things a
// session does are impossible to drive from a test — a billed model call, a
// docker/ssh host, and a full-screen TUI — and these are their stand-ins.
// Nothing in production ever sets one; a nil field means "do the real thing".
//
// Adding a field here is how you resist exporting another piece of the CLI's
// insides. Prefer driving the real command tree (NewRootCmd / Run) when the
// behaviour under test is observable from outside at all.
type TestHooks struct {
	// SetupAgent replaces the setup role's harness client, so the first-run
	// flow can be driven end to end without a real (billed) model call.
	SetupAgent conductor.Scaffolder
	// BoxRunner replaces the transport `box add` and the wizard provision over
	// — docker on this machine or ssh to another — with a fake that answers the
	// commands Provision issues.
	BoxRunner func(*cobra.Command, []string) (box.Runner, error)
	// ConsoleEntered fires the moment the console takes the screen, before the
	// work behind it starts. It is how the ordering tests observe "the dashboard
	// came up first" without a pty, since the no-terminal fallback reaches the
	// same point and then calls Work.
	ConsoleEntered func(ConsoleUp)
}

// ConsoleUp is what a test learns when the console takes the screen: the
// session's name (empty on a fresh create — the control plane mints it one
// round trip later) and the quit the screen offers, which is the same act every
// daemon's Close performs.
//
// It is deliberately narrower than the console's own session struct, which is
// full of plumbing — the announcer, the work goroutine — that no test should be
// able to reach into from another module.
type ConsoleUp struct {
	PIN   string
	Leave func(context.Context) error
}

var (
	hooksMu sync.RWMutex
	hooks   TestHooks
)

// SetTestHooks installs every non-nil field of h, leaving the rest as they
// were, and returns a func that puts the whole previous set back. The merge is
// what lets one test layer a fake box onto a fake setup agent without either
// helper knowing about the other; the restore is `t.Cleanup(cli.SetTestHooks(…))`.
func SetTestHooks(h TestHooks) (restore func()) {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	prev := hooks
	if h.SetupAgent != nil {
		hooks.SetupAgent = h.SetupAgent
	}
	if h.BoxRunner != nil {
		hooks.BoxRunner = h.BoxRunner
	}
	if h.ConsoleEntered != nil {
		hooks.ConsoleEntered = h.ConsoleEntered
	}
	return func() {
		hooksMu.Lock()
		defer hooksMu.Unlock()
		hooks = prev
	}
}

// testHooks reads the installed set. Every production caller goes through this
// and treats a nil field as "no hook", so an unhooked binary behaves as if the
// seam were not there.
func testHooks() TestHooks {
	hooksMu.RLock()
	defer hooksMu.RUnlock()
	return hooks
}
