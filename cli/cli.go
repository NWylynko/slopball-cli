// Package cli defines slopball's command surface. slopball is mostly a CLI, but
// its primary users are the *agents*, not the humans (MASTERPLAN §13) — so the
// output is kept machine-legible and the human-facing verbs are deliberately few
// (`slopball` to host, `slopball join` to join); the rest are driven by the
// in-repo agent contract.
package cli

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/slopball-cli/controlplane"
	sbGit "github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/logx"
	"github.com/spf13/cobra"
)

var traceOnce sync.Once

// setupTracing wires every git invocation into the debug logger, so a
// SLOPBALL_LOG=debug run shows the full git conversation the host/join loops
// have with the session. No-op unless debug is enabled, and installed at most once.
func setupTracing() {
	traceOnce.Do(func() {
		gitLog := logx.New("git")
		sbGit.Trace = func(dir string, args []string, dur time.Duration, err error) {
			if !logx.DebugEnabled() {
				return
			}
			where := dir
			if where == "" {
				where = "-"
			}
			if err != nil {
				gitLog.Debugf("%s  (%s, %s) FAILED: %v", argline(args), where, dur.Round(time.Millisecond), err)
				return
			}
			gitLog.Debugf("%s  (%s, %s)", argline(args), where, dur.Round(time.Millisecond))
		}
	})
}

func argline(args []string) string {
	const max = 8
	if len(args) <= max {
		return "git " + join(args)
	}
	return "git " + join(args[:max]) + " …"
}

func join(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		// Keep each trace to one line — commit messages carry newlines.
		out += strings.ReplaceAll(a, "\n", "⏎")
	}
	return out
}

// Version is the build version, overridden at link time via
// -ldflags "-X github.com/nwylynko/slopball/internal/cli.Version=<v>".
var Version = "0.0.0-dev"

// controlFlag is an optional --control override (tests). Empty → BaseURL().
var controlFlag string

// stub returns a RunE that prints a uniform "not implemented" line naming the
// plan that will implement the verb, and succeeds.
func stub(plan string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		fmt.Fprintf(cmd.OutOrStdout(), "%s: not implemented yet — see %s\n", cmd.CommandPath(), plan)
		return nil
	}
}

// NewRootCmd builds the full command tree.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "slopball",
		Short: "Coordinate many AI agents building one live product",
		Long: "slopball lets multiple AI agents dump code into one project at once and keeps the\n" +
			"result coherent and runnable. Run with no arguments to host a session, or\n" +
			"`slopball join <pin>` to join one. Most verbs are invoked by your agent (via the\n" +
			"contract slopball installs into the repo), not by you.",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runHost,
	}
	addHostFlags(root)
	// First-run only (plan 29). These live on `root` and deliberately NOT on
	// `_host`: the wizard is a human entry point, and a booting box container
	// that stopped to ask a question would be a hung provision.
	root.Flags().Bool("interactive", false, "ask the first-run questions (default: auto — only on a terminal, and never with --once)")
	root.Flags().Bool("box", false, "run this session on a box the control plane provisions (no ssh target, no docker here)")
	root.Flags().String("box-ssh", "", "BYO: provision the box on a machine you own — <user@host>, or `local` for this machine")
	root.Flags().String("agent-merger", "", "agent for the merger role as harness[:model] (overrides --conductor)")
	root.Flags().String("agent-watcher", "", "agent for the error-watcher role as harness[:model] (overrides --conductor)")
	root.Flags().String("agent-setup", "", "agent for the setup role as harness[:model] (overrides --conductor)")
	root.Flags().String("mirror", "", "git URL for the durability snapshot (plan 15); the token is read from $GITHUB_TOKEN/$GH_TOKEN here and never published")
	root.PersistentFlags().StringVar(&controlFlag, "control", "", "control plane URL (default: "+controlplane.DefaultURL+")")

	// The machine-to-machine door into the same host loop. A PIN names a session
	// that already exists, so only a machine resuming/claiming one may pass it —
	// `box add` boots the container with `_host --pin <pin>` (plan 27). Humans
	// create with plain `slopball` and join with `slopball join <pin>`.
	hostCmd := &cobra.Command{
		Use:    "_host",
		Short:  "Host a session under a given PIN (internal: used by `box add`)",
		Hidden: true,
		RunE:   runHost,
	}
	addHostFlags(hostCmd)
	hostCmd.Flags().String("pin", "", "session PIN to host (≥6 alphanumeric)")
	// Hidden even here because a human typing it on a laptop is how you steal a
	// live PIN from a working box — the demotion default (plan 24) stops that.
	hostCmd.Flags().Bool("takeover", false, "claim this PIN even if the session is hosted elsewhere")
	_ = hostCmd.Flags().MarkHidden("takeover")

	join := &cobra.Command{
		Use:   "join <pin>",
		Short: "Join a session by PIN and bootstrap this laptop's agent into the game",
		Args:  cobra.ExactArgs(1),
		RunE:  runJoin,
	}
	join.Flags().String("name", "", "client name used for the branch (default: agent)")
	addConsoleFlag(join)

	syncCmd := &cobra.Command{
		Use:   "sync",
		Short: "Pull + integrate the latest main, then push your finished work (run at each task boundary)",
		RunE:  runSync,
	}
	addPinFlag(syncCmd)
	addIntentFlag(syncCmd)

	push := &cobra.Command{
		Use:   "push",
		Short: "Push your finished work up to the merger (internal half of sync)",
		RunE:  runPush,
	}
	addPinFlag(push)
	addIntentFlag(push)

	pull := &cobra.Command{
		Use:   "pull",
		Short: "Pull + integrate the latest main into your branch (internal half of sync)",
		RunE:  runPull,
	}
	addPinFlag(pull)

	repoint := &cobra.Command{
		Use:   "repoint",
		Short: "Retarget this client's origin at a relocated canonical (auto on sync; use for forced moves)",
		RunE:  runRepoint,
	}
	repoint.Flags().String("to", "", "git URL to point origin at (default: resolve PIN via control plane)")
	addPinFlag(repoint)

	runCmd := &cobra.Command{
		Use:   "run <cmd>...",
		Short: "Run a command against the host terminal (dev / migrate / seed / …)",
		Args:  cobra.MinimumNArgs(1),
		RunE:  runRun,
	}
	addPinFlag(runCmd)

	conductorCmd := &cobra.Command{
		Use:    "conductor",
		Short:  "Run the conductor fleet against the session's canonical (internal)",
		Hidden: true,
		RunE:   runConductor,
	}
	conductorCmd.Flags().Bool("once", false, "tick the fleet once and exit")
	conductorCmd.Flags().String("remote", "", "git URL of a remote canonical (cloud box) to drive; empty = local on-disk canonical")
	addPinFlag(conductorCmd)
	daemon := &cobra.Command{
		Use:    "_daemon",
		Short:  "The long-running join daemon: keep the local main mirror fresh (internal)",
		Hidden: true,
		RunE:   runDaemon,
	}
	daemon.Flags().Bool("once", false, "refresh the mirror once and exit")
	addPinFlag(daemon)

	elect := &cobra.Command{
		Use:   "elect",
		Short: "Elect this laptop's harness CLI as the conductor runner for a remote/cloud box",
		Long: "For a cloud box someone else owns, elect this machine as the conductor runner.\n" +
			"AI roles shell out to your local harness CLI (Claude Code / Codex / Cursor) under\n" +
			"your subscription — login never lands on the box. Local/mesh hosts do not need election.",
		RunE: runElect,
	}
	elect.Flags().String("name", "", "elector name (default: session branch or $USER)")
	elect.Flags().String("harness", "", "harness CLI: claude|codex|cursor (default: first installed)")
	elect.Flags().String("model", "", "optional model override")
	elect.Flags().Bool("revoke", false, "revoke the current harness-runner election")
	elect.Flags().Bool("self", false, "self-elect as local conductor (no remote box; host-leave path)")
	elect.Flags().String("box", "", "remote box work root to install the runner marker into")
	addPinFlag(elect)

	// The manual half of automatic placement (plan 30). Automatic covers the
	// case where nobody is around; these cover the case where somebody is.
	take := &cobra.Command{
		Use:   "take <git|dev|conductor|all>",
		Short: "Run a session service on this machine (refused while another member is serving it)",
		Long: "Claim a service lease for this machine. Services are placed automatically —\n" +
			"this is the override, and the way a returning creator picks its services back up.\n" +
			"A service another member is actively serving is NOT taken: hand it off from there,\n" +
			"or use --force only for an owner that has wedged.",
		Args: cobra.ExactArgs(1),
		RunE: runTake,
	}
	take.Flags().Bool("force", false, "take a service from a LIVE owner (wedged-owner escape hatch)")
	addPinFlag(take)

	handOff := &cobra.Command{
		Use:   "hand-off <git|dev|conductor|all>",
		Short: "Give a session service up, so another member takes it over",
		Args:  cobra.ExactArgs(1),
		RunE:  runHandOff,
	}
	handOff.Flags().String("to", "", "member to hand it to (default: whoever ranks best)")
	addPinFlag(handOff)

	services := &cobra.Command{
		Use:   "services",
		Short: "Show where each session service is running right now",
		RunE:  runServices,
	}
	addPinFlag(services)

	mon := &cobra.Command{
		Use:   "monitor",
		Short: "Live read-only view of a session: members, endpoints, convergence",
		RunE:  runMonitor,
	}
	mon.Flags().Bool("once", false, "print one snapshot and exit (scripting / CI)")
	mon.Flags().Bool("json", false, "emit the resolved session as JSON from the control plane")
	mon.Flags().Bool("local", false, "read local disk only (skip control plane)")
	mon.Flags().Duration("interval", 2*time.Second, "refresh interval")
	addPinFlag(mon)

	// `control` and `relay` are NOT verbs here. They are operator services
	// shipped as `slopball-control` and `slopball-relay` (plan 39): different
	// audience, different release cadence, linux-only. Keeping them on this
	// binary is what made every teammate's laptop link a postgres driver.
	root.AddCommand(hostCmd, join, syncCmd, push, pull, repoint, runCmd, conductorCmd, daemon, elect, take, handOff, services, newMembersCmd(), newAccessCmd(), newTelemetryCmd(), newReportCmd(), mon, newBoxCmd(), newOpenCmd(), newSiteCmd(), newDevSetupCmd())
	return root
}

// addHostFlags defines the flags shared by the human create verb (`slopball`)
// and the internal boot verb (`_host`), so the two cannot drift. Only `_host`
// adds --pin/--takeover on top.
func addHostFlags(cmd *cobra.Command) {
	cmd.Flags().String("brief", "", "one line saying what we're building (e.g. \"reactjs vite with tailwind\"); the setup role scaffolds it and every joined agent's contract quotes it")
	cmd.Flags().String("seed", "", "optional local directory to seed canonical from")
	cmd.Flags().String("seed-url", "", "optional git remote URL to seed canonical from")
	cmd.Flags().String("conductor", "", "harness CLI for the conductor fleet: claude|codex|cursor")
	cmd.Flags().String("model", "", "optional model override for the harness CLI")
	cmd.Flags().Bool("once", false, "start session, print join info, and exit (tests / scripting)")
	cmd.Flags().Bool("serve-only", false, "cloud-box mode: serve canonical + track main, but defer merging to the elected conductor (no local harness)")
	cmd.Flags().String("dev", "", "dev-server command to supervise on canonical main (e.g. \"npm run dev\"); its logs are served at /logs for the error-watcher")
	cmd.Flags().String("install", "", "dependency-install command to run in canonical/work before the supervised --dev process (e.g. \"npm install\"); empty with --dev auto-detects from lockfiles")
	addConsoleFlag(cmd)
}

// addConsoleFlag opts a long-running verb out of the session console, back to
// the line log it has always printed. Automatic when stdout is not a terminal —
// the flag is for a terminal you want the plain output on anyway.
func addConsoleFlag(cmd *cobra.Command) {
	cmd.Flags().Bool("no-console", false, "print the plain line log instead of the live session console")
}

// newBoxCmd builds the `box` command group: provision and drive a cloud dev box
// (a Docker container on a remote machine) over ssh.
func newBoxCmd() *cobra.Command {
	boxCmd := &cobra.Command{
		Use:   "box",
		Short: "Provision and drive the session's cloud dev box",
	}

	add := &cobra.Command{
		Use:   "add <user@host>",
		Short: "Provision a container on the target, seed the session, and start hosting on it",
		Long: "Default: docker pull the published multi-arch image matching this CLI's version\n" +
			"(ghcr.io/nwylynko/slopball-box:<version>, or --image) and run it.\n" +
			"No binary is shipped and nothing is built on the box.\n\n" +
			"No registry needed: if the pull fails but the box already holds that image,\n" +
			"the box boots it with a warning. Build it there with `make box-image` — the\n" +
			"registry-less path while CI has published no tag.\n\n" +
			"For developing slopball itself or air-gapped boxes, pass --build-local\n" +
			"--binary <linux/amd64 path> to ship + docker build on the target (the old\n" +
			"plan-14 path). Private registries: `docker login` on the box first\n" +
			"(slopball never ships registry credentials).",
		RunE: runBoxAdd,
	}
	add.Flags().Bool("build-local", false, "build the image on the box from a shipped binary instead of pulling a published image (for developing slopball / registry-less boxes)")
	add.Flags().String("binary", "", "path to the slopball linux/amd64 binary to ship (required with --build-local; ignored on the pull path)")
	add.Flags().String("image", "", "image ref to pull (default: ghcr.io/nwylynko/slopball-box:<CLI-version>); with --build-local, the local tag to build (default slopball-box)")
	add.Flags().String("pull", "always", "when to docker pull on the default path: always|missing")
	add.Flags().Bool("require-version-match", false, "error when the container's slopball --version differs from this CLI (default: warn)")
	add.Flags().String("seed", "", "local directory to seed the box's canonical from")
	add.Flags().String("seed-url", "", "override the git URL the box seeds its canonical from (default: the session's live canonical, resolved from the control plane)")
	add.Flags().Bool("serve-only", false, "box serves canonical + tracks main but runs no on-box merger")
	add.Flags().String("dev", "", "dev-server command the box supervises (e.g. \"npm run dev\"); its logs are served at /logs for the elected conductor's error-watcher")
	add.Flags().String("install", "", "dependency-install command the box runs in canonical/work before --dev (e.g. \"npm install\"); empty with --dev auto-detects from lockfiles")
	add.Flags().String("brief", "", "one line saying what we're building; the box records it on main and the setup role scaffolds from it")
	add.Flags().String("volume", "", "host path to mount at the box's $HOME/.slopball (persist across restarts)")
	add.Flags().Bool("local", false, "provision on this machine instead of over ssh (you are already on the box)")
	add.Flags().String("name", "", "container name (default slopball-<pin>)")
	add.Flags().Bool("drain", false, "quiesce expectation: catch-up main from --seed-url / current host before flipping rendezvous")
	add.Flags().Bool("no-cutover", false, "provision only — do not flip the session rendezvous onto the box")
	addPinFlag(add)

	runOnBox := &cobra.Command{
		Use:   "run <command>...",
		Short: "Run a command in the session box and stream its output",
		Long:  "Runs a command in the box work tree over the session network. Use `--` before the remote command when its flags would collide with this CLI.",
		RunE:  runSessionBoxRun,
	}
	addPinFlag(runOnBox)

	logs := &cobra.Command{
		Use:   "logs",
		Short: "Show the session box's supervised logs",
		Long:  "Fetches the published /logs stream (dev server + install) over the session network — not docker logs from this machine.",
		RunE:  runSessionBoxLogs,
	}
	addPinFlag(logs)

	rm := &cobra.Command{
		Use:   "rm",
		Short: "Remove the session's box",
		Long:  "Shuts the box down, destroys it, and clears the control-plane record. The session keeps running.",
		RunE:  runSessionBoxRm,
	}
	rm.Flags().Bool("yes", false, "remove without confirmation")
	addPinFlag(rm)

	boxCmd.AddCommand(add, runOnBox, logs, rm)
	return boxCmd
}

// rootPinGuidance returns the message to fail with when a human hands the root
// (create) verb a PIN. `slopball --pin x` used to mean create-or-resume-or-
// silently-demote depending on invisible on-disk state; a PIN now always means
// "join a session that exists" (plan 27). Returns "" for every other invocation
// — subcommands (`join`, `sync`, `_host`, …) take PINs as before.
func rootPinGuidance(root *cobra.Command, args []string) string {
	target, _, err := root.Find(args)
	if err != nil || target != root {
		return ""
	}
	pin := "<pin>"
	for i, a := range args {
		switch {
		case a == "--pin" && i+1 < len(args):
			pin = args[i+1]
		case strings.HasPrefix(a, "--pin="):
			pin = strings.TrimPrefix(a, "--pin=")
		default:
			continue
		}
		return fmt.Sprintf("a PIN identifies an existing session — join it with 'slopball join %s'. "+
			"'slopball' with no arguments starts a new one.", pin)
	}
	return ""
}

// Run executes slopball with the given args, writing to out/errW, and returns a
// process exit code.
func Run(args []string, out, errW io.Writer) int {
	setupTracing()
	root := NewRootCmd()
	if msg := rootPinGuidance(root, args); msg != "" {
		fmt.Fprintln(errW, "error:", msg)
		return 1
	}
	root.SetArgs(args)
	root.SetOut(out)
	root.SetErr(errW)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(errW, "error:", err)
		return 1
	}
	return 0
}
