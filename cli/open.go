package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nwylynko/slopball-cli/session"
	"github.com/spf13/cobra"
)

func newOpenCmd() *cobra.Command {
	open := &cobra.Command{
		Use:     "open [pin]",
		Aliases: []string{"cd", "workspace"},
		Short:   "Enter a session work tree in a subshell (exit to leave)",
		Long: "Opens your $SHELL in the session work tree; exit returns to this\n" +
			"shell. One live session is picked automatically; several show a menu.\n" +
			"Pass a PIN to open that session from local disk. Use --print to emit only\n" +
			"the work path for scripts.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			printPath, _ := cmd.Flags().GetBool("print")
			return runOpen(cmd, args, printPath)
		},
	}
	open.Flags().Bool("print", false, "print the work path and exit (no subshell)")
	return open
}

func runOpen(cmd *cobra.Command, args []string, printPath bool) error {
	out := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()
	pin, err := resolveOpenPIN(cmd, args)
	if err != nil {
		return err
	}
	workPath := workDir(session.ForPin(pin))
	if cwd, _ := os.Getwd(); cwd != "" && alreadyInSessionWork(pin, workPath, cwd) {
		fmt.Fprintf(errW, "already in %s work tree\n", pin)
		if printPath || !openTTY(cmd) {
			fmt.Fprintln(out, workPath)
		}
		return nil
	}
	if !printPath && openTTY(cmd) {
		fmt.Fprintf(errW, "session %s — exit to leave\n", pin)
		return spawnShell(cmd, pin, workPath)
	}
	fmt.Fprintln(out, workPath)
	return nil
}

func resolveOpenPIN(cmd *cobra.Command, args []string) (string, error) {
	errW := cmd.ErrOrStderr()
	if len(args) == 1 {
		pin := args[0]
		// Check liveness BEFORE the metadata: a session still standing up has a
		// live marker but has not saved session.json yet, and reporting "no
		// local session" for one that a process on this machine is actively
		// building is the lie this whole path exists to stop telling.
		if err := awaitWorkTree(cmd, pin); err != nil {
			return "", err
		}
		if _, err := session.Load(pin); err != nil {
			return "", fmt.Errorf("no local session %s — run slopball to host or slopball join %s", pin, pin)
		}
		live := livePinSet()
		if !live[pin] {
			fmt.Fprintf(errW, "session %s is not live — mirror may be stale\n", pin)
		}
		return pin, nil
	}
	entries, err := session.LivePins()
	if err != nil {
		return "", err
	}
	switch len(entries) {
	case 0:
		return "", fmt.Errorf("no live session on this machine — run slopball to host or slopball join <pin>")
	case 1:
		pin := entries[0].PIN
		if err := awaitWorkTree(cmd, pin); err != nil {
			return "", err
		}
		fmt.Fprintf(errW, "opening %s → %s\n", pin, workDir(session.ForPin(pin)))
		return pin, nil
	default:
		if !openTTY(cmd) {
			return "", fmt.Errorf("%d live sessions — pass slopball open <pin> or run on a terminal", len(entries))
		}
		pin, err := PickLiveSession(cmd, entries)
		if err != nil {
			return "", err
		}
		// A session still standing up is on the menu — it is live, that is the
		// point — so picking it has to wait exactly like the other two branches.
		if err := awaitWorkTree(cmd, pin); err != nil {
			return "", err
		}
		return pin, nil
	}
}

// awaitWorkTree holds until pin's work tree is usable, and says so while it
// waits. A session that is already up costs one file read and no message.
//
// This is the whole of TASK-13's answer, and it sits in resolveOpenPIN because
// that is the single chokepoint every session-entering verb shares — open, cd,
// workspace, claude and codex — so none of them can grow its own opinion about
// what a half-built session is.
//
// Waiting, not reporting, is the deliberate choice: the alternative told a
// human to run the same command again in a few seconds, and told an agent —
// which cannot judge "a few seconds" — that the session did not exist.
func awaitWorkTree(cmd *cobra.Command, pin string) error {
	if !session.StandingUp(pin) {
		return nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "session %s is still standing up — waiting for its work tree\n", pin)
	return session.WaitUntilReady(cmd.Context(), pin)
}

func livePinSet() map[string]bool {
	entries, err := session.LivePins()
	if err != nil {
		return nil
	}
	m := make(map[string]bool, len(entries))
	for _, e := range entries {
		m[e.PIN] = true
	}
	return m
}

// PickLiveSession is the menu `slopball open` shows when this machine holds
// more than one live session, and the answer it accepts.
//
// Exported because it only ever runs on a terminal — resolveOpenPIN reaches it
// behind an isCharDevice check on both stdin and stdout — so the one way to
// drive it with a scripted answer is to call it, short of taking a pty
// dependency for a menu.
func PickLiveSession(cmd *cobra.Command, entries []session.LiveEntry) (string, error) {
	errW := cmd.ErrOrStderr()
	for i, e := range entries {
		fmt.Fprintf(errW, "[%d] %s — %s — %s\n", i+1, e.PIN, e.Live.Role, pickerBranch(e))
	}
	fmt.Fprint(errW, "pick > ")
	sc := bufio.NewScanner(cmd.InOrStdin())
	if !sc.Scan() {
		return "", fmt.Errorf("no selection")
	}
	ans := strings.TrimSpace(sc.Text())
	n, err := strconv.Atoi(ans)
	if err != nil || n < 1 || n > len(entries) {
		return "", fmt.Errorf("invalid pick %q", ans)
	}
	pin := entries[n-1].PIN
	fmt.Fprintf(errW, "opening %s → %s\n", pin, workDir(session.ForPin(pin)))
	return pin, nil
}

func pickerBranch(e session.LiveEntry) string {
	if e.Live.Branch != "" {
		return e.Live.Branch
	}
	if s, err := session.Load(e.PIN); err == nil && s.Branch != "" {
		return session.BranchLabel(s.Branch)
	}
	return "-"
}

// alreadyInSessionWork is true when dir sits inside this session's work tree.
// pinFromCwd reuses the same sessions-layout logic as sync/push/pull.
func alreadyInSessionWork(pin, workPath, dir string) bool {
	if pinFromCwd() != pin {
		return false
	}
	return dirInside(resolvePath(workPath), resolvePath(dir))
}

func dirInside(base, dir string) bool {
	rel, err := filepath.Rel(base, dir)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func openTTY(cmd *cobra.Command) bool {
	in, _ := cmd.InOrStdin().(*os.File)
	out, _ := cmd.OutOrStdout().(*os.File)
	return isCharDevice(in) && isCharDevice(out)
}

// spawnShell opens $SHELL in the session work tree.
func spawnShell(cmd *cobra.Command, pin, workPath string) error {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	return spawnInSessionWork(cmd, pin, workPath, shell, nil)
}

// spawnInSessionWork runs bin in the session work tree with `slopball` on PATH,
// whatever the running binary is called on disk — an installed release is named
// slopball-<os>-<arch>, and an agent told to run `slopball sync` in that
// subshell has to find something.
//
// It is what both doors into a session share: `open`'s subshell and the
// `slopball claude` / `slopball codex` verbs, which are the same act with the
// agent CLI in the shell's place.
func spawnInSessionWork(cmd *cobra.Command, pin, workPath, bin string, args []string) error {
	env := append(os.Environ(), "SLOPBALL_PIN="+pin)
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if filepath.Base(exe) != "slopball" {
		binDir, err := os.MkdirTemp("", "slopball-open-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(binDir)
		link := filepath.Join(binDir, "slopball")
		if err := os.Symlink(exe, link); err != nil {
			script := "#!/bin/sh\nexec " + strconv.Quote(exe) + " \"$@\"\n"
			if err := os.WriteFile(link, []byte(script), 0o755); err != nil {
				return err
			}
		}
		env = append(env, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	c := exec.Command(bin, args...)
	c.Dir = workPath
	c.Stdin = cmd.InOrStdin()
	c.Stdout = cmd.OutOrStdout()
	c.Stderr = cmd.ErrOrStderr()
	c.Env = env
	return c.Run()
}
