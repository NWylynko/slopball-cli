// Package conductor is the host-side fleet of parallel role-agents (plans/06–07, 20).
// Clean merges pass through with no AI; roles spend tokens only on real work.
// After-roles (runtime reconciler) run sequentially once main has advanced.
package conductor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nwylynko/slopball-cli/canonical"
	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/durability"
	sbGit "github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/logx"
	"github.com/nwylynko/slopball-cli/syncengine"
)

// mergerLog surfaces what the merger is doing — especially when a conflict hands
// off to the AI harness — visible by default (not behind SLOPBALL_LOG=debug) so an
// operator watching the host or conductor terminal sees the agent working.
var mergerLog = logx.New("merger")

// Role is one specialized agent in the fleet.
type Role interface {
	Name() string
	Tick(ctx context.Context) error
}

// Fleet runs roles concurrently, then optional After roles sequentially.
// After is for post-merge work that must see the advanced main (plan 20 runtime).
// Callers that SyncWorkToMain between merge and runtime should use TickRoles
// then TickAfter around the sync, not TickAll.
type Fleet struct {
	Roles []Role
	After []Role
}

// TickRoles runs Roles in parallel (merger, error-watcher, …).
func (f *Fleet) TickRoles(ctx context.Context) error {
	var wg sync.WaitGroup
	errCh := make(chan error, len(f.Roles))
	for _, r := range f.Roles {
		wg.Add(1)
		go func(r Role) {
			defer wg.Done()
			if err := r.Tick(ctx); err != nil {
				errCh <- fmt.Errorf("%s: %w", r.Name(), err)
			}
		}(r)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		return err
	}
	return nil
}

// TickAfter runs After roles in order (runtime reconciler, …).
func (f *Fleet) TickAfter(ctx context.Context) error {
	for _, r := range f.After {
		if err := r.Tick(ctx); err != nil {
			return fmt.Errorf("%s: %w", r.Name(), err)
		}
	}
	return nil
}

// TickAll runs TickRoles then TickAfter. Prefer the split form when work-tree
// sync must happen between them.
func (f *Fleet) TickAll(ctx context.Context) error {
	if err := f.TickRoles(ctx); err != nil {
		return err
	}
	return f.TickAfter(ctx)
}

// ConflictResolver is invoked only when git auto-merge fails. It is given the
// merge work tree (where the conflicted files sit with markers), the pushing
// branch, its intent note, and the conflicted paths; it returns the resolved
// file contents keyed by path. A nil resolver falls back to incoming-change
// bias (`checkout --theirs`).
type ConflictResolver func(ctx context.Context, workDir, branch, intent string, conflicted []string) (map[string]string, error)

// Merger integrates branches ahead of main into main (plans/06).
type Merger struct {
	Host         *canonical.Host
	ID           sbGit.Identity
	Resolve      ConflictResolver // optional; nil = mechanical --theirs
	Harness      string           // harness name, for logs ("claude"/…); "" = generic
	Mirror       *durability.Mirror
	HarnessCalls atomic.Int64 // increments when Resolve is invoked
	// Control + PIN: optional plan-24 convergence publisher (fire-and-forget).
	Control *controlplane.Client
	PIN     string
	// States is the dashboard's view of this role (plan 36 §2). Nil is legal
	// and publishes nothing.
	States *StateBoard
}

func (m *Merger) harnessName() string {
	if m.Harness == "" {
		return "harness"
	}
	return m.Harness
}

func (m *Merger) Name() string { return "merger" }

// Tick picks up one ahead-of-main branch and merges it, then sweeps main for
// leftover conflict markers. The sweep matters because a client that re-runs
// push mid-conflict finalizes a merge commit with markers still in the files;
// that branch then merges cleanly into main (main is already an ancestor), so no
// git-level conflict is ever raised and the broken markers ship. The sweep is
// the safety net that catches and heals that case (and runs even when nothing is
// ahead of main, e.g. the markers already landed before the conductor started).
func (m *Merger) Tick(ctx context.Context) error {
	ahead, err := m.Host.BranchesAheadOfMain(ctx)
	if err != nil {
		return err
	}
	if len(ahead) > 0 {
		m.States.Working(RoleMerger, "merging "+ahead[0])
		err := m.mergeBranch(ctx, ahead[0])
		m.States.Idle(RoleMerger)
		if err != nil {
			return err
		}
	}
	if err := m.sweepMainMarkers(ctx); err != nil {
		return err
	}
	m.publishConvergence(ctx)
	return nil
}

func (m *Merger) mergeBranch(ctx context.Context, branch string) error {
	// Work in the canonical work tree, which tracks main.
	c := &sbGit.Cmd{Dir: m.Host.Work, Env: m.ID.EnvVars()}
	if err := c.Run(ctx, "fetch", "origin"); err != nil {
		return err
	}
	if err := c.Run(ctx, "checkout", "main"); err != nil {
		return err
	}
	if err := c.Run(ctx, "reset", "--hard", "origin/main"); err != nil {
		return err
	}

	intent := m.readIntent(ctx, branch)
	err := c.Run(ctx, "merge", "--no-edit", "origin/"+branch)
	if err == nil {
		if err := c.Run(ctx, "push", "origin", "main"); err != nil {
			return err
		}
		mergerLog.Infof("merged %s into main (clean, no AI needed)", branch)
		m.publishApplied(ctx, branch, intent, 0)
		m.afterMainAdvanced()
		return nil
	}

	// Conflict path — invoke resolver or fall back to --theirs (incoming bias).
	conflicted := listConflicted(ctx, m.Host.Work)
	if m.Resolve != nil {
		mergerLog.Infof("conflict merging %s — %s resolving %d file(s): %s",
			branch, m.harnessName(), len(conflicted), strings.Join(conflicted, ", "))
		// The slow half of a merge, and the reason the dot exists: this is the
		// one every other member's dashboard should see happening.
		m.States.Working(RoleMerger, fmt.Sprintf("%s resolving %d conflict(s) on %s",
			m.harnessName(), len(conflicted), branch))
		m.HarnessCalls.Add(1)
		files, rerr := m.Resolve(ctx, m.Host.Work, branch, intent, conflicted)
		if rerr != nil {
			mergerLog.Warnf("%s could not resolve %s: %v — aborting this merge, will retry next tick",
				m.harnessName(), branch, rerr)
			_ = c.Run(ctx, "merge", "--abort")
			return rerr
		}
		for path, body := range files {
			if werr := writeFile(m.Host.Work, path, body); werr != nil {
				_ = c.Run(ctx, "merge", "--abort")
				return werr
			}
			_ = c.Run(ctx, "add", path)
		}
		mergerLog.Infof("%s resolved %d file(s) on %s — committing to main", m.harnessName(), len(files), branch)
	} else {
		mergerLog.Infof("conflict merging %s — no harness available, taking the incoming side for %d file(s): %s",
			branch, len(conflicted), strings.Join(conflicted, ", "))
		for _, path := range conflicted {
			_ = c.Run(ctx, "checkout", "--theirs", path)
			_ = c.Run(ctx, "add", path)
		}
	}
	msg := fmt.Sprintf("merge %s into main", branch)
	if intent != "" {
		msg += "\n\nSlopball-Intent: " + intent
	}
	if err := c.Run(ctx, "commit", "-m", msg); err != nil {
		_ = c.Run(ctx, "merge", "--abort")
		return err
	}
	if err := c.Run(ctx, "push", "origin", "main"); err != nil {
		return err
	}
	m.publishApplied(ctx, branch, intent, len(conflicted))
	m.afterMainAdvanced()
	return nil
}

// sweepMainMarkers heals conflict markers that were committed into main without
// ever raising a git-level conflict (see Tick). It resets the work tree to
// origin/main, hands any marker-carrying files to the resolver (or strips them
// mechanically, incoming-biased, when no harness is present), verifies the
// result is clean, and pushes. No markers → no-op, so it is cheap to run every
// tick.
func (m *Merger) sweepMainMarkers(ctx context.Context) error {
	c := &sbGit.Cmd{Dir: m.Host.Work, Env: m.ID.EnvVars()}
	if err := c.Run(ctx, "fetch", "origin"); err != nil {
		return err
	}
	if err := c.Run(ctx, "checkout", "main"); err != nil {
		return err
	}
	if err := c.Run(ctx, "reset", "--hard", "origin/main"); err != nil {
		return err
	}
	marked := conflictMarkerFiles(ctx, m.Host.Work)
	if len(marked) == 0 {
		return nil
	}

	if m.Resolve != nil {
		mergerLog.Infof("main carries leftover conflict markers in %d file(s): %s — %s healing",
			len(marked), strings.Join(marked, ", "), m.harnessName())
		m.States.Working(RoleMerger, fmt.Sprintf("%s healing conflict markers on main (%d file(s))",
			m.harnessName(), len(marked)))
		defer m.States.Idle(RoleMerger)
		m.HarnessCalls.Add(1)
		files, rerr := m.Resolve(ctx, m.Host.Work, "main (committed conflict markers)", "", marked)
		if rerr != nil {
			mergerLog.Warnf("%s could not heal markers: %v — will retry next tick", m.harnessName(), rerr)
			return rerr
		}
		for path, body := range files {
			if werr := writeFile(m.Host.Work, path, body); werr != nil {
				return werr
			}
			_ = c.Run(ctx, "add", path)
		}
	} else {
		mergerLog.Infof("main carries leftover conflict markers in %d file(s): %s — no harness, taking the incoming side",
			len(marked), strings.Join(marked, ", "))
		for _, path := range marked {
			body, err := readWorkFile(m.Host.Work, path)
			if err != nil {
				return err
			}
			if werr := writeFile(m.Host.Work, path, stripConflictMarkers(body)); werr != nil {
				return werr
			}
			_ = c.Run(ctx, "add", path)
		}
	}

	// Never publish a "heal" that still has markers — bail and retry next tick.
	if still := conflictMarkerFiles(ctx, m.Host.Work); len(still) > 0 {
		_ = c.Run(ctx, "reset", "--hard", "origin/main")
		return fmt.Errorf("markers remain after heal in %s; not pushing", strings.Join(still, ", "))
	}
	if err := c.Run(ctx, "commit", "-am", "conductor: heal leftover conflict markers on main"); err != nil {
		return err
	}
	if err := c.Run(ctx, "push", "origin", "main"); err != nil {
		return err
	}
	mergerLog.Infof("healed conflict markers on main (%d file(s))", len(marked))
	m.afterMainAdvanced()
	return nil
}

// publishApplied announces work landing on main. readIntent already had the
// intent in hand, so the feed's "what was this for?" costs one control-plane
// write and no extra git.
func (m *Merger) publishApplied(ctx context.Context, branch, intent string, conflicts int) {
	if m.Control == nil || m.PIN == "" {
		return
	}
	sha, _ := sbGit.Output(ctx, m.Host.Bare, "rev-parse", "refs/heads/main")
	m.Control.PublishEventBestEffort(ctx, m.PIN, controlplane.EventMergeApplied, map[string]any{
		"branch": branch, "intent": intent, "sha": strings.TrimSpace(sha), "conflicts": conflicts,
	})
}

func (m *Merger) afterMainAdvanced() {
	ctx := context.Background()
	if m.Mirror != nil {
		m.Mirror.Trigger(ctx)
	}
	m.publishConvergence(ctx)
	// main actually moved, which is the one frame every client mirror and the
	// remote conductor react to. Deposit-and-wait would put up to a MemberCycle
	// between the merge and everyone hearing about it, so this rare, real change
	// goes out now — the plan trades per-tick traffic away, not per-merge latency.
	if out := m.Control.OutboxFor(m.PIN); out != nil {
		if err := out.FlushChange(ctx); err != nil {
			mergerLog.Debugf("could not publish main.advanced immediately: %v", err)
		}
	}
}

func (m *Merger) publishConvergence(ctx context.Context) {
	if m.Control == nil || m.PIN == "" || m.Host == nil {
		return
	}
	mainSHA, _ := sbGit.Output(ctx, m.Host.Bare, "rev-parse", "refs/heads/main")
	ahead, _ := m.Host.BranchesAheadOfMain(ctx)
	ahead, omitted := controlplane.TruncateBranchesAhead(ahead)
	sha := strings.TrimSpace(mainSHA)
	m.publishConvergenceRecord(ctx, controlplane.Convergence{
		MainSHA: sha, BranchesAhead: ahead, BranchesAheadOmitted: omitted,
	})
	// The per-tick density lives here, not in the event stream (plan 40):
	// `main.advanced` is now a transition, so this is what tells an operator
	// the merger is alive and what it is looking at.
	mergerLog.Infof("tick — %d branch(es) ahead, main %s", len(ahead), shortSHA(sha))
}

// publishConvergenceRecord sends one sample. Sampling stays on the fleet's own
// 2s tick — that is the merge hot path — but the *send* rides this process's
// member cycle when it has one (plan 43). Only a process with no membership at
// all, which is `slopball conductor` run standalone, publishes directly.
func (m *Merger) publishConvergenceRecord(ctx context.Context, c controlplane.Convergence) {
	if out := m.Control.OutboxFor(m.PIN); out != nil {
		out.SetConvergence(c)
		return
	}
	m.Control.PutConvergenceBestEffort(ctx, m.PIN, c)
}

func (m *Merger) readIntent(ctx context.Context, branch string) string {
	out, err := sbGit.Output(ctx, m.Host.Bare, "log", "-1", "--format=%B", branch)
	if err != nil {
		return ""
	}
	return syncengine.ReadIntent(out)
}

func listConflicted(ctx context.Context, work string) []string {
	out, err := sbGit.Output(ctx, work, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths
}

func writeFile(root, rel, body string) error {
	return writeFileOS(root, rel, body)
}

// shortSHA is the human-length form used in tick lines.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	if sha == "" {
		return "(none)"
	}
	return sha
}
