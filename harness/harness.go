// Package harness drives conductor intelligence through AI coding-agent CLIs
// (Claude Code / Codex / Cursor) under a subscription login — never provider
// API tokens (MASTERPLAN §6.1 / §10, plans 06–07 / 21).
package harness

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Name is one supported coding-agent harness CLI.
type Name string

const (
	Claude Name = "claude"
	Codex  Name = "codex"
	Cursor Name = "cursor"
)

// Client shells out to a harness CLI for one-shot Complete turns.
// If CompleteFn is set (tests), it is used instead of exec.
type Client struct {
	Name  Name
	Model string
	Bin   string // resolved binary path or name

	// Dir is the working directory for Complete turns. Empty inherits the
	// slopball process's cwd, which is almost never what a caller wants: a
	// conductor started from ~/dev pointed the merger's harness at ~/dev, and a
	// CLI that gates on workspace trust then blocked on a directory unrelated to
	// the session. Callers should set this to the session work tree.
	Dir string

	// CompleteFn overrides CLI execution — the emulator / unit-test seam.
	CompleteFn func(ctx context.Context, system, user string) (string, error)

	// Run overrides process execution (defaults to exec.CommandContext).
	Run func(ctx context.Context, bin string, args []string, dir, stdin string) (stdout string, err error)

	// AgentFn overrides Agent entirely — the emulator / unit-test seam for the
	// agentic path, matching CompleteFn's role for text turns.
	AgentFn func(ctx context.Context, dir, prompt string, out io.Writer) error

	// OnEvent receives every decoded step of an agentic run as it happens. It is
	// how a caller reports *what* the agent is doing right now, as opposed to
	// the rendered lines it writes to out; the setup role turns it into the
	// dashboard's live activity.
	OnEvent func(Event)

	// RunAgent overrides agentic process execution (defaults to defaultRunAgent).
	RunAgent func(ctx context.Context, bin string, args []string, dir string, out io.Writer) error
}

// Fake returns a Client that never shells out — used by large emulator tests.
func Fake(fn func(ctx context.Context, system, user string) (string, error)) *Client {
	return &Client{Name: "fake", Bin: "fake", CompleteFn: fn}
}

// Lookup resolves a harness by name. Returns nil when the name is empty or the
// binary is missing (caller falls back to mechanical merge/fix). The session
// record / wizard / flags supply the name — there is no environment override.
func Lookup(name, model string) *Client {
	n := Name(strings.ToLower(strings.TrimSpace(name)))
	if n == "" {
		return nil
	}
	bin := defaultBin(n)
	if bin == "" {
		return nil
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil
	}
	return &Client{Name: n, Model: model, Bin: path}
}

// FirstAvailable returns this machine's first installed harness CLI, or nil.
// Used only as a Fallback for roles the session never named — never as a
// silent substitute for a harness the session chose that is missing here.
func FirstAvailable() *Client {
	for _, a := range Available() {
		if a.Present {
			return Lookup(string(a.Name), "")
		}
	}
	return nil
}

// Availability reports whether one harness CLI is installed on this machine.
type Availability struct {
	Name    Name
	Bin     string // resolved path when Present
	Present bool
}

// All is every harness slopball knows how to drive, in the order the first-run
// wizard offers them.
var All = []Name{Claude, Codex, Cursor}

// Available reports which harness CLIs are on PATH. The first-run wizard (plan
// 29) offers only what is installed and refuses a pick that is not — a session
// that asked for an agent must never be silently downgraded to mechanical
// merging.
func Available() []Availability {
	out := make([]Availability, 0, len(All))
	for _, n := range All {
		a := Availability{Name: n}
		if bin := defaultBin(n); bin != "" {
			if path, err := exec.LookPath(bin); err == nil {
				a.Bin, a.Present = path, true
			}
		}
		out = append(out, a)
	}
	return out
}

// IsAvailable reports whether one named harness is installed.
func IsAvailable(name string) bool {
	want := Name(strings.ToLower(strings.TrimSpace(name)))
	for _, a := range Available() {
		if a.Name == want {
			return a.Present
		}
	}
	return false
}

// SuggestedModels are hints shown in the model prompt — never validation. A
// hardcoded allow-list would start rejecting valid models the day a CLI ships
// a new one, so blank (the CLI's own default) always stays legal and anything
// typed is passed straight through.
func SuggestedModels(n Name) []string {
	switch Name(strings.ToLower(string(n))) {
	case Claude:
		return []string{"opus", "sonnet", "haiku"}
	case Codex:
		return []string{"gpt-5-codex", "gpt-5"}
	case Cursor:
		return []string{"auto", "sonnet-4.5", "gpt-5"}
	default:
		return nil
	}
}

func defaultBin(n Name) string {
	switch n {
	case Claude:
		return "claude"
	case Codex:
		return "codex"
	case Cursor:
		// Cursor's non-interactive agent CLI is `agent` (cursor-agent legacy).
		if _, err := exec.LookPath("agent"); err == nil {
			return "agent"
		}
		return "cursor-agent"
	default:
		return ""
	}
}

// Complete runs one system+user turn and returns the model's text.
func (c *Client) Complete(ctx context.Context, system, user string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("harness: nil client")
	}
	if c.CompleteFn != nil {
		return c.CompleteFn(ctx, system, user)
	}
	args, stdin, err := c.buildArgs(system, user)
	if err != nil {
		return "", err
	}
	run := c.Run
	if run == nil {
		run = defaultRun
	}
	out, err := run(ctx, c.Bin, args, c.Dir, stdin)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (c *Client) buildArgs(system, user string) (args []string, stdin string, err error) {
	switch c.Name {
	case Claude:
		args = []string{
			"-p",
			"--output-format", "text",
			"--system-prompt", system,
			"--dangerously-skip-permissions",
		}
		if c.Model != "" {
			args = append(args, "--model", c.Model)
		}
		args = append(args, user)
		return args, "", nil
	case Cursor:
		// --force alongside --mode ask: ask keeps the turn read-only (the caller
		// writes the resolved file from the returned text), while --force is the
		// only way past the workspace-trust prompt a non-interactive run can
		// never answer.
		args = []string{"-p", "--mode", "ask", "--force", "--output-format", "text"}
		if c.Model != "" {
			args = append(args, "--model", c.Model)
		}
		args = append(args, user)
		// Cursor has no separate system-prompt flag; fold it into the user turn.
		if system != "" {
			args[len(args)-1] = system + "\n\n" + user
		}
		return args, "", nil
	case Codex:
		// codex exec: non-interactive one-shot. Prompt on stdin keeps argv short.
		args = []string{"exec", "--skip-git-repo-check"}
		if c.Model != "" {
			args = append(args, "--model", c.Model)
		}
		args = append(args, "-")
		stdin = system + "\n\n" + user
		return args, stdin, nil
	case "fake":
		return nil, "", fmt.Errorf("harness fake client requires CompleteFn")
	default:
		return nil, "", fmt.Errorf("harness: unknown name %q", c.Name)
	}
}

// Agent runs the harness CLI *agentically* in dir: the model gets its own
// tools and writes files directly, and whatever tree it leaves behind is the
// result (plan 28's setup role — a text turn cannot run `create-next-app`).
// Output streams to out as it arrives, because these runs take minutes.
//
// This is additive: Complete and CompleteFn-backed fakes are untouched.
func (c *Client) Agent(ctx context.Context, dir, prompt string, out io.Writer) error {
	if c == nil {
		return fmt.Errorf("harness: nil client")
	}
	if c.AgentFn != nil {
		return c.AgentFn(ctx, dir, prompt, out)
	}
	args, err := c.agentArgs(prompt)
	if err != nil {
		return err
	}
	run := c.RunAgent
	if run == nil {
		run = defaultRunAgent
	}
	if out == nil {
		out = io.Discard
	}
	// The CLI's raw stream is decoded on the way through rather than after the
	// fact: `out` is what a human watches, and a run that reports only on exit
	// is the hang this whole path exists to end.
	dec := newStreamDecoder(c.Name, func(e Event) {
		if c.OnEvent != nil {
			c.OnEvent(e)
		}
		_, _ = io.WriteString(out, e.Line()+"\n")
	})
	err = run(ctx, c.Bin, args, dir, dec)
	dec.Close()
	return err
}

// agentArgs builds the write-capable, non-interactive argv per CLI. These flags
// differ per harness and move between releases — the opt-in real-CLI test
// (SLOPBALL_REAL_HARNESS=1) is what keeps them honest.
func (c *Client) agentArgs(prompt string) ([]string, error) {
	var args []string
	switch c.Name {
	case Claude:
		// --verbose is load-bearing, not noise: stream-json without it emits one
		// final result frame, which is the same silence as `--output-format text`.
		args = []string{"-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions"}
	case Codex:
		// exec = non-interactive; --full-auto = write + run without approvals.
		// It already streams plain text, so there is no format to ask for.
		args = []string{"exec", "--full-auto", "--skip-git-repo-check"}
	case Cursor:
		// Deliberately NOT `--mode ask`: that is cursor's read-only mode, which
		// is what the text path uses. --force bypasses the approval prompts a
		// non-interactive run can never answer.
		args = []string{"-p", "--output-format", "stream-json", "--force"}
	case "fake":
		return nil, fmt.Errorf("harness fake client requires AgentFn")
	default:
		return nil, fmt.Errorf("harness: unknown name %q", c.Name)
	}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	return append(args, prompt), nil
}

// defaultRunAgent execs the CLI in dir with stdout+stderr streaming to out.
// Unlike defaultRun it sets cmd.Dir — the whole point of the agentic path — and
// never buffers, so a long scaffold visibly makes progress.
func defaultRunAgent(ctx context.Context, bin string, args []string, dir string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("harness %s: %w", bin, err)
	}
	return nil
}

func defaultRun(ctx context.Context, bin string, args []string, dir, stdin string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("harness %s: %s", bin, msg)
	}
	return stdout.String(), nil
}
