package box

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SSHRunner executes commands on a remote box over `ssh`. Target is any value
// ssh accepts (user@host, a ~/.ssh/config alias, a Tailscale name). It shells
// out to the system ssh so it inherits the user's keys, agent, and config — the
// CLI never handles credentials itself.
type SSHRunner struct {
	Addr    string   // ssh target: user@host / alias
	Options []string // extra ssh -o options (e.g. "BatchMode=yes")
}

func (s *SSHRunner) Target() string { return s.Addr }

func (s *SSHRunner) args(extra ...string) []string {
	a := []string{}
	for _, o := range s.Options {
		a = append(a, "-o", o)
	}
	a = append(a, s.Addr)
	return append(a, extra...)
}

func (s *SSHRunner) Run(ctx context.Context, command string) (string, error) {
	// Run under sh -c on the far side so pipes/&&/cd behave as written.
	cmd := exec.CommandContext(ctx, "ssh", s.args("sh", "-c", shQuote(command))...)
	return combined(cmd, nil)
}

// Stream attaches to a remote command over ssh, piping local stdin/stdout/stderr
// so `box run` can stream npm/dev-server logs back to the laptop.
func (s *SSHRunner) Stream(ctx context.Context, command string, in io.Reader, out, errW io.Writer) error {
	cmd := exec.CommandContext(ctx, "ssh", s.args("sh", "-c", shQuote(command))...)
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = errW
	return cmd.Run()
}

func (s *SSHRunner) Put(ctx context.Context, data []byte, remotePath string) error {
	return s.PutMode(ctx, data, remotePath, 0o644)
}

func (s *SSHRunner) PutMode(ctx context.Context, data []byte, remotePath string, mode os.FileMode) error {
	dir := filepath.Dir(remotePath)
	mkdir := exec.CommandContext(ctx, "ssh", s.args("mkdir", "-p", shQuote(dir))...)
	if out, err := combined(mkdir, nil); err != nil {
		return fmt.Errorf("mkdir %s: %w\n%s", dir, err, out)
	}
	// Stream through stdin so the payload is never an ssh argv element. umask
	// makes the create mode tight before the data lands; chmod then sets the
	// exact mode the caller asked for (including executable bits for binaries).
	script := fmt.Sprintf("umask 077 && cat > %s && chmod %o %s",
		shSingle(remotePath), mode.Perm(), shSingle(remotePath))
	write := exec.CommandContext(ctx, "ssh", s.args("sh", "-c", shQuote(script))...)
	out, err := combined(write, data)
	if err != nil {
		return fmt.Errorf("write %s: %w\n%s", remotePath, err, out)
	}
	return nil
}

// LocalRunner executes on the machine slopball is running on — for provisioning a
// box you are already logged into, and for tests. No SSH involved.
type LocalRunner struct{}

func (LocalRunner) Target() string { return "localhost" }

func (LocalRunner) Run(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	return combined(cmd, nil)
}

func (LocalRunner) Stream(ctx context.Context, command string, in io.Reader, out, errW io.Writer) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = errW
	return cmd.Run()
}

func (r LocalRunner) Put(ctx context.Context, data []byte, remotePath string) error {
	return r.PutMode(ctx, data, remotePath, 0o755)
}

func (LocalRunner) PutMode(_ context.Context, data []byte, remotePath string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(remotePath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(remotePath, data, mode)
}

func combined(cmd *exec.Cmd, stdin []byte) (string, error) {
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// shQuote wraps a whole command for `ssh … sh -c <this>`, which os/exec passes as
// one argv element; single-quote it so the local shell doesn't touch it.
func shQuote(s string) string { return shSingle(s) }

func shSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
