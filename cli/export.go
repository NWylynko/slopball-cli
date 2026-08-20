package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	sbGit "github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/session"
	"github.com/spf13/cobra"
)

// newExportCmd builds `slopball export <path>` — the way work leaves a session
// without a GitHub account.
//
// It exists because the wizard asks about the GitHub snapshot during first run,
// before anybody has written a line of code, and the honest answer to "then how
// do I keep my work?" could not be "decide about GitHub now". The mirror stays
// what plan 37 decided it is — opt-in, and not your backup; this is the door
// that makes deferring that decision free.
func newExportCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "export <path>",
		Short: "Copy this session's workspace out to a standalone git repository you own",
		Long: "Writes the session's workspace to <path> as a normal git repository: the full\n" +
			"session history, your branch checked out exactly as it stands (uncommitted and\n" +
			"untracked work included), `main` beside it as the merged product, and no remote\n" +
			"at all — nothing in it points back at a session that stops existing.\n\n" +
			"Files git was told to ignore stay behind, so node_modules does not come with it.\n" +
			"The session is only read: nothing is committed, checked out or pushed.",
		Args: cobra.ExactArgs(1),
		RunE: runExport,
	}
	addPinFlag(c)
	return c
}

func runExport(cmd *cobra.Command, args []string) error {
	s, p, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	src := workDir(p)
	if _, err := os.Stat(filepath.Join(src, ".git")); err != nil {
		return fmt.Errorf("no work tree to export at %s — join the session first with `slopball join %s`", src, s.PIN)
	}
	dest, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}
	// `slopball export ./out` from inside the work tree is easy to type and bad
	// to do: it drops a second repository into the tree the merger is working
	// in, which the next sync then publishes to everybody.
	if dirInside(resolvePath(src), resolvePath(dest)) {
		return fmt.Errorf("%s is inside the session — export copies your work OUT of it, so pick a path of your own (e.g. ~/dev/my-project)", dest)
	}
	if err := requireEmptyExportDir(dest); err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	// --no-hardlinks because the export is meant to outlive the session, and a
	// local clone otherwise shares object files with a directory whose whole
	// purpose is to be swept.
	if err := sbGit.Run(ctx, "", "clone", "--no-hardlinks", "-q", src, dest); err != nil {
		return fmt.Errorf("export %s: %w", src, err)
	}
	hasMain, err := exportFreshestMain(ctx, p, src, dest)
	if err != nil {
		return err
	}
	// The one thing that must not survive: origin points at the session git
	// server, which answers only while the session is live and only to a member.
	// Left in place, the user's first `git push` fails against an address that
	// no longer means anything.
	if err := sbGit.Run(ctx, dest, "remote", "remove", "origin"); err != nil {
		return err
	}
	if err := exportDirtyState(ctx, src, dest); err != nil {
		return err
	}

	// The path on stdout, the story on stderr — the same split `open --print`
	// uses, so a script gets one line and a human gets told what they got.
	// Naming the branch matters: the export stands on the client's branch, not
	// main, and someone who assumes otherwise pushes the wrong one at whatever
	// remote they eventually pick.
	fmt.Fprintln(cmd.OutOrStdout(), dest)
	head, err := sbGit.Output(ctx, dest, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	errW := cmd.ErrOrStderr()
	fmt.Fprintf(errW, "exported %s → %s\n", s.PIN, dest)
	if hasMain {
		fmt.Fprintf(errW, "on %s (your work, uncommitted bits and all), with main beside it — `git checkout main` for the merged product\n", strings.TrimSpace(head))
	} else {
		// Never quietly. The export still holds this client's work, which is
		// why it is a warning and not a refusal — but "main is not in here" is
		// exactly the thing a user would otherwise discover weeks later.
		fmt.Fprintf(errW, "on %s (your work, uncommitted bits and all)\n"+
			"warning: no main came across — this machine has neither a fresh mirror of main nor an origin/main\n"+
			"         to read it from, so the export holds this branch only, not everyone's merged work\n",
			strings.TrimSpace(head))
	}
	fmt.Fprintln(errW, "a plain git repo with no remote: `git remote add origin <url>` if and when you pick one")
	return nil
}

// requireEmptyExportDir refuses anything but a missing or empty directory.
// Export writes a repository; there is no merge semantics for dropping one on
// top of a directory somebody already has work in.
func requireEmptyExportDir(dest string) error {
	st, err := os.Stat(dest)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is a file — export writes a new repository directory, so pick a path that is free", dest)
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s is not empty — export writes a fresh repository and will not write over one, so pick a path that is free", dest)
	}
	return nil
}

// exportFreshestMain makes `main` in the export the merged product.
//
// Two things go wrong without it, and the second is the one that bites:
//
//  1. A clone creates exactly one local branch — the checked-out one — and puts
//     everything else under refs/remotes/origin/. Dropping origin below then
//     takes those with it, so main does not survive the export at all.
//  2. The obvious repair, promoting the clone's own origin/main, promotes the
//     WRONG commit: that ref mirrors work/'s local main, which froze at the
//     moment this client joined. Sync merges main into the client's branch and
//     never back into that ref, so by the end of a session it is stale by every
//     merge the conductor has made.
//
// The fresh main lives in the join daemon's mirror, or failing that in work/'s
// own origin/main — the same order sync trusts them in (syncengine.resolveBase).
//
// It reports whether the export ends up with a main at all, because when it
// does not there is nothing to fall back on — see (1) — and the caller has to
// say so rather than hand over a repo quietly missing everyone else's work.
func exportFreshestMain(ctx context.Context, p session.Paths, src, dest string) (bool, error) {
	// A host's canonical work tree is itself checked out on main. Moving the ref
	// out from under a live checkout would desync its index for no gain: that
	// tree already holds canonical main.
	head, err := sbGit.Output(ctx, dest, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(head) == "main" {
		return true, nil
	}
	repo, ref := freshestMainRef(ctx, p, src)
	if repo == "" {
		return false, nil
	}
	if err := sbGit.Run(ctx, dest, "fetch", "--no-tags", "-q", repo, "+"+ref+":refs/heads/main"); err != nil {
		return false, err
	}
	return true, nil
}

// freshestMainRef names the repository and ref holding the newest main this
// machine has, or ("", "") when neither source resolves.
func freshestMainRef(ctx context.Context, p session.Paths, src string) (repo, ref string) {
	if m := mirrorDir(p); m != "" {
		if _, err := sbGit.Output(ctx, m, "rev-parse", "--verify", "-q", "refs/heads/main"); err == nil {
			return m, "refs/heads/main"
		}
	}
	if _, err := sbGit.Output(ctx, src, "rev-parse", "--verify", "-q", "refs/remotes/origin/main"); err == nil {
		return src, "refs/remotes/origin/main"
	}
	return "", ""
}

// exportDirtyState reproduces the workspace as it actually stands. A clone
// carries commits, and the work most at risk when someone asks how to keep
// their work is precisely the work they have not committed yet.
//
// It is done by reading the source, never by writing to it: no commit, no
// stash, no checkout. Agents are working in that tree, and a verb that moved
// their HEAD to make a copy would be racing every one of them.
func exportDirtyState(ctx context.Context, src, dest string) error {
	// diff HEAD covers staged and unstaged changes to tracked files, deletions
	// included; --binary so a changed image survives the round trip.
	patch, err := sbGit.Output(ctx, src, "diff", "HEAD", "--binary")
	if err != nil {
		return fmt.Errorf("read uncommitted work in %s: %w", src, err)
	}
	if strings.TrimSpace(patch) != "" {
		c := &sbGit.Cmd{Dir: dest, Stdin: []byte(patch)}
		if err := c.Run(ctx, "apply", "--binary", "-"); err != nil {
			return fmt.Errorf("carry uncommitted work into %s: %w", dest, err)
		}
	}
	return exportUntracked(ctx, src, dest)
}

// exportUntracked copies the files git can see but does not track. It asks git
// rather than walking the tree so .gitignore is honoured by the same engine
// that honours it everywhere else — which is what keeps node_modules, the one
// thing slopball never syncs, out of the export too.
func exportUntracked(ctx context.Context, src, dest string) error {
	out, err := sbGit.Output(ctx, src, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return fmt.Errorf("list untracked work in %s: %w", src, err)
	}
	for _, rel := range strings.Split(out, "\x00") {
		if rel == "" {
			continue
		}
		if err := copyExportFile(filepath.Join(src, rel), filepath.Join(dest, rel)); err != nil {
			return fmt.Errorf("export untracked %s: %w", rel, err)
		}
	}
	return nil
}

func copyExportFile(from, to string) error {
	st, err := os.Lstat(from)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		// ls-files -o reports regular files; a symlink or socket that slipped in
		// is not workspace content worth reproducing.
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	w, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, in); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}
