// Package syncengine implements slopball sync = pull + push (plans/04, 35).
// Agents call this at task boundaries; humans never see git.
package syncengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nwylynko/slopball-cli/controlplane"
	sbGit "github.com/nwylynko/slopball-cli/git"
)

const intentTrailer = "Slopball-Intent"

// Base describes the `main` a client resolved against — which commit, how old
// it is, and whether the hub could still be reached when the client integrated
// it. Sync returns it so the agent and the human learn that a resolution was
// made against an old base (plan 35 decision 4: surfaced, never blocking).
type Base struct {
	Ref          string        // the ref merged: "mirror/main" or "origin/main"
	SHA          string        // the commit that was integrated
	Age          time.Duration // how long ago that commit was made
	HubSHA       string        // main on the hub, when it could be asked
	HubReachable bool          // false when the hub did not answer
}

// Stale is true when the base is not known to be level with the hub — either
// the hub was unreachable, or it answered with a main this client has not seen.
// Both degrade toward hub-side resolution, which is the right way to fail.
func (b Base) Stale() bool {
	if !b.HubReachable {
		return true
	}
	return b.HubSHA != "" && b.HubSHA != b.SHA
}

// Report is the one-line human/agent summary. Empty when the base is fresh —
// the ordinary case should add no noise.
func (b Base) Report() string {
	if !b.Stale() {
		return ""
	}
	if !b.HubReachable {
		return fmt.Sprintf("resolved against a %s old main (%s) — the hub was unreachable, so this base may be behind", humanAge(b.Age), shortSHA(b.SHA))
	}
	return fmt.Sprintf("resolved against a %s old main (%s) — the hub has moved on to %s, so this base is behind", humanAge(b.Age), shortSHA(b.SHA), shortSHA(b.HubSHA))
}

// note is the compact form carried into the intent trailer so the merger sees
// that a resolution was made against an old main.
func (b Base) note() string {
	if !b.Stale() {
		return ""
	}
	why := "hub unreachable"
	if b.HubReachable {
		why = "hub at " + shortSHA(b.HubSHA)
	}
	return fmt.Sprintf("[base: main %s, %s old, %s]", shortSHA(b.SHA), humanAge(b.Age), why)
}

// annotate folds the staleness note into an agent's intent.
func (b Base) annotate(intent string) string {
	if intent == "" {
		return ""
	}
	if n := b.note(); n != "" {
		return intent + " " + n
	}
	return intent
}

// Push commits the working tree (if dirty) with an intent note trailer and
// pushes the client branch to the session remote.
//
// Under pull-first this runs on a tree that Pull has already reconciled and
// committed, so the commit half is usually a no-op; it stays for bare
// `slopball push`, which promises no preceding pull.
func Push(ctx context.Context, work, branch, remote, intent string, id sbGit.Identity) error {
	if intent == "" {
		return fmt.Errorf("push requires an intent note")
	}
	c := &sbGit.Cmd{Dir: work, Env: id.EnvVars()}

	if err := commitLocal(ctx, c, intent); err != nil {
		return err
	}
	if remote == "" {
		remote = "origin"
	}
	if err := c.Run(ctx, "push", remote, "HEAD:"+branch); err != nil {
		return fmt.Errorf("could not publish %s to the hub at %s: %w\n"+
			"your work is committed locally and main is already integrated, so nothing is lost — "+
			"re-run `slopball sync` once the hub is reachable again", branch, remoteURL(ctx, c, remote), err)
	}
	return nil
}

// Pull integrates main into the current branch and reports the base it used.
// Prefer merging from the local mirror the join daemon keeps fresh (plan 11);
// otherwise fetch from the remote, which needs the hub to be up.
//
// intent, when set, is the message the client's local work is committed with —
// Sync passes the agent's intent so the trailer survives pull-first. Bare
// `slopball pull` passes "" and gets the generic message.
func Pull(ctx context.Context, work, remote, mirrorDir, intent string, id sbGit.Identity) (Base, error) {
	c := &sbGit.Cmd{Dir: work, Env: id.EnvVars()}
	if remote == "" {
		remote = "origin"
	}

	base, err := resolveBase(ctx, c, remote, mirrorDir)
	if err != nil {
		return base, err
	}

	// Commit any local work (tracked edits AND untracked files) before merging.
	// Otherwise git aborts the whole merge the moment an untracked file — the
	// contract files join installs, or a file another agent independently created
	// — would be overwritten, leaving nothing for the agent to resolve. Once
	// committed, the same collision becomes a normal 3-way merge that lands
	// conflict markers the client's agent can resolve and re-sync.
	if err := commitLocal(ctx, c, base.annotate(intent)); err != nil {
		return base, err
	}

	if err := c.Run(ctx, "merge", "--no-edit", base.Ref); err != nil {
		// Leave the conflict in the tree for this client's agent to resolve
		// (§6.1 spoke). Nothing has been published yet, so nobody else is
		// resolving it — that is the whole point of pull-first.
		return base, fmt.Errorf("pull merge conflict (resolve locally then re-sync): %w", err)
	}
	return base, nil
}

// resolveBase makes main available locally and describes it. With a mirror this
// is a local operation that works with the hub gone; without one it needs the
// hub, and failing there is the mirror-less offline case.
func resolveBase(ctx context.Context, c *sbGit.Cmd, remote, mirrorDir string) (Base, error) {
	if mirrorDir == "" {
		if err := c.Run(ctx, "fetch", remote, "main"); err != nil {
			return Base{}, fmt.Errorf("cannot reach the hub at %s to fetch main, and this machine keeps no local mirror of main to fall back on: %w\n"+
				"run `slopball join <pin>` — its daemon holds the session connection and keeps a background-fresh mirror, "+
				"which is what lets a client keep working while the hub is away", remoteURL(ctx, c, remote), err)
		}
		base := Base{Ref: remote + "/main", HubReachable: true}
		base.SHA = revParse(ctx, c, base.Ref)
		base.HubSHA = base.SHA
		base.Age = commitAge(ctx, c, base.SHA)
		return base, nil
	}

	// Local merge of the already-fresh mirror (foundation: add mirror as remote once).
	_ = c.Run(ctx, "remote", "remove", "mirror")
	if err := c.Run(ctx, "remote", "add", "mirror", mirrorDir); err != nil {
		return Base{}, err
	}
	if err := c.Run(ctx, "fetch", "mirror", "main"); err != nil {
		return Base{}, fmt.Errorf("local main mirror at %s is unusable: %w", mirrorDir, err)
	}
	base := Base{Ref: "mirror/main"}
	base.SHA = revParse(ctx, c, base.Ref)
	base.Age = commitAge(ctx, c, base.SHA)
	// Ask the hub where main is, purely to report freshness. A hub that does not
	// answer marks the base stale; it never blocks the merge.
	if hub, err := c.Output(ctx, "ls-remote", remote, "refs/heads/main"); err == nil {
		base.HubReachable = true
		base.HubSHA = firstField(hub)
	}
	return base, nil
}

// Sync is pull then push — the task-boundary round-trip (plan 35).
//
// Reversed from the original push-then-pull so that a client branch only ever
// reaches the hub in a reconciled state. Under push-first the client published
// its branch *before* discovering it conflicted with main, and for that window
// the conductor's merger and the client's own agent were both resolving the same
// hunk — the loser's resolution came back as a second conflict on the same file
// with no clean side left to bias toward. The agent holding the context of what
// the human asked for is the one that makes its changes work in the repo.
//
// The invariant this buys: a branch is pushed only once it already contains
// main, so the hub's branch->main merge is a fast-forward in the common case.
// The merger's resolver stays as the absorber of the residual — main advancing
// while the agent resolves — which is rare and never concurrent.
func Sync(ctx context.Context, work, branch, remote, mirrorDir, intent string, id sbGit.Identity) (Base, error) {
	if intent == "" {
		return Base{}, fmt.Errorf("sync requires an intent note")
	}
	base, err := Pull(ctx, work, remote, mirrorDir, intent, id)
	if err != nil {
		return base, fmt.Errorf("pull: %w", err)
	}
	if err := Push(ctx, work, branch, remote, base.annotate(intent), id); err != nil {
		return base, fmt.Errorf("push: %w", err)
	}
	return base, nil
}

// AnnouncePushed tells the session that work arrived (plan 36 §2). It is the
// client's half of the console's work feed: `sync.pushed` here, `merge.applied`
// from the merger, and the gap between them is the interesting number.
//
// Best-effort by construction — a member whose control plane blinks still
// syncs, because the feed is what people watch and never what the merge path
// waits on. Shared by the `sync` verb and the emulator so the two cannot drift.
func AnnouncePushed(ctx context.Context, client *controlplane.Client, pin, member, branch, work, intent string) {
	if client == nil || pin == "" {
		return
	}
	sha, _ := sbGit.Output(ctx, work, "rev-parse", "HEAD")
	client.PublishEventBestEffort(ctx, pin, controlplane.EventSyncPushed, map[string]any{
		"member": member, "branch": branch, "intent": intent, "sha": strings.TrimSpace(sha),
	})
}

// CloneClient clones canonical, creates/checks out the client branch, and sets up work.
func CloneClient(ctx context.Context, remote, work, branch string, id sbGit.Identity) error {
	if err := os.MkdirAll(filepath.Dir(work), 0o755); err != nil {
		return err
	}
	if err := sbGit.Run(ctx, "", "clone", remote, work); err != nil {
		return err
	}
	c := &sbGit.Cmd{Dir: work, Env: id.EnvVars()}
	// Prefer the pre-created remote branch; otherwise branch off main.
	if err := c.Run(ctx, "checkout", "-B", branch, "origin/"+branch); err != nil {
		if err := c.Run(ctx, "checkout", "-B", branch, "origin/main"); err != nil {
			return err
		}
	}
	return c.Run(ctx, "push", "-u", "origin", branch)
}

// commitLocal stages and commits any dirty state on the current branch so a
// following merge sees a clean tree. No-op when there is nothing to commit.
// With an intent it records the Slopball-Intent trailer the merger reads.
func commitLocal(ctx context.Context, c *sbGit.Cmd, intent string) error {
	if err := c.Run(ctx, "add", "-A"); err != nil {
		return err
	}
	// Never fold conflict markers into the commit — a tree still carrying them
	// means a prior merge was left unresolved. Committing here would launder the
	// markers into history where they merge cleanly to main.
	if marked := conflictMarkerFiles(ctx, c); len(marked) > 0 {
		return fmt.Errorf("unresolved conflict markers in %s — resolve the conflict(s), then re-run sync", strings.Join(marked, ", "))
	}
	status, err := c.Output(ctx, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) == "" {
		return nil
	}
	msg := "slopball: local work before pull"
	if intent != "" {
		msg = fmt.Sprintf("slopball: %s\n\n%s: %s\n", shortIntent(intent), intentTrailer, flatten(intent))
	}
	return c.Run(ctx, "commit", "-m", msg)
}

// conflictMarkerFiles lists tracked files whose working-tree content still has
// git conflict markers. Anchored on the open/close markers, which do not
// collide with markdown or ordinary source. git grep exits non-zero on no
// match; that error is expected and ignored.
func conflictMarkerFiles(ctx context.Context, c *sbGit.Cmd) []string {
	out, _ := c.Output(ctx, "grep", "-lI", "--no-color", "-e", "^<<<<<<< ", "-e", "^>>>>>>> ")
	seen := map[string]bool{}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !seen[line] {
			seen[line] = true
			files = append(files, line)
		}
	}
	return files
}

// remoteURL resolves a remote name to its URL for error messages — "origin"
// tells a human nothing about which hub went away.
func remoteURL(ctx context.Context, c *sbGit.Cmd, remote string) string {
	out, err := c.Output(ctx, "remote", "get-url", remote)
	if err != nil || strings.TrimSpace(out) == "" {
		return remote
	}
	return strings.TrimSpace(out)
}

func revParse(ctx context.Context, c *sbGit.Cmd, ref string) string {
	out, err := c.Output(ctx, "rev-parse", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// commitAge is how long ago sha was committed. Zero when unreadable, which
// reads as "fresh" — staleness is decided by the SHA comparison, not the clock.
func commitAge(ctx context.Context, c *sbGit.Cmd, sha string) time.Duration {
	if sha == "" {
		return 0
	}
	out, err := c.Output(ctx, "show", "-s", "--format=%ct", sha)
	if err != nil {
		return 0
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0
	}
	age := time.Since(time.Unix(secs, 0))
	if age < 0 {
		return 0
	}
	return age
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func firstField(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func shortIntent(s string) string {
	s = flatten(s)
	if len(s) > 72 {
		return s[:72]
	}
	return s
}

func flatten(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ReadIntent extracts the Slopball-Intent trailer from a commit message.
func ReadIntent(msg string) string {
	for _, line := range strings.Split(msg, "\n") {
		if strings.HasPrefix(line, intentTrailer+":") {
			return strings.TrimSpace(strings.TrimPrefix(line, intentTrailer+":"))
		}
	}
	return ""
}
