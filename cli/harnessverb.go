package cli

import (
	"fmt"

	"github.com/nwylynko/slopball-cli/harness"
	"github.com/nwylynko/slopball-cli/session"
	"github.com/spf13/cobra"
)

// newHarnessVerbCmd builds `slopball claude` / `slopball codex`: the one-word
// form of `slopball open && claude`. It resolves the session exactly as `open`
// does and starts the agent CLI in that session's work tree, with the same
// session environment the subshell gets — $SLOPBALL_PIN and a runnable
// `slopball`.
//
// It exists because the two commands are what a human types every time and the
// pair is the whole ceremony of joining an agent to a session; a verb that
// forgets neither the directory nor the PIN is one less way to end up editing
// the wrong tree.
//
// The agent CLI is NOT the conductor's harness (internal/harness drives that
// non-interactively). This is the interactive one a human sits in front of —
// the same binaries, resolved the same way, which is why it reuses
// harness.Available rather than keeping a second table of CLI names.
func newHarnessVerbCmd(n harness.Name) *cobra.Command {
	verb := string(n)
	c := &cobra.Command{
		Use:   verb + " [pin] [-- args...]",
		Short: "Start " + verb + " in the session work tree (`slopball open` + " + verb + " in one verb)",
		Long: "Shorthand for `slopball open` followed by `" + verb + "`: picks the live session\n" +
			"the way `open` does, then runs " + verb + " in its work tree with $SLOPBALL_PIN set\n" +
			"and slopball on PATH. Pass a PIN to choose a session, and put anything for\n" +
			verb + " itself after `--` so its flags never collide with slopball's.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHarnessVerb(cmd, n, args)
		},
	}
	return c
}

func runHarnessVerb(cmd *cobra.Command, n harness.Name, args []string) error {
	pinArgs, passthrough := splitAtDash(cmd, args)
	if len(pinArgs) > 1 {
		return fmt.Errorf("%s takes at most a PIN — put %s's own arguments after `--`", n, n)
	}
	bin, ok := harnessBin(n)
	if !ok {
		return fmt.Errorf("%s is not installed on this machine — install its CLI and run `slopball %s` again (`slopball open` gives you the session work tree without it)", n, n)
	}
	pin, err := resolveOpenPIN(cmd, pinArgs)
	if err != nil {
		return err
	}
	workPath := workDir(session.ForPin(pin))
	fmt.Fprintf(cmd.ErrOrStderr(), "session %s — running %s in %s\n", pin, n, workPath)
	return spawnInSessionWork(cmd, pin, workPath, bin, passthrough)
}

// splitAtDash divides args at the `--` cobra swallowed: before it is slopball's
// (the optional PIN), after it is the agent CLI's.
func splitAtDash(cmd *cobra.Command, args []string) (mine, theirs []string) {
	at := cmd.ArgsLenAtDash()
	if at < 0 {
		return args, nil
	}
	return args[:at], args[at:]
}

// harnessBin resolves the CLI binary for a harness on this machine, reusing the
// same lookup the wizard offers from — cursor's is `agent`/`cursor-agent`, and
// a second table of names here would be the place they disagree.
func harnessBin(n harness.Name) (string, bool) {
	for _, a := range harness.Available() {
		if a.Name == n {
			return a.Bin, a.Present
		}
	}
	return "", false
}
