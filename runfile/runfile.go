// Package runfile owns `.slopball/run.json` — the project's own install and dev
// commands, committed to canonical main (plan 29).
//
// They live in git rather than in a flag on whoever booted, because they are
// project facts, not machine facts: a host migration (plan 16), a box cutover
// (plan 25) or plan 30's next `git` lease holder all inherit the same commands
// with nothing threaded through. Deliberately NOT `.slopball/runtime.env`,
// which the runtime reconciler materializes into the app's own `.env` — the dev
// command is slopball's business, not the application's environment.
package runfile

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/nwylynko/slopball-cli/canonical"
	sbGit "github.com/nwylynko/slopball-cli/git"
)

// Path is where the file lives, relative to the repo root.
const Path = ".slopball/run.json"

// File is the parsed run commands. Empty fields mean "nothing declared", which
// leaves detection in charge.
type File struct {
	Install []string
	Dev     []string
}

type wire struct {
	Install string `json:"install,omitempty"`
	Dev     string `json:"dev,omitempty"`
}

// Read loads the run file from a checked-out work tree. Missing, unreadable or
// malformed all mean "nothing declared" — this file must never be able to fail
// a host start.
func Read(workDir string) File {
	data, err := os.ReadFile(filepath.Join(workDir, filepath.FromSlash(Path)))
	if err != nil {
		return File{}
	}
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return File{}
	}
	return File{Install: strings.Fields(w.Install), Dev: strings.Fields(w.Dev)}
}

// ReadFromMain loads the run file from canonical's main ref rather than from a
// checked-out tree. This is the one a host start must use: a resumed canonical's
// work tree is whatever the last run left on disk, which can be behind main —
// and the file's whole purpose is that a *later* host inherits it.
func ReadFromMain(ctx context.Context, host *canonical.Host) File {
	if host == nil {
		return File{}
	}
	out, err := sbGit.Output(ctx, host.Bare, "show", "main:"+Path)
	if err != nil {
		return File{}
	}
	var w wire
	if err := json.Unmarshal([]byte(out), &w); err != nil {
		return File{}
	}
	return File{Install: strings.Fields(w.Install), Dev: strings.Fields(w.Dev)}
}

// Resolve applies the precedence every host start uses: an explicit flag beats
// the committed file, which beats detection.
func Resolve(flag, committed []string, detect func() []string) []string {
	if len(flag) > 0 {
		return flag
	}
	if len(committed) > 0 {
		return committed
	}
	if detect == nil {
		return nil
	}
	return detect()
}

// Commit writes the run commands onto main through a private temp clone (the
// same pattern the error-watcher and setup role use, so it never fights the
// merger over Host.Work). Committing nothing is a no-op: an empty file would
// later read as "this project declares no dev command" and suppress detection.
func Commit(ctx context.Context, host *canonical.Host, id sbGit.Identity, install, dev []string) error {
	if host == nil || (len(install) == 0 && len(dev) == 0) {
		return nil
	}
	body, err := json.MarshalIndent(wire{
		Install: strings.Join(install, " "),
		Dev:     strings.Join(dev, " "),
	}, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')

	src := host.Bare
	if host.Remote != "" {
		src = host.Remote
	}
	tmp, err := os.MkdirTemp("", "slopball-runfile-*")
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
	// Already identical on main → nothing to say.
	if existing, err := os.ReadFile(filepath.Join(tmp, filepath.FromSlash(Path))); err == nil &&
		strings.TrimSpace(string(existing)) == strings.TrimSpace(string(body)) {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(tmp, ".slopball"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, filepath.FromSlash(Path)), body, 0o644); err != nil {
		return err
	}
	if err := c.Run(ctx, "add", Path); err != nil {
		return err
	}
	if err := c.Run(ctx, "commit", "-m", "run: how this project installs and starts"); err != nil {
		return err
	}
	return c.Run(ctx, "push", "origin", "main")
}
