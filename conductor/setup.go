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
	"github.com/nwylynko/slopball-cli/contracts"
	"github.com/nwylynko/slopball-cli/controlplane"
	sbGit "github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/logx"
	"github.com/nwylynko/slopball-cli/runtime"
)

var setupLog = logx.New("setup")

// MarkerFile records on main that the setup role has run. Committed (like the
// brief) so "once" survives a host restart, a migration, and a box cutover —
// the role ticks every 2s and idempotence is its entire safety story.
const MarkerFile = ".slopball/setup.done"

// setupTimeout bounds one scaffold. A create-next-app + install is 30–90s; the
// ceiling exists because plan 29's first-run flow blocks on exactly one tick,
// and an agent wedged on an interactive generator prompt must fail the tick
// rather than hang the session forever.
const setupTimeout = 10 * time.Minute

// Scaffolder runs an agentic harness turn inside dir with the given prompt and
// leaves a project behind. Nil disables the role — there is deliberately no
// mechanical fallback (see plan 28 Notes: a stub index.html masquerading as a
// scaffold is worse than an empty repo and an honest log line).
//
// onActivity is how the run reports what it is doing *while* it runs. It is a
// parameter rather than a field on the client because a scaffold is minutes
// long and the role — which owns the state board — is the only thing that knows
// what to call the work; without it the dashboard showed the brief it was
// handed at the start, unchanged, for the whole run.
type Scaffolder func(ctx context.Context, dir, prompt string, onActivity func(string)) error

// Setup is the fleet's third role: it turns the session's one-line brief into
// an actual project on main (plan 28). Two modes, both requiring a brief:
// *scaffold* fills an empty canonical, *adapt* moves a repo someone brought
// toward the brief. It works in a private temp clone so it never fights the
// merger over Host.Work, and lands exactly one ordinary commit on main.
type Setup struct {
	Host    *canonical.Host
	ID      sbGit.Identity
	Agent   Scaffolder // nil = role disabled (no mechanical fallback)
	Harness string     // harness name, for logs
	Control *controlplane.Client
	PIN     string
	// States is the dashboard's view of this role (plan 36 §2). Nil is legal.
	States *StateBoard

	mu           sync.Mutex
	saidDisabled bool
}

func (s *Setup) Name() string { return "setup" }

func (s *Setup) harnessName() string {
	if s.Harness == "" {
		return "harness"
	}
	return s.Harness
}

func (s *Setup) Tick(ctx context.Context) error {
	if s.Host == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Cheap reads first: a session with no brief, or one already scaffolded,
	// costs one `git show` per tick and nothing else.
	brief := strings.TrimSpace(s.showMain(ctx, contracts.BriefFile))
	if brief == "" {
		return nil
	}
	if s.marked(ctx) {
		return nil
	}
	if s.Agent == nil {
		if !s.saidDisabled {
			s.saidDisabled = true
			setupLog.Infof("setup disabled: no harness CLI — the brief stays on main, so every joined agent's contract still carries it")
		}
		return nil
	}

	files := s.projectFiles(ctx)
	mode, prompt := "scaffold", scaffoldPrompt(brief)
	if len(files) > 0 {
		mode, prompt = "adapt", adaptPrompt(brief)
	}
	setupLog.Infof("%s mode — %s working from the brief: %s", mode, s.harnessName(), oneLine(brief))
	s.States.Working(RoleSetup, fmt.Sprintf("%s (%s): %s", s.harnessName(), mode, oneLine(brief)))
	defer s.States.Idle(RoleSetup)
	// The agent's own report of what it is doing replaces the brief line above
	// as soon as it has one. Since is preserved across these, so the dashboard's
	// elapsed timer keeps measuring the scaffold rather than the latest step.
	onActivity := func(a string) {
		if a = strings.TrimSpace(a); a != "" {
			s.States.Working(RoleSetup, fmt.Sprintf("%s: %s", s.harnessName(), oneLine(a)))
		}
	}

	src := s.Host.Bare
	if s.Host.Remote != "" {
		src = s.Host.Remote
	}
	tmp, err := os.MkdirTemp("", "slopball-setup-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := sbGit.Run(ctx, "", "clone", src, tmp); err != nil {
		return err
	}
	c := &sbGit.Cmd{Dir: tmp, Env: s.ID.EnvVars()}
	if err := c.Run(ctx, "checkout", "main"); err != nil {
		return err
	}

	// Ignore rules go in BEFORE the agent runs so the generator inherits them.
	if err := ensureIgnore(tmp); err != nil {
		return err
	}

	runCtx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	// Scaffold mode hands the tree over to a generator, which refuses to run
	// alongside slopball's own files — so they step out and come back, with any
	// collision reconciled by a second turn (plan 31). Adapt mode has a real
	// project and no generator, so nothing moves.
	run := func() error { return s.Agent(runCtx, tmp, prompt, onActivity) }
	if mode == "scaffold" {
		run = func() error { return s.scaffoldHandover(runCtx, tmp, prompt, onActivity) }
	}
	if err := run(); err != nil {
		setupLog.Warnf("%s could not %s the project: %v — main untouched, retrying next tick", s.harnessName(), mode, err)
		return err
	}

	// Re-apply: the generator may have written its own ignore file over ours.
	if err := ensureIgnore(tmp); err != nil {
		return err
	}
	if err := c.Run(ctx, "add", "-A"); err != nil {
		return err
	}
	if err := guardStaged(ctx, tmp); err != nil {
		setupLog.Errorf("refusing to commit the %s: %v", mode, err)
		return err
	}
	if err := os.MkdirAll(filepath.Join(tmp, ".slopball"), 0o755); err != nil {
		return err
	}
	// slopball owns .slopball/ — the agent is forbidden to touch it. The box
	// container sets PORT= too, but mesh hosts read this file and Vite ignores
	// env anyway, so the framework config the prompt asks for is what matters.
	if err := os.WriteFile(filepath.Join(tmp, filepath.FromSlash(runtime.SharedEnvPath)), []byte("PORT=3000\n"), 0o644); err != nil {
		return err
	}
	marker := fmt.Sprintf("%s by slopball's setup role from .slopball/brief.md\n", mode)
	if err := os.WriteFile(filepath.Join(tmp, filepath.FromSlash(MarkerFile)), []byte(marker), 0o644); err != nil {
		return err
	}
	if err := c.Run(ctx, "add", runtime.SharedEnvPath, MarkerFile); err != nil {
		return err
	}
	if err := c.Run(ctx, "commit", "-m", "setup: "+mode+" the project from the brief"); err != nil {
		return err
	}
	if err := c.Run(ctx, "push", "origin", "main"); err != nil {
		return err
	}
	setupLog.Infof("%s landed on main — every client's next sync pulls the project", mode)
	return nil
}

// showMain reads one path from main. Empty when it does not exist.
func (s *Setup) showMain(ctx context.Context, path string) string {
	out, err := sbGit.Output(ctx, s.Host.Bare, "show", "main:"+path)
	if err != nil {
		return ""
	}
	return out
}

func (s *Setup) marked(ctx context.Context) bool {
	_, err := sbGit.Output(ctx, s.Host.Bare, "show", "main:"+MarkerFile)
	return err == nil
}

// projectFiles lists what is tracked on main that is actually somebody's
// project, as opposed to slopball's own artifacts. Non-empty → adapt mode.
func (s *Setup) projectFiles(ctx context.Context) []string {
	out, err := sbGit.Output(ctx, s.Host.Bare, "ls-tree", "-r", "--name-only", "main")
	if err != nil {
		return nil
	}
	var paths []string
	for _, p := range strings.Split(out, "\n") {
		p = strings.TrimSpace(p)
		if p == "" || isSlopballArtifact(p) {
			continue
		}
		// The seed README canonical.Create writes is not a project.
		if p == "README.md" && strings.TrimSpace(s.showMain(ctx, p)) == strings.TrimSpace(canonical.SeedReadme(s.Host.PIN)) {
			continue
		}
		paths = append(paths, p)
	}
	return paths
}

func isSlopballArtifact(p string) bool {
	return strings.HasPrefix(p, ".slopball/") ||
		strings.HasPrefix(p, ".cursor/") ||
		p == "AGENTS.md" || p == "CLAUDE.md" || p == ".gitignore"
}

// WriteBrief commits brief onto main when main carries none, and reports
// whether it wrote one. This is how `slopball --brief "…"` (and plan 29's
// wizard) hand the role its input without a flag living in memory.
func WriteBrief(ctx context.Context, host *canonical.Host, id sbGit.Identity, brief string) error {
	brief = strings.TrimSpace(brief)
	if host == nil || brief == "" {
		return nil
	}
	if _, err := sbGit.Output(ctx, host.Bare, "show", "main:"+contracts.BriefFile); err == nil {
		return nil // main already has one; never overwrite what the session agreed on
	}
	src := host.Bare
	if host.Remote != "" {
		src = host.Remote
	}
	tmp, err := os.MkdirTemp("", "slopball-brief-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := sbGit.Run(ctx, "", "clone", src, tmp); err != nil {
		return err
	}
	c := &sbGit.Cmd{Dir: tmp, Env: id.EnvVars()}
	if err := c.Run(ctx, "checkout", "main"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(tmp, ".slopball"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, filepath.FromSlash(contracts.BriefFile)), []byte(brief+"\n"), 0o644); err != nil {
		return err
	}
	if err := c.Run(ctx, "add", contracts.BriefFile); err != nil {
		return err
	}
	if err := c.Run(ctx, "commit", "-m", "brief: what we're building"); err != nil {
		return err
	}
	return c.Run(ctx, "push", "origin", "main")
}

// WriteContracts commits the per-harness agent contracts onto main, and is a
// no-op when they are already there and unchanged.
//
// They belong on main, not only in each client's work tree, for two reasons
// (plan 31). The setup role's scaffolding agent should build against the same
// protocol its teammates will follow — it cannot, if the contracts only exist on
// branches it never sees. And a scaffold that writes its own AGENTS.md must
// collide with slopball's *while the setup role is still running*, because that
// is the only moment anything can reconcile the two; arriving later via a merge
// puts the conflict where nothing is left to resolve it.
func WriteContracts(ctx context.Context, host *canonical.Host, id sbGit.Identity) error {
	if host == nil {
		return nil
	}
	src := host.Bare
	if host.Remote != "" {
		src = host.Remote
	}
	tmp, err := os.MkdirTemp("", "slopball-contracts-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := sbGit.Run(ctx, "", "clone", src, tmp); err != nil {
		return err
	}
	c := &sbGit.Cmd{Dir: tmp, Env: id.EnvVars()}
	if err := c.Run(ctx, "checkout", "main"); err != nil {
		return err
	}
	// Install reads the brief out of the tree it writes into, so this must run
	// after WriteBrief or the contracts quote nothing.
	if err := contracts.Install(tmp, host.PIN); err != nil {
		return err
	}
	// `add -A` rather than one add per contract, because Install now *removes*
	// what an earlier slopball wrote (contracts.Legacy) and a deletion nobody
	// stages is a deletion nobody commits — the stale file would come straight
	// back on the next clone. This is a fresh clone of main, so the only thing
	// there is to stage is what Install just did.
	if err := c.Run(ctx, "add", "-A"); err != nil {
		return err
	}
	staged, err := sbGit.Output(ctx, tmp, "diff", "--cached", "--name-only")
	if err != nil {
		return err
	}
	if strings.TrimSpace(staged) == "" {
		return nil // already current: a resumed canonical must not grow a commit per start
	}
	if err := c.Run(ctx, "commit", "-m", "contracts: how agents work in this session"); err != nil {
		return err
	}
	return c.Run(ctx, "push", "origin", "main")
}

// ignoreRules is the baseline that keeps dependencies and build output out of
// canonical. node_modules is never synced (MASTERPLAN §9.2) and a scaffold runs
// an install, so without this the session's first commit is 200MB.
var ignoreRules = []string{
	"node_modules/", ".next/", "dist/", "build/", "target/",
	"__pycache__/", ".venv/", ".env", ".DS_Store",
}

// IgnoreRules is the baseline as data, for the guard that holds it level with
// canonical's seed refusal. It is the mirror image of canonical.SeedGuardDirs,
// exported for the same reason and returning a copy for the same reason: the
// two lists are one decision split across two packages, and the only way to
// notice them drifting apart is to compare them from a test that can see both.
// Nothing else needs it — a caller that wants the rules applied wants
// ensureIgnore.
func IgnoreRules() []string { return append([]string(nil), ignoreRules...) }

// ensureIgnore adds any missing baseline rule to .gitignore, extending the
// repo's existing file rather than replacing it (adapt mode brings its own).
func ensureIgnore(dir string) error {
	path := filepath.Join(dir, ".gitignore")
	existing, _ := os.ReadFile(path)
	have := map[string]bool{}
	for _, l := range strings.Split(string(existing), "\n") {
		have[strings.TrimSpace(l)] = true
	}
	var add []string
	for _, rule := range ignoreRules {
		if !have[rule] && !have[strings.TrimSuffix(rule, "/")] {
			add = append(add, rule)
		}
	}
	if len(add) == 0 {
		return nil
	}
	body := string(existing)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if body != "" {
		body += "\n"
	}
	body += "# slopball: never sync dependencies or build output\n" + strings.Join(add, "\n") + "\n"
	return os.WriteFile(path, []byte(body), 0o644)
}

// Staged bounds. A real scaffold is hundreds of small files; anything past this
// means the ignore rules missed a dependency directory, and shipping it to
// every client's mirror is worse than refusing and saying so.
const (
	maxStagedFiles = 4000
	maxStagedBytes = 40 << 20
)

func guardStaged(ctx context.Context, dir string) error {
	out, err := sbGit.Output(ctx, dir, "diff", "--cached", "--name-only")
	if err != nil {
		return err
	}
	var files int
	var bytes int64
	for _, p := range strings.Split(out, "\n") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		files++
		if st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p))); err == nil {
			bytes += st.Size()
		}
	}
	if files > maxStagedFiles || bytes > maxStagedBytes {
		return fmt.Errorf("staged tree is %d files / %d MiB — that looks like committed dependencies, not a project (limits: %d files / %d MiB)",
			files, bytes>>20, maxStagedFiles, maxStagedBytes>>20)
	}
	return nil
}

func oneLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 120 {
		return s[:117] + "…"
	}
	return s
}

// sharedSetupRules are the constraints both modes need. slopball owns the
// commit, so the agent must never touch git — a `git init` or a stray commit in
// the temp clone would land something nobody reviewed on main.
const sharedSetupRules = `
Hard rules:
- Work IN THE CURRENT DIRECTORY. It is an existing git work tree (it has dotfiles). Do not create a subdirectory for the project, do not run "git init", and NEVER run any git command — slopball makes the commit.
- Do not create, edit or delete anything under .slopball/.
- Leave the project runnable: a dev script in package.json (or the ecosystem's equivalent) and a committed lockfile.
- Do not start a dev server or any other long-running process. Install dependencies if you need to, but exit when the files are written.
- Dependencies and build output are already gitignored; leave those rules alone.
- The dev server must listen on port 3000 and accept connections from outside localhost — slopball's box and session-network splice dial 127.0.0.1:3000, not the framework default. Next.js and similar stacks honor PORT=3000; Vite does NOT — set server.port to 3000 and server.host to true in vite.config (or the ecosystem equivalent).
- Teammates open the dev server through slopball, so it will see requests with a Host header like "<pin>.slopball.localhost:<port>" rather than localhost. If the framework gates on that (Next.js allowedDevOrigins, Vite server.allowedHosts), allow "*.slopball.localhost" in the dev config.`

func scaffoldPrompt(brief string) string {
	return fmt.Sprintf(`You are slopball's setup role-agent. Several AI agents are about to build one product together, and the repository is empty. Create the project they will build in, from this brief:

%s

Prefer the ecosystem's own generator over hand-writing files (for example: npx create-next-app@latest ., npm create vite@latest ., cargo new --vcs none .). Pick sensible defaults rather than asking questions — nothing can answer an interactive prompt.

The directory has been cleared for you: slopball's own files are held aside so generators that refuse a non-empty directory will run. They come back after this turn, so do not recreate them — if you want to write an AGENTS.md of your own, do, and you will be asked to merge the two.
%s`, brief, sharedSetupRules)
}

func adaptPrompt(brief string) string {
	return fmt.Sprintf(`You are slopball's setup role-agent. This is someone's EXISTING PROJECT, which they brought into a slopball session. Move it toward this brief:

%s

Guardrails, because this is not your repository:
- Read the project first and follow its conventions.
- Make the SMALLEST COHERENT SET OF CHANGES that moves it toward the brief.
- NEVER delete or rewrite files the brief does not reach. Prefer additive work — new routes, components, config — over restructuring.
- If the brief is already satisfied, change nothing.
%s`, brief, sharedSetupRules)
}
