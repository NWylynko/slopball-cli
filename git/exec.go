package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Trace, if set, is invoked after every git invocation. slopball wires it to
// the debug logger (see internal/cli) so a SLOPBALL_LOG=debug run shows every git
// command the host/join loops fire, how long it took, and whether it failed —
// the single most useful thing when a sync/merge misbehaves. Left nil in tests
// and normal runs so there is zero overhead when tracing is off.
var Trace func(dir string, args []string, dur time.Duration, err error)

// hermeticConfig are the -c key=value flags passed on every invocation so a
// hostile ~/.gitconfig (custom merge drivers, rerere, autocrlf, …) cannot
// change merge results. Fixed merge/diff/line-ending behavior = determinism.
var hermeticConfig = []string{
	"core.autocrlf=false",
	"core.safecrlf=false",
	"core.eol=lf",
	"core.filemode=true",
	"merge.conflictstyle=merge",
	"merge.ff=false",
	"rerere.enabled=false",
	"rerere.autoupdate=false",
	"advice.detachedHead=false",
	"init.defaultBranch=main",
}

// HermeticConfig returns the -c key=value pairs applied on every invocation.
func HermeticConfig() []string {
	out := make([]string, len(hermeticConfig))
	copy(out, hermeticConfig)
	return out
}

// Env builds the controlled environment for a bundled-git invocation.
// User global/system configs are disabled, and identity defaults to
// HermeticIdentity — a caller that supplies its own (SessionIdentity) passes it
// in `extra`, which is appended LAST and therefore wins, since the final
// occurrence of a variable is the one exec applies.
//
// Identity is in the base for the same reason the config files are excluded:
// without it git does not fail, it guesses from the unix account and GECOS, so
// the author of a commit varied by machine and was unavailable entirely on a
// runner with no GECOS entry.
func Env(extra ...string) []string {
	env := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_AUTHOR_NAME=" + HermeticIdentity.Name,
		"GIT_AUTHOR_EMAIL=" + HermeticIdentity.Email,
		"GIT_COMMITTER_NAME=" + HermeticIdentity.Name,
		"GIT_COMMITTER_EMAIL=" + HermeticIdentity.Email,
		"LANG=C",
		"LC_ALL=C",
		// Keep PATH minimal — the launcher must not find a system git helper
		// that disagrees with our pin. cmd/git sets its own exec-path.
		"PATH=/usr/bin:/bin",
	}
	// Preserve HOME only so the launcher can resolve relative paths if needed;
	// global config is already nulled out above.
	if h := os.Getenv("HOME"); h != "" {
		env = append(env, "HOME="+h)
	}
	if p := os.Getenv("TMPDIR"); p != "" {
		env = append(env, "TMPDIR="+p)
	}
	return append(env, extra...)
}

// Cmd is a hermetic git invocation rooted at Dir (optional). All git traffic
// in slopball must go through Cmd — never exec.Command("git", …).
type Cmd struct {
	Dir    string   // working tree / repo; passed as the process cwd
	Env    []string // additional env (e.g. GIT_AUTHOR_NAME); merged onto hermetic base
	Stdin  []byte
	Stdout *bytes.Buffer
	Stderr *bytes.Buffer
}

// Run executes `git <args…>` with hermetic -c flags and the bundled binary.
func (c *Cmd) Run(ctx context.Context, args ...string) error {
	bin, err := Bin()
	if err != nil {
		return err
	}
	full := make([]string, 0, len(hermeticConfig)*2+len(args))
	for _, kv := range hermeticConfig {
		full = append(full, "-c", kv)
	}
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, bin, full...)
	cmd.Dir = c.Dir
	cmd.Env = Env(c.Env...)
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	var stdout, stderr bytes.Buffer
	outW, errW := &stdout, &stderr
	if c.Stdout != nil {
		outW = c.Stdout
	}
	if c.Stderr != nil {
		errW = c.Stderr
	}
	cmd.Stdout = outW
	cmd.Stderr = errW

	var start time.Time
	if Trace != nil {
		start = time.Now()
	}
	runErr := cmd.Run()
	if Trace != nil {
		Trace(c.Dir, args, time.Since(start), runErr)
	}
	if runErr != nil {
		msg := strings.TrimSpace(errW.String())
		if msg == "" {
			msg = runErr.Error()
		}
		return fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return nil
}

// Output runs git and returns stdout.
func (c *Cmd) Output(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(ctx, args...); err != nil {
		return stdout.String(), err
	}
	return stdout.String(), nil
}

// Run is a convenience for one-shot invocations with no extra env.
func Run(ctx context.Context, dir string, args ...string) error {
	return (&Cmd{Dir: dir}).Run(ctx, args...)
}

// Output is a convenience for one-shot invocations returning stdout.
func Output(ctx context.Context, dir string, args ...string) (string, error) {
	return (&Cmd{Dir: dir}).Output(ctx, args...)
}
