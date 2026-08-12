package canonical

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	sbGit "github.com/nwylynko/slopball-cli/git"
)

// Seeding a canonical from a local directory (plan 33).
//
// `--seed <dir>` is a session input like the brief and the run commands, and it
// has to reach canonical wherever canonical lives. Under remote-first the box
// creates the session's one canonical before this laptop can touch it, so the
// directory travels the same way everything else does: a clone of the box's
// git endpoint, a commit, a push to main. Shipping a tarball over ssh would be
// a second transport for something git already does, and would not work for a
// box the laptop only reaches over the mesh.

// dependencyDirs are the trees that must never enter canonical. Kept in step
// with conductor.IgnoreRules, which is asserted from there — a seed guard
// laxer than the ignore rules would let in exactly what they exist to keep out.
var dependencyDirs = []string{
	"node_modules", ".next", "dist", "build", "target", "__pycache__", ".venv",
}

// SeedGuardDirs is what PreflightSeedDir refuses to carry, for the test that
// holds it level with conductor's ignore rules.
func SeedGuardDirs() []string { return append([]string(nil), dependencyDirs...) }

// PreflightSeedDir is what makes `--seed <bad path>` fail before a container
// exists rather than minutes later, as a dev server that cannot find a
// package.json. It refuses an unreadable directory, and a directory carrying an
// unignored dependency tree — hundreds of megabytes pushed to canonical and
// then to every client's mirror is worse than a refusal that names the fix.
func PreflightSeedDir(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("--seed %s: %w", dir, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("--seed %s: not a directory", dir)
	}
	if _, err := os.ReadDir(dir); err != nil {
		return fmt.Errorf("--seed %s: %w", dir, err)
	}
	ignored := seedIgnoreRules(dir)
	for _, name := range dependencyDirs {
		if ignored[name] {
			continue
		}
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil && st.IsDir() {
			return fmt.Errorf("--seed %s carries %s/ and nothing ignores it — seeding it would push "+
				"dependencies into canonical and into every client's mirror.\n"+
				"       add %s/ to %s/.gitignore and try again",
				dir, name, name, dir)
		}
	}
	return nil
}

// seedIgnoreRules reads the seed's own .gitignore. Only top-level rules matter
// here: the guard asks whether the repo already keeps its dependency
// directories out, which is a one-line answer in every real project.
func seedIgnoreRules(dir string) map[string]bool {
	body, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		return nil
	}
	rules := map[string]bool{}
	for _, l := range strings.Split(string(body), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		rules[strings.Trim(l, "/")] = true
	}
	return rules
}

// SeedRemote lands a local directory on the `main` of a canonical served
// somewhere else, in one commit. It is the remote twin of hoststart's
// seedFromDir, differing only in pushing to a URL instead of writing into a
// bare repo on this disk.
//
// Call it before the setup role runs: a seeded session is by definition not a
// blank one, and the setup role picks scaffold-vs-adapt by looking at what is
// on main (plan 31).
func SeedRemote(ctx context.Context, gitURL, dir string, id sbGit.Identity) error {
	if gitURL == "" || dir == "" {
		return nil
	}
	if err := PreflightSeedDir(dir); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "slopball-seed-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := sbGit.Run(ctx, "", "clone", "--branch", "main", gitURL, tmp); err != nil {
		return fmt.Errorf("clone canonical to seed it: %w", err)
	}
	if err := CopyTree(dir, tmp); err != nil {
		return fmt.Errorf("copy %s into canonical: %w", dir, err)
	}
	c := &sbGit.Cmd{Dir: tmp, Env: id.EnvVars()}
	if err := c.Run(ctx, "add", "-A"); err != nil {
		return err
	}
	status, _ := c.Output(ctx, "status", "--porcelain")
	if strings.TrimSpace(status) == "" {
		return nil
	}
	if err := c.Run(ctx, "commit", "-m", "seed from "+filepath.Base(dir)); err != nil {
		return err
	}
	return c.Run(ctx, "push", "origin", "main")
}

// CopyTree copies src over dst, leaving dst's own .git alone — the seed brings
// a project, not a history.
func CopyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(os.PathSeparator)) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}
