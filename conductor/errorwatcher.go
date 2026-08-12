package conductor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/slopball-cli/canonical"
	"github.com/nwylynko/slopball-cli/controlplane"
	sbGit "github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/logx"
)

var watcherLog = logx.New("watcher")

// Fixer diagnoses a runtime error against the current source tree and returns
// the single file to rewrite and its new contents. A nil Fixer makes the
// watcher fall back to writing a marker file (mechanical, no harness).
type Fixer func(ctx context.Context, errLog string, files map[string]string) (path string, content string, err error)

// ErrorWatcher reads dev-server logs and produces fix commits onto main when
// runtime errors appear (plans/07). Works in a private temp clone so it can
// run concurrent with the merger without fighting over Host.Work. Logs may be
// the in-process buffer (local host) or a RemoteLogSource polling the box's
// /logs endpoint (off-box/elected conductor) — the watcher only needs to read
// the accumulated output.
type ErrorWatcher struct {
	Host    *canonical.Host
	Logs    LogSource
	ID      sbGit.Identity
	Fix     Fixer  // optional; nil = mechanical marker file
	Harness string // harness name, for logs
	// States is the dashboard's view of this role (plan 36 §2). Nil is legal.
	States *StateBoard
	// Control + PIN: optional plan-24 lastError publisher.
	Control *controlplane.Client
	PIN     string

	// Settle is how long the watchable stream must go quiet before a burst is
	// acted on. A broken dev server writes a screenful at once — a stack trace,
	// a failed compile — and every one of those lines used to be its own fix,
	// its own harness call, and its own commit on main. One failure, one fix.
	Settle time.Duration
	// MaxWait fires a burst that never settles. A crash loop logs forever, and
	// waiting for silence that never comes is the same as not watching.
	MaxWait time.Duration
	// Cooldown is the floor between two fixes. The watcher pushes to main and
	// the dev server restarts on that merge; giving the last fix time to land
	// is what stops it fixing the rubble of its own previous attempt.
	Cooldown time.Duration

	// Health is an independent trigger, not a gate on the log path: a dev
	// server can be wedged, or serving 500s from a route that compiled fine,
	// without writing anything at all. nil disables it. HealthStreak is how
	// many consecutive failures it takes — a Reload restarts the process on
	// every merge, so a single blip must not count.
	Health       func(ctx context.Context) error
	HealthStreak int

	// Now is a clock seam for tests.
	Now func() time.Time

	mu sync.Mutex
	// seen is the byte offset already pulled from the source; pending is what
	// has been pulled but not yet acted on, held across ticks so a burst
	// arrives at the fixer whole.
	seen         int
	pending      string
	firstPending time.Time
	lastAppend   time.Time
	lastFix      time.Time
	// fixed fingerprints never buy a second fix: either it worked, or the model
	// cannot fix it, and retrying forever is a commit per tick.
	fixed map[string]bool
	// health outage bookkeeping. healthFixed makes it one fix per outage,
	// cleared when the dev server answers again.
	unhealthy   int
	healthErr   string
	healthFixed bool
	Fixes       int
}

const (
	defaultSettle       = 5 * time.Second
	defaultMaxWait      = 30 * time.Second
	defaultCooldown     = 60 * time.Second
	defaultHealthStreak = 3
)

func (e *ErrorWatcher) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *ErrorWatcher) settle() time.Duration {
	if e.Settle > 0 {
		return e.Settle
	}
	return defaultSettle
}

func (e *ErrorWatcher) maxWait() time.Duration {
	if e.MaxWait > 0 {
		return e.MaxWait
	}
	return defaultMaxWait
}

func (e *ErrorWatcher) cooldown() time.Duration {
	if e.Cooldown > 0 {
		return e.Cooldown
	}
	return defaultCooldown
}

func (e *ErrorWatcher) healthStreak() int {
	if e.HealthStreak > 0 {
		return e.HealthStreak
	}
	return defaultHealthStreak
}

func (e *ErrorWatcher) harnessName() string {
	if e.Harness == "" {
		return "harness"
	}
	return e.Harness
}

func (e *ErrorWatcher) Name() string { return "error-watcher" }

func (e *ErrorWatcher) Tick(ctx context.Context) error {
	if e.Logs == nil || e.Host == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()

	e.pollLogs(now)
	e.pollHealth(ctx)

	chunk, fromHealth := e.due(now)
	if chunk == "" {
		return nil
	}
	// The cooldown holds pending output rather than dropping it — it gets its
	// turn on a later tick, once the previous fix has had a chance to land.
	if !e.lastFix.IsZero() && now.Sub(e.lastFix) < e.cooldown() {
		return nil
	}
	fp := fingerprint(chunk)
	if !fromHealth && e.fixed[fp] {
		e.pending = ""
		return nil
	}
	errLine := firstErrorLine(chunk)
	if e.Control != nil && e.PIN != "" {
		msg := errLine
		e.Control.PutConvergenceBestEffort(ctx, e.PIN, controlplane.Convergence{
			LastError: &msg, Watcher: "fixing",
		})
	}

	// Push fixes to the real canonical: the box URL for an off-box conductor
	// (Host.Remote), or the local bare for an in-host fleet. Cloning the local
	// mirror here (as before) would strand fixes on the laptop.
	src := e.Host.Bare
	if e.Host.Remote != "" {
		src = e.Host.Remote
	}
	tmp, err := os.MkdirTemp("", "slopball-watcher-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := sbGit.Run(ctx, "", "clone", src, tmp); err != nil {
		return err
	}
	c := &sbGit.Cmd{Dir: tmp, Env: e.ID.EnvVars()}
	if err := c.Run(ctx, "checkout", "main"); err != nil {
		return err
	}

	writePath := "slopball-fix.log"
	writeBody := fmt.Sprintf("auto-fix for log error:\n%s\n", firstErrorLine(chunk))
	commitMsg := "error-watcher: auto-fix"
	if e.Fix != nil {
		// Harness path: hand the model the error + current source, apply its patch.
		watcherLog.Infof("dev-server error detected — %s generating a fix: %s", e.harnessName(), firstErrorLine(chunk))
		e.States.Working(RoleErrorWatcher, e.harnessName()+" fixing: "+firstErrorLine(chunk))
		defer e.States.Idle(RoleErrorWatcher)
		p, body, ferr := e.Fix(ctx, chunk, readTree(tmp))
		if ferr != nil {
			// The harness had its shot at this one. Bank it as attempted so a
			// failing fixer cannot retry the same error every tick forever.
			e.bank(now, fp, fromHealth)
			watcherLog.Warnf("%s could not produce a fix: %v", e.harnessName(), ferr)
			return ferr
		}
		writePath, writeBody = p, body
		commitMsg = "error-watcher: fix " + p
		watcherLog.Infof("%s wrote a fix for %s — pushing to main", e.harnessName(), p)
	} else {
		watcherLog.Infof("dev-server error detected — no harness, writing marker file: %s", firstErrorLine(chunk))
	}

	dst := filepath.Join(tmp, writePath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, []byte(writeBody), 0o644); err != nil {
		return err
	}
	if err := c.Run(ctx, "add", writePath); err != nil {
		return err
	}
	// A fixer that decides the file is already correct hands back what is
	// already on main. `git commit` calls that a failure and exit status 1 is a
	// terrible way to report "nothing was wrong" — bank it and move on, so the
	// tick does not error and the same input cannot queue up again.
	if err := c.Run(ctx, "diff", "--cached", "--quiet"); err == nil {
		watcherLog.Infof("%s produced no change to %s — nothing to push", e.harnessName(), writePath)
		e.bank(now, fp, fromHealth)
		return nil
	}
	if err := c.Run(ctx, "commit", "-m", commitMsg); err != nil {
		return err
	}
	if err := c.Run(ctx, "push", "origin", "main"); err != nil {
		return err
	}
	e.bank(now, fp, fromHealth)
	e.Fixes++
	if e.Control != nil && e.PIN != "" {
		cleared := ""
		e.Control.PutConvergenceBestEffort(ctx, e.PIN, controlplane.Convergence{
			LastError: &cleared, Watcher: "idle",
		})
	}
	return nil
}

// pollLogs pulls whatever is new in the watchable stream into pending. The
// timestamps are when the watcher *saw* the output, not when it was written —
// it polls, so that is the only clock it has.
func (e *ErrorWatcher) pollLogs(now time.Time) {
	all := e.Logs.Watchable()
	if len(all) < e.seen { // the buffer was reset under us
		e.seen = 0
	}
	if len(all) <= e.seen {
		return
	}
	chunk := all[e.seen:]
	e.seen = len(all)
	if strings.TrimSpace(chunk) == "" {
		return
	}
	if e.pending == "" {
		e.firstPending = now
	}
	e.pending += chunk
	e.lastAppend = now
}

func (e *ErrorWatcher) pollHealth(ctx context.Context) {
	if e.Health == nil {
		return
	}
	if err := e.Health(ctx); err != nil {
		e.unhealthy++
		e.healthErr = err.Error()
		return
	}
	// Answering again ends the outage, and with it the one-fix-per-outage hold.
	e.unhealthy, e.healthErr, e.healthFixed = 0, "", false
}

// due reports the text to hand the fixer, and whether it came from the health
// probe rather than the log stream. The two triggers are independent: either
// can fire on its own, and neither gates the other.
func (e *ErrorWatcher) due(now time.Time) (string, bool) {
	if e.pending != "" {
		quiet := now.Sub(e.lastAppend) >= e.settle()
		waited := now.Sub(e.firstPending) >= e.maxWait()
		if quiet || waited {
			return e.pending, false
		}
	}
	if e.unhealthy >= e.healthStreak() && !e.healthFixed {
		return fmt.Sprintf("the dev server is not answering: %s\n\nlast output:\n%s",
			e.healthErr, e.pending), true
	}
	return "", false
}

// bank records that this trigger has been acted on.
func (e *ErrorWatcher) bank(now time.Time, fp string, fromHealth bool) {
	e.lastFix = now
	e.pending = ""
	if fromHealth {
		e.healthFixed = true
		e.unhealthy = 0
		return
	}
	if e.fixed == nil {
		e.fixed = map[string]bool{}
	}
	e.fixed[fp] = true
}

// fingerprint reduces a failure to something stable enough to recognise on a
// second sighting: its most error-shaped line, minus the digits that churn
// between otherwise identical failures (line/column numbers, ports, timings).
func fingerprint(s string) string {
	line := firstErrorLine(s)
	if line == "" || line == "ERROR: unknown" {
		for _, l := range strings.Split(s, "\n") {
			if strings.TrimSpace(l) != "" {
				line = strings.TrimSpace(l)
				break
			}
		}
	}
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(line) {
		switch {
		case r >= '0' && r <= '9':
		case r == ' ' || r == '\t':
			space = b.Len() > 0
		default:
			if space {
				b.WriteByte(' ')
				space = false
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// readTree loads small tracked text files from the clone for the fixer prompt,
// skipping .git, binaries, and anything over 64 KiB.
func readTree(root string) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > 64*1024 {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil || !utf8Text(data) {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		out[rel] = string(data)
		return nil
	})
	return out
}

func utf8Text(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return true
}

// errorSignals are substrings that mark a broken build/runtime in dev-server
// output. "ERROR:" is slopball's own convention (the runtime reconciler writes
// it on migration/reseed failure, plan 20); the rest catch common framework
// build breakage — including the compile errors a conflict marker or a bad
// merge produces — so the watcher actually fires on a broken site, not just on
// slopball-tagged lines.
var errorSignals = []string{
	"ERROR:",
	"Failed to compile",
	"Module not found",
	"SyntaxError",
	"Unexpected token",
	"Cannot find module",
	"error TS",    // tsc
	"panic:",      // go
	"Traceback (", // python
}

func looksLikeError(s string) bool {
	for _, sig := range errorSignals {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

func firstErrorLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		for _, sig := range errorSignals {
			if strings.Contains(line, sig) {
				return strings.TrimSpace(line)
			}
		}
	}
	return "ERROR: unknown"
}
