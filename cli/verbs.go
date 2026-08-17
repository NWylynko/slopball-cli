package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nwylynko/slopball-cli/box"
	"github.com/nwylynko/slopball-cli/canonical"
	"github.com/nwylynko/slopball-cli/conductor"
	"github.com/nwylynko/slopball-cli/console"
	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/cutover"
	"github.com/nwylynko/slopball-cli/devserver"
	sbGit "github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/harness"
	"github.com/nwylynko/slopball-cli/hoststart"
	"github.com/nwylynko/slopball-cli/joindaemon"
	"github.com/nwylynko/slopball-cli/logx"
	"github.com/nwylynko/slopball-cli/monitor"
	"github.com/nwylynko/slopball-cli/session"
	"github.com/nwylynko/slopball-cli/syncengine"
	"github.com/nwylynko/slopball-cli/telemetry"
	"github.com/nwylynko/slopball-cli/trust"
	"github.com/spf13/cobra"
)

// sessionCtx resolves the active session and loads its metadata. The PIN is
// resolved in order of specificity: an explicit --pin, then $SLOPBALL_PIN, then
// the current working directory — because every work tree lives under
// <home>/sessions/<pin>/, just running a verb from inside the folder you're
// editing is enough (agents never have to thread --pin through every call).
func sessionCtx(cmd *cobra.Command) (session.Session, session.Paths, error) {
	pin := resolvePin(cmd)
	if pin == "" {
		return session.Session{}, session.Paths{}, fmt.Errorf("no session pin — run inside the session work tree, or pass --pin / set SLOPBALL_PIN")
	}
	s, err := session.Load(pin)
	if err != nil {
		return session.Session{}, session.Paths{}, fmt.Errorf("load session %s: %w", pin, err)
	}
	return s, session.ForPin(pin), nil
}

// resolvePin picks the session PIN from (most specific first) the --pin flag,
// $SLOPBALL_PIN, or the current working directory.
func resolvePin(cmd *cobra.Command) string {
	if cmd != nil {
		if pin, _ := cmd.Flags().GetString("pin"); pin != "" {
			return pin
		}
	}
	if pin := os.Getenv("SLOPBALL_PIN"); pin != "" {
		return pin
	}
	return pinFromCwd()
}

// pinFromCwd returns the PIN of the session whose tree contains the current
// directory, or "" if we're not inside one.
func pinFromCwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return pinUnderSessions(cwd, filepath.Join(session.Home(), "sessions"))
}

// pinUnderSessions extracts the PIN when dir sits inside <sessionsRoot>/<pin>/…
// (the layout session.ForPin lays down). Symlinks on either side are resolved so
// a session under a symlinked $SLOPBALL_HOME (or /tmp on macOS) still matches.
func pinUnderSessions(dir, sessionsRoot string) string {
	dir = resolvePath(dir)
	sessionsRoot = resolvePath(sessionsRoot)
	rel, err := filepath.Rel(sessionsRoot, dir)
	if err != nil {
		return ""
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	if i := strings.IndexRune(rel, filepath.Separator); i >= 0 {
		return rel[:i]
	}
	return rel
}

func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// workDir resolves this machine's working tree for a session — where the
// human/agent edits and what sync/push/pull operate on. A client's dev tree
// (sessions/<pin>/work) is preferred whenever it exists: a hosting laptop now
// gets one too (§4.4: host = client + box-operator), so this is where edits go
// even on a host. canonical/work is only a fallback for an older host that has
// no client tree — and it must not be edited directly (the loop hard-resets it).
func workDir(p session.Paths) string {
	if _, err := os.Stat(filepath.Join(p.Work, ".git")); err == nil {
		return p.Work
	}
	if st, err := os.Stat(p.Canonical); err == nil && st.IsDir() {
		cand := filepath.Join(p.Canonical, canonical.WorkDir)
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return p.Work
}

// mirrorDir returns the client's local main mirror if it exists, else "" so the
// caller falls back to fetching from origin. A host has no mirror/ — its
// canonical work tree's origin already is the session bare repo.
func mirrorDir(p session.Paths) string {
	if st, err := os.Stat(p.Mirror); err == nil && st.IsDir() {
		return p.Mirror
	}
	return ""
}

func addPinFlag(cmd *cobra.Command) {
	cmd.Flags().String("pin", "", "session PIN (or $SLOPBALL_PIN)")
}

func addIntentFlag(cmd *cobra.Command) {
	cmd.Flags().String("intent", "", "what changed and why (required for push/sync)")
}

func runHost(cmd *cobra.Command, _ []string) error {
	// The command's own context, not a fresh Background: a caller that can stop
	// this verb (a test, an embedding process) has no other way to, now that the
	// whole standup runs behind the console rather than in front of it.
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pin, _ := cmd.Flags().GetString("pin")
	seed, _ := cmd.Flags().GetString("seed")
	seedURL, _ := cmd.Flags().GetString("seed-url")
	cond, _ := cmd.Flags().GetString("conductor")
	modelStr, _ := cmd.Flags().GetString("model")
	once, _ := cmd.Flags().GetBool("once")
	serveOnly, _ := cmd.Flags().GetBool("serve-only")
	devCmd, _ := cmd.Flags().GetString("dev")
	installCmd, _ := cmd.Flags().GetString("install")
	takeover, _ := cmd.Flags().GetBool("takeover")

	// Seeds are checked before a single question is asked, let alone a container
	// created: `--seed <bad path>` used to surface minutes later as a dev server
	// with no package.json (plan 33).
	if err := checkSeedFlags(seed, seedURL); err != nil {
		return err
	}

	// The first run asks its questions BEFORE anything starts, so the session
	// comes up against one fully-resolved plan (plan 29). Non-interactive runs
	// resolve the same plan from flags alone and ask nothing, which is what
	// keeps every e2e/emulator/box-booted container unaffected.
	plan, wizard, err := resolveFirstRun(cmd, seedOrURL(seed, seedURL))
	if err != nil {
		return err
	}
	// A box target is dialed before a session directory exists: a typo found
	// after cutover is the expensive one.
	if plan.BoxTarget != "" {
		if err := preflightBox(ctx, cmd, plan.BoxTarget, cmd.OutOrStdout()); err != nil {
			return err
		}
	}
	warnMirrorCredential(cmd, plan)

	// Remote-first (plan 32): a named box IS the session's first host. Branch
	// before anything local starts, so there is no local host to move, no
	// takeover, no cutover and only ever one canonical.
	if plan.Box || plan.BoxTarget != "" {
		return standUpOnBox(ctx, cmd, plan, once)
	}

	startInstall, startDev := strings.Fields(installCmd), strings.Fields(devCmd)
	if wizard {
		// Nothing to install or serve yet — the project does not exist until the
		// setup role has run. startLocalDev picks these up afterwards, from the
		// commands recordRunCommands resolved against the scaffold.
		startInstall, startDev = nil, nil
	}
	// A fresh create leaves the PIN empty: the control plane mints it, and the
	// console draws with "minting session…" until Announce fills the join line
	// (abuse-surface ticket 11). `_host` is still handed one on its argv.
	ann, lv := newAnnouncer(), &leaver{}
	// Leaving and cleaning up are one act, and it is the host's own Close. The
	// standup that creates it runs behind the console, so it is registered from
	// in there rather than captured here.
	defer func() { _ = lv.Leave(context.Background()) }()

	// The host loop moves onto its own goroutine so the console can hold the
	// terminal (plan 36 §3). Without a terminal to draw on, runConsole calls
	// this straight through with the caller's own stdout, so `--once`, CI, the
	// emulator and piped output stay byte-identical to what they print today.
	work := func(ctx context.Context, out io.Writer) error {
		r, err := hoststart.Start(ctx, hoststart.Options{
			PIN: pin, SeedDir: seed, SeedURL: seedURL,
			Conductor: cond, Model: modelStr, Brief: plan.Brief,
			ServeOnly:      serveOnly,
			BoxBoot:        cmd.Name() == "_host",
			Agents:         agentsOption(plan),
			Brains:         testBrains(),
			DevCommand:     startDev,
			InstallCommand: startInstall,
			Control:        controlClient(cmd),
			Takeover:       takeover,
			MirrorURL:      plan.MirrorURL,
		})
		if err != nil {
			return err
		}
		pin = r.PIN
		lv.holds(func() { _ = r.Close(context.Background()) })
		// PIN (on create), branch and work path exist only now.
		ann.Announce(console.MemberMsg{PIN: r.PIN, MemberID: r.MemberID, Branch: r.Branch, WorkPath: r.WorkDir})
		fmt.Fprint(out, r.PrintJoin())

		if wizard {
			// Order matters: scaffold first, so DetectInstall/DetectDev have a real
			// project to read rather than an empty tree. A box target never reaches
			// here — plan 32 branched to standUpOnBox before any of this started.
			scaffoldNow(ctx, cmd, out, r.Host, r.PIN, plan)
			plan = recordRunCommands(ctx, cmd, out, r.Host, r.PIN, plan)
			startLocalDev(ctx, cmd, r, plan)
		}
		if once {
			return nil
		}

		log := logx.New("host")
		mode := "conductor"
		if serveOnly {
			mode = "serve-only (elected conductor merges off-box)"
		}
		log.Infof("session %s live — mode=%s", r.PIN, mode)
		log.Infof("git=%s  control=%s", r.GitURL, r.ControlURL)
		if r.WorkDir != "" && r.WorkDir != r.Host.Work {
			log.Infof("edit here: %s (branch %s) — then `slopball sync`", r.WorkDir, r.Branch)
			log.Infof("demo mirror (tracks main, do not edit): %s", r.Host.Work)
		} else {
			log.Infof("work=%s", r.Host.Work)
		}
		log.Infof("ticking every 2s — Ctrl-C to stop (SLOPBALL_LOG=debug for git traces)")

		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		tick := 0
		for {
			select {
			case <-ctx.Done():
				log.Infof("stopping")
				fmt.Fprintln(out, "host stopping")
				return nil
			case <-r.Demoted():
				// Another machine took the PIN (a `box add`, or this run's own
				// handoff). Ticking a fleet against a canonical nobody reads is a
				// zombie host — stop, and let the next start rejoin as a client.
				log.Infof("this machine is no longer the host for %s — stopping the fleet", r.PIN)
				fmt.Fprintln(out, "no longer the host — hosting moved to another machine; run `slopball join "+r.PIN+"` to follow it")
				return nil
			case <-t.C:
				tick++
				// Renew the leases this machine serves, and pick up anything that has
				// fallen free (plan 30). An unreachable control plane moves nothing.
				if r.Placement != nil {
					if err := r.Placement.Tick(ctx); err != nil {
						log.Debugf("tick %d: placement: %v", tick, err)
					}
				}
				old, err := sbGit.Output(ctx, r.Host.Work, "rev-parse", "HEAD")
				if err != nil {
					log.Warnf("tick %d: read work HEAD: %v", tick, err)
				}
				old = strings.TrimSpace(old)

				// serve-only (cloud box): don't run the merger fleet — the elected
				// conductor merges off-box under its harness login. Still track main
				// (which the elector pushes) so the box's dev server stays current.
				if !serveOnly {
					if ahead, err := r.Host.BranchesAheadOfMain(ctx); err != nil {
						log.Warnf("tick %d: list branches ahead: %v", tick, err)
					} else if len(ahead) > 0 {
						log.Infof("tick %d: %d branch(es) ahead of main: %s", tick, len(ahead), strings.Join(ahead, ", "))
					} else {
						log.Debugf("tick %d: nothing ahead of main", tick)
					}
					if err := r.Fleet.TickRoles(ctx); err != nil {
						log.Warnf("tick %d: fleet tick: %v", tick, err)
					}
				} else {
					log.Debugf("tick %d: serve-only, skipping fleet", tick)
				}

				if err := r.Host.SyncWorkToMain(ctx); err != nil {
					log.Warnf("tick %d: sync work to main: %v", tick, err)
				}
				newH, err := sbGit.Output(ctx, r.Host.Work, "rev-parse", "HEAD")
				if err != nil {
					log.Warnf("tick %d: read new work HEAD: %v", tick, err)
				}
				newH = strings.TrimSpace(newH)
				advanced := old != "" && newH != "" && old != newH
				if advanced {
					log.Infof("main advanced %s → %s — reinstalling deps / reconciling dev server", short(old), short(newH))
				}

				// A host that booted against an empty canonical has no dev command
				// yet — the project, and the run file naming how to start it, arrive
				// later. Look again now that main has moved (plan 32); once a dev
				// server is supervised here this is a no-op and PostMergeInstall
				// takes over.
				if err := r.AdoptDeclaredRun(ctx); err != nil {
					log.Warnf("tick %d: adopt declared run commands: %v", tick, err)
				}

				if err := devserver.PostMergeInstall(ctx, r.Host.Work, old, newH, r.InstallCommand, sbGit.Output,
					r.Dev.Logs.Writer(devserver.StreamStderr, devserver.PhaseInstall)); err != nil {
					log.Warnf("tick %d: post-merge install: %v", tick, err)
				}
				if r.Runtime != nil {
					if _, err := r.Runtime.ReconcileFromTo(ctx, old, newH); err != nil {
						log.Warnf("tick %d: runtime reconcile: %v", tick, err)
					}
				}
				// Last, and every tick: the reconcile above restarts the dev server
				// for a migration, an env change or a reseed, but never because it
				// died. Liveness is not tied to anyone merging (plan 34).
				r.KeepDevAlive(ctx, advanced)
			}
		}
	}
	// A standup failure comes back from the console the same way it used to come
	// back from the call in front of it — console.Run takes the screen down and
	// hands the error on, so this branch reads identically on both paths.
	finish := func(err error) error {
		var joinAs *hoststart.ErrJoinAsClient
		if !errors.As(err, &joinAs) {
			return err
		}
		// The client path takes a pin and a name and nothing else — no dev
		// server, no conductor, no canonical. Say so, because a --dev that
		// silently evaporates here is plan 25's original bug.
		fmt.Fprintln(cmd.ErrOrStderr(), joinAs.Message)
		if devCmd != "" {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"warning: joining as a client, so --dev %q will NOT run here — the dev server belongs to whoever hosts canonical\n", devCmd)
		}
		if installCmd != "" {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"warning: joining as a client, so --install %q will NOT run here — installs belong to whoever hosts canonical\n", installCmd)
		}
		joinPIN := pin
		if joinAs.Session.PIN != "" {
			joinPIN = joinAs.Session.PIN
		}
		return runJoinAs(cmd, joinPIN, "host")
	}

	if once {
		// --once is scripting: no console, no interception, today's output.
		return finish(work(ctx, cmd.OutOrStdout()))
	}

	return finish(runConsole(ctx, cmd, consoleSession{
		PIN: pin, Me: clientName(),
		// A mesh host runs the fleet here, so this is where the agent tabs
		// have a live stream to show.
		Elector:  !serveOnly,
		Leave:    lv.Leave,
		Work:     work,
		Announce: ann,
	}))
}

// short trims a git SHA to its 7-char prefix for log lines.
func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func runJoin(cmd *cobra.Command, args []string) error {
	pin := args[0]
	name, _ := cmd.Flags().GetString("name")
	return runJoinAs(cmd, pin, name)
}

func runJoinAs(cmd *cobra.Command, pin, name string) error {
	if name == "" {
		name = clientName()
	}
	client := controlClient(cmd)
	log := logx.New("join")
	log.Infof("joining %s via control plane %s", pin, client.Base)
	ctx := context.Background()
	if err := checkOrphanedCanonical(ctx, client, pin); err != nil {
		return err
	}
	j, err := joindaemon.Join(ctx, joindaemon.Options{
		PIN: pin, Name: name, Control: client,
	})
	if err != nil {
		log.Errorf("join failed: %v", err)
		return err
	}
	log.Infof("joined %s as %s", pin, j.Session.Branch)
	log.Infof("remote=%s overlay=%s", j.Remote, j.Session.HostOverlayAddr)
	log.Infof("work=%s", j.Paths.Work)
	log.Infof("mirror refreshing every 2s — Ctrl-C to stop (SLOPBALL_LOG=debug for git traces)")
	fmt.Fprintf(cmd.OutOrStdout(), "joined %s as %s\nwork: %s\n", pin, j.Session.Branch, j.Paths.Work)
	fmt.Fprintln(cmd.OutOrStdout(), "mirror daemon running — Ctrl-C to stop")

	// The joiner's console differs from the creator's only in what it does not
	// have: no box question, no election. Same dashboard, same tabs, same code
	// path, same leaver — the signal wait below simply becomes the console's
	// event loop. Join clones before the screen rather than behind it, and
	// deliberately: it is one clone, and the branch it puts you on is the first
	// thing the dashboard says.
	lv := &leaver{}
	lv.holds(func() { log.Infof("stopping"); j.Close() })
	defer func() { _ = lv.Leave(ctx) }()
	return runConsole(ctx, cmd, consoleSession{
		PIN: pin, Me: j.Name, MemberID: j.MemberID, Branch: j.Session.Branch, WorkPath: j.Paths.Work,
		Leave: lv.Leave,
		Work: func(ctx context.Context, _ io.Writer) error {
			waitForSignal(ctx)
			return nil
		},
	})
}

// waitForSignal blocks until Ctrl-C or the context ends. It is what holds a
// session open when there is no console to hold it — the fallback path's whole
// body, unchanged from what `join` has always done.
func waitForSignal(ctx context.Context) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(ch)
	select {
	case <-ch:
	case <-ctx.Done():
	}
}

// checkOrphanedCanonical refuses to join a session whose canonical lives on
// *this* machine while the control plane serves no host for it — the state a
// mesh host leaves behind on Ctrl-C now that no human verb re-hosts a PIN
// (plan 27). Joining would clone from nobody and leave a mirror daemon polling
// a dead endpoint, so say what actually happened instead. Claiming the free
// lease automatically is plan 30's job.
func checkOrphanedCanonical(ctx context.Context, client *controlplane.Client, pin string) error {
	p := session.ForPin(pin)
	if _, err := os.Stat(filepath.Join(p.Canonical, canonical.BareDir)); err != nil {
		return nil
	}
	sess, err := client.Session(ctx, pin)
	if err == nil {
		// raw endpoint ok: presence check only — is anybody serving canonical.
		if ep, ok := sess.Endpoints[controlplane.EndpointGit]; ok && ep.URL != "" {
			return nil // someone is serving it — an ordinary join
		}
	}
	// A refused build learns NOTHING about who is serving canonical, so the
	// confident paragraph below would be a wrong answer with instructions in it.
	// One sentence instead (plan 48).
	if errors.Is(err, controlplane.ErrUpgradeRequired) {
		return controlplane.ErrUpgradeRequired
	}
	return fmt.Errorf("session %s: this machine holds canonical at %s, but the control plane %s shows no live host serving it — "+
		"there is nothing to join. Re-host it onto a box with `slopball box add <user@host> --pin %s`, "+
		"which takes the session over and serves this canonical again",
		pin, p.Canonical, client.Base, pin)
}

func runSync(cmd *cobra.Command, _ []string) error {
	s, p, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	intent, _ := cmd.Flags().GetString("intent")
	id := sbGit.SessionIdentity(s.Branch, s.PIN)
	if s.Branch == "" {
		id = sbGit.SessionIdentity("agent", s.PIN)
	}
	branch := s.Branch
	if branch == "" {
		branch = "client/local"
	}
	ctx := context.Background()
	followHost(ctx, s, p, id)
	base, err := syncengine.Sync(ctx, workDir(p), branch, "origin", mirrorDir(p), intent, id)
	if report := base.Report(); report != "" {
		// Never blocking, always said out loud: a resolution made against an old
		// main degrades toward hub-side resolution, and both the agent and the
		// human should know that is what happened (plan 35 decision 4).
		fmt.Fprintln(cmd.OutOrStdout(), "note: "+report)
	}
	if err != nil {
		return err
	}
	syncengine.AnnouncePushed(ctx, controlClient(cmd), s.PIN, clientName(), branch, workDir(p), intent)
	fmt.Fprintln(cmd.OutOrStdout(), "ok sync")
	return nil
}

func runPush(cmd *cobra.Command, _ []string) error {
	s, p, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	intent, _ := cmd.Flags().GetString("intent")
	id := sbGit.SessionIdentity(s.Branch, s.PIN)
	branch := s.Branch
	if branch == "" {
		branch = "client/local"
	}
	ctx := context.Background()
	followHost(ctx, s, p, id)
	if err := syncengine.Push(ctx, workDir(p), branch, "origin", intent, id); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "ok push")
	return nil
}

func runPull(cmd *cobra.Command, _ []string) error {
	s, p, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	id := sbGit.SessionIdentity(s.Branch, s.PIN)
	ctx := context.Background()
	followHost(ctx, s, p, id)
	base, err := syncengine.Pull(ctx, workDir(p), "origin", mirrorDir(p), "", id)
	if report := base.Report(); report != "" {
		fmt.Fprintln(cmd.OutOrStdout(), "note: "+report)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "ok pull")
	return nil
}

// followHost re-resolves the PIN and retargets origin when the canonical moved
// (plan 22). Soft: resolve failures never block sync against the current origin.
func followHost(ctx context.Context, s session.Session, p session.Paths, id sbGit.Identity) {
	client := controlClient(nil)
	_, _ = syncengine.FollowHost(ctx, syncengine.FollowOpts{
		Work: workDir(p), Mirror: mirrorDir(p), PIN: s.PIN,
		Cursors: p.Cursors, ID: id,
		Resolve: func(ctx context.Context, pin string) (string, int, error) {
			return client.GitURL(ctx, pin)
		},
	})
}

// controlClients memoizes one Client per base URL so a console and its own
// daemon share a cache and a single Watch stream (plan 43). Building a new
// *Client per call used to mean two streams for one session.
var controlClients sync.Map // string → *controlplane.Client

func controlClient(_ *cobra.Command) *controlplane.Client {
	// flag → $SLOPBALL_CONTROL → stamped/loopback DefaultURL (BaseURL).
	url := controlplane.BaseURL(controlFlag)
	if v, ok := controlClients.Load(url); ok {
		return v.(*controlplane.Client)
	}
	c := controlplane.NewClient(url)
	actual, _ := controlClients.LoadOrStore(url, c)
	return actual.(*controlplane.Client)
}

// runRepoint retargets this client's origin at a relocated canonical. With no
// --to, resolves the PIN through the control plane; --to forces a URL.
func runRepoint(cmd *cobra.Command, _ []string) error {
	s, p, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	to, _ := cmd.Flags().GetString("to")
	id := sbGit.SessionIdentity(s.Branch, s.PIN)
	ctx := context.Background()
	work, mirror := workDir(p), mirrorDir(p)
	gen := 0

	if to == "" {
		client := controlClient(cmd)
		var err error
		to, gen, err = client.GitURL(ctx, s.PIN)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", s.PIN, err)
		}
	}
	cur, _ := syncengine.OriginURL(ctx, work)
	if err := syncengine.Repoint(ctx, work, mirror, to, id); err != nil {
		return err
	}
	_ = syncengine.SaveCursors(p.Cursors, syncengine.Cursors{Endpoint: to, Generation: gen})
	if cur == to || strings.TrimRight(cur, "/") == strings.TrimRight(to, "/") {
		fmt.Fprintln(cmd.OutOrStdout(), "ok repoint (unchanged)")
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "ok repoint\n  %s\n→ %s\n", cur, to)
	}
	return nil
}

func runRun(cmd *cobra.Command, args []string) error {
	s, p, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	_ = s
	return devserver.Run(context.Background(), workDir(p), args, cmd.OutOrStdout())
}

// runConductor opens the session's canonical and runs the merger role — the
// integrate loop (§6.1). Uses the selected harness CLI when installed,
// mechanical incoming-bias otherwise. This is the standalone conductor a
// laptop host (or elected conductor-on-elector) runs against canonical.
func runConductor(cmd *cobra.Command, _ []string) error {
	s, p, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	// The command's own context: as a verb that is Background, but the wizard
	// calls this in-process (conductorLoop) and its cancellation is the only way
	// a console quit reaches the fleet.
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --remote drives a canonical that lives on another machine (a cloud box):
	// the conductor runs here, under this laptop's harness login, and pushes
	// merges back to the box (plan 21 conductor-on-elector). If omitted: prefer
	// a local on-disk canonical (on-host conductor); only fall through to the
	// control-plane git URL when there is no local canonical (elector laptop).
	remote, _ := cmd.Flags().GetString("remote")
	localCanonical := false
	if remote == "" {
		if _, err := os.Stat(filepath.Join(p.Canonical, "bare.git")); err == nil {
			localCanonical = true
		} else if url := resolveCanonicalRemote(ctx, s.PIN); url != "" {
			remote = url
		}
	}

	var host *canonical.Host
	if remote != "" && !localCanonical {
		// `published` is the address the session carries and the only one a human
		// has ever seen; `remote` becomes the loopback forwarder git dials. Every
		// word printed from here on names the first (firstrun does the same): a
		// forwarder port belongs to this process and stays true for as long as it
		// runs, which is not long enough to put in front of anybody.
		published := remote
		// --remote may be the printed session address (slop://…); git cannot dial
		// that. Dialable is the one place that knows — same as firstrun / join.
		client := controlClient(cmd)
		if sess, serr := client.Session(ctx, s.PIN); serr != nil {
			return fmt.Errorf("resolve session %s for remote %s: %w", s.PIN, published, serr)
		} else if dialed, derr := client.Dialable(ctx, sess, published); derr != nil {
			return canonical.ExplainRemoteOpenFailure(s.PIN, published, derr)
		} else {
			remote = dialed
		}
		host, err = canonical.OpenRemote(ctx, filepath.Join(p.Root, "replica"), s.PIN, remote)
		if err != nil {
			return canonical.ExplainRemoteOpenFailure(s.PIN, published, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "conductor driving remote canonical: %s\n", published)
	} else {
		host, err = canonical.Open(p.Canonical, s.PIN)
		if err != nil {
			return err
		}
	}
	defer host.Close(context.Background())

	// Adopt the session's own fleet composition when it published one (plan 29):
	// a machine electing itself conductor mid-session reproduces the agents the
	// session chose rather than imposing its own default. Falls back to this
	// machine's harness for any role the session never named. The join daemon
	// builds its fleet through the same call, so the two cannot drift.
	log := logx.New("conductor")
	built := conductor.BuildSessionFleet(ctx, conductor.SessionFleetSpec{
		Host: host, ID: sbGit.SessionIdentity("conductor", s.PIN),
		Control: controlClient(cmd), PIN: s.PIN,
		Fallback: harness.FirstAvailable(),
	})
	fleet := built.Fleet
	if built.LogsURL != "" {
		// Probe, don't announce (plan 40). The old line named a URL it never
		// dialled, so a watcher that was blind for a whole session looked
		// perfectly healthy in the log.
		if err := (&conductor.RemoteLogSource{URL: built.LogsURL}).Probe(); err == nil {
			log.Infof("error-watcher watching dev-server logs at %s (reachable)", built.LogsURL)
		} else {
			log.Warnf("error-watcher cannot read dev-server logs at %s: %v — it will see nothing until that answers", built.LogsURL, err)
		}
	} else {
		log.Infof("error-watcher idle — no HTTP dev-server logs to watch (remote=%q)", host.RemoteURL())
	}
	if built.HasIntelligence() {
		log.Infof("conducting session %s — %s", s.PIN, built.Summary())
	} else {
		log.Infof("conducting session %s (no harness — mechanical incoming-bias resolution)", s.PIN)
	}

	// The fleet's own tick stays 2s (local git, the merge hot path); the
	// network fetch against a remote canonical reacts to `sync.pushed` with a
	// floor underneath it (plan 43). A local host is never gated — its Refresh
	// is already a no-op.
	gate := conductor.NewRefreshGate(controlClient(cmd), s.PIN, host.RemoteURL() != "")
	// The two doors differ in whether they wait for the roles they start, and
	// the difference is the whole point: `--once` has no next tick to catch a
	// role it skipped, so it runs them to completion; the loop must come round
	// every 2s even while a minutes-long setup scaffold is in flight.
	refresh := func() error {
		if gate.Due(time.Now()) {
			return host.Refresh(ctx) // no-op for a local host
		}
		return nil
	}

	once, _ := cmd.Flags().GetBool("once")
	if once {
		if err := refresh(); err != nil {
			return err
		}
		return fleet.TickAll(ctx)
	}
	tick := func() error {
		if err := refresh(); err != nil {
			return err
		}
		if err := fleet.TickRoles(ctx); err != nil {
			return err
		}
		return fleet.TickAfter(ctx)
	}
	// A standalone conductor is long-running, so it holds the stream too — that
	// is what makes a merge follow a push rather than a floor. `--once` above
	// has already returned, so no one-shot verb picks this up.
	controlClient(cmd).Watch(ctx, s.PIN)
	// Role state samples beside the loop, not inside a tick: the tick that
	// blocks for a 40-second AI resolve is the one worth seeing (plan 36 §2).
	go built.Publisher.Run(ctx)
	fmt.Fprintln(cmd.OutOrStdout(), "conductor running — Ctrl-C to stop")
	return tickUntilDone(ctx, log, tick)
}

// tickUntilDone runs one fleet tick every 2s until the context is cancelled.
// Shared by `slopball conductor` and the first-run handoff so both loops behave
// identically — including tolerating a failing tick rather than exiting.
func tickUntilDone(ctx context.Context, log *logx.Logger, tick func() error) error {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Infof("stopping")
			return nil
		case <-t.C:
			if err := tick(); err != nil {
				log.Warnf("tick: %v", err)
			}
		}
	}
}

// conductorLoop drives the fleet against a remote canonical under this
// machine's harness login — the terminal state of the first-run box branch, and
// the same thing `slopball conductor --remote` does.
func conductorLoop(ctx context.Context, cmd *cobra.Command, pin, remote string) error {
	conductorCmd := &cobra.Command{}
	// The caller's context, or this is a fleet nothing can stop: the wizard runs
	// the conductor as the tail of its work function, and a console quit ends
	// that context without sending anyone a signal.
	conductorCmd.SetContext(ctx)
	conductorCmd.Flags().String("pin", pin, "")
	conductorCmd.Flags().String("remote", remote, "")
	conductorCmd.Flags().Bool("once", false, "")
	conductorCmd.SetOut(cmd.OutOrStdout())
	conductorCmd.SetErr(cmd.ErrOrStderr())
	return runConductor(conductorCmd, nil)
}

// checkSeedFlags settles what a session is seeded from before anything is
// started. Two seeds is a user error rather than something to merge, and a
// directory that cannot become a commit should say so while the only cost of
// being wrong is retyping the command.
func checkSeedFlags(seedDir, seedURL string) error {
	if seedDir != "" && seedURL != "" {
		return fmt.Errorf("--seed %s and --seed-url %s both name where the session starts from — pass one, not both",
			seedDir, seedURL)
	}
	if seedDir == "" {
		return nil
	}
	return canonical.PreflightSeedDir(seedDir)
}

// ResolveSeedURL picks the canonical a new box clones from when --seed-url was
// not passed. The flag should never be needed: the CLI is already talking to the
// control plane (it resolves this same URL for the cutover), so the session's
// live canonical is known. Two cases legitimately yield "" — a blank box:
//   - the session has no canonical yet (genuinely new session), and
//   - the only canonical *is* the container we are about to replace, which
//     Provision `docker rm -f`s before the new one boots, so cloning from it
//     would dial an endpoint that just died. Caller warns; --volume is the way
//     to keep a box's canonical across a re-provision.
//
// Exported because those two empty answers are the decision rule — seeding from
// the container about to be `docker rm -f`'d is the bug that orphaned a session
// — and a full provision is far too blunt an instrument to pin them with.
func ResolveSeedURL(ctx context.Context, pin, target, container string) string {
	s, err := controlClient(nil).Session(ctx, pin)
	if err != nil {
		return ""
	}
	// raw endpoint ok: the seed URL is handed back to the box, which resolves
	// it on its own machine — resolving it here would forward a loopback.
	ep, ok := s.Endpoints[controlplane.EndpointGit]
	if !ok || ep.URL == "" {
		return ""
	}
	if s.Box != nil && s.Box.Target == target && s.Box.Container == container {
		return ""
	}
	return ep.URL
}

// resolveCanonicalRemote asks the control plane where the session's git lives,
// AS PUBLISHED — on the session network that is `slop://<pin>/git/canonical.git`.
// The caller runs it through Dialable to get something git can clone, and keeps
// this form for everything it prints: a loopback forwarder port is an address
// that belongs to the running process, so it is no use in a message and worse
// than none in an error (session 2lmymb printed two of them and named neither
// the session's git nor a thing to do about it).
func resolveCanonicalRemote(ctx context.Context, pin string) string {
	s, err := controlClient(nil).Session(ctx, pin)
	if err != nil {
		return ""
	}
	// raw endpoint ok: this IS the published address, and the caller dials it
	// through Dialable — resolving here would throw away the only form a human
	// recognises.
	ep, ok := s.Endpoints[controlplane.EndpointGit]
	if !ok {
		return ""
	}
	return ep.URL
}

// runDaemon is the long-running join-side worker (§5.8): keep the local main
// mirror fresh so `sync`'s pull-half is a fast local merge and every client
// stays a near-latest replica (the migration safety net). Origin is a Dialable
// URL — on the session network a loopback forwarder that dies with the join
// process — so every refresh re-resolves before fetch, the same way the join
// daemon's tick does via FollowHost.
func runDaemon(cmd *cobra.Command, _ []string) error {
	s, p, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	id := sbGit.SessionIdentity(s.Branch, s.PIN)
	refresh := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		followHost(ctx, s, p, id)
		return sbGit.Run(ctx, p.Mirror, "fetch", "origin", "+refs/heads/main:refs/heads/main")
	}
	once, _ := cmd.Flags().GetBool("once")
	if once {
		if err := refresh(); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "ok mirror")
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintln(cmd.OutOrStdout(), "mirror daemon running — Ctrl-C to stop")
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			_ = refresh()
		}
	}
}

// runMonitor gives a live, read-only view of one session — role, where main
// sits, branches still ahead of main (convergence), and whether the git and
// dev servers answer. --json reports the resolved session from the control
// plane; the fleet listing that used to walk every live PIN is gone (plan 44).
func runMonitor(cmd *cobra.Command, _ []string) error {
	once, _ := cmd.Flags().GetBool("once")
	asJSON, _ := cmd.Flags().GetBool("json")
	localOnly, _ := cmd.Flags().GetBool("local")
	interval, _ := cmd.Flags().GetDuration("interval")
	if interval <= 0 {
		interval = 2 * time.Second
	}
	home := session.Home()
	client := controlClient(cmd)
	out := cmd.OutOrStdout()

	var pin string
	if asJSON && !localOnly {
		pin = resolvePin(cmd)
		if pin == "" {
			return fmt.Errorf("no session pin — run inside the session work tree, or pass --pin / set SLOPBALL_PIN")
		}
	}

	frame := func() {
		ctx, cancel := context.WithTimeout(context.Background(), interval)
		defer cancel()
		if asJSON && !localOnly {
			sess, err := client.Session(ctx, pin)
			if err != nil {
				fmt.Fprintf(out, `{"error":%q}`+"\n", err.Error())
				return
			}
			_ = jsonEncode(out, sess)
			return
		}
		base := ""
		if !localOnly {
			base = client.Base
		}
		all := monitor.Snapshot(ctx, home, base)
		header := fmt.Sprintf("slopball monitor · home=%s · control=%s", home, dashStr(base))
		monitor.Render(out, header, all)
	}

	if once {
		frame()
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		fmt.Fprint(out, "\033[2J\033[H")
		frame()
		fmt.Fprintln(out, "\n(refreshing every", interval, "— Ctrl-C to stop)")
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func jsonEncode(w interface{ Write([]byte) (int, error) }, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func dashStr(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// boxRunnerFn is the one door onto a box's transport: TestHooks.BoxRunner when
// a test has installed one, production's boxRunner otherwise. Every caller goes
// through here so a fake docker host reaches `box add` and the wizard alike.
func boxRunnerFn(cmd *cobra.Command, args []string) (box.Runner, error) {
	if fake := testHooks().BoxRunner; fake != nil {
		return fake(cmd, args)
	}
	return boxRunner(cmd, args)
}

// boxRunner picks the transport for a box command: --local provisions the
// machine you are on, otherwise the first positional arg is the ssh target.
func boxRunner(cmd *cobra.Command, args []string) (box.Runner, error) {
	if local, _ := cmd.Flags().GetBool("local"); local {
		return box.LocalRunner{}, nil
	}
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return nil, fmt.Errorf("pass an ssh target (user@host) or --local")
	}
	// The literal target `local` means this machine — the form the first-run
	// wizard accepts, since it asks for a target rather than a flag.
	if strings.EqualFold(args[0], "local") {
		return box.LocalRunner{}, nil
	}
	return &box.SSHRunner{Addr: args[0]}, nil
}

// boxRunnerForTarget builds a runner for an explicit target string, going
// through the same seam `box add` uses so tests drive both with one fake.
func boxRunnerForTarget(cmd *cobra.Command, target string) (box.Runner, error) {
	return boxRunnerFn(cmd, []string{target})
}

// boxContainer resolves the container name: --name, else slopball-<pin>.
func boxContainer(cmd *cobra.Command, pin string) string {
	if n, _ := cmd.Flags().GetString("name"); n != "" {
		return n
	}
	return "slopball-" + pin
}

// BoxAddOptions turns `box add` flags + environment into box.Options. Split out
// from runBoxAdd because the wizard's remote-first path resolves the same
// options for a box it provisions itself (standUpOnBox), so the two callers
// cannot drift on which path the defaults select or which flags are meaningless
// on the other one.
//
// Exported for the same reason it exists: that mapping is a decision rule with
// a dozen branches, and reaching it through a real `box add` would need a
// control plane and a docker host per branch — which is exactly the cost the
// split was made to avoid.
func BoxAddOptions(cmd *cobra.Command, pin string) (box.Options, error) {
	warn := func(format string, a ...any) {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: "+format+"\n", a...)
	}
	buildLocal, _ := cmd.Flags().GetBool("build-local")
	image, _ := cmd.Flags().GetString("image")

	var binary []byte
	binPath, _ := cmd.Flags().GetString("binary")
	if binPath != "" && !buildLocal {
		warn("--binary is only used with --build-local; ignoring on the pull path")
		binPath = ""
	}
	if binPath != "" {
		b, err := os.ReadFile(binPath)
		if err != nil {
			return box.Options{}, fmt.Errorf("read --binary: %w", err)
		}
		binary = b
	}
	if buildLocal && len(binary) == 0 {
		return box.Options{}, fmt.Errorf("--build-local requires --binary <path to linux/amd64 slopball>")
	}

	seedURL, _ := cmd.Flags().GetString("seed-url")
	serveOnly, _ := cmd.Flags().GetBool("serve-only")
	volume, _ := cmd.Flags().GetString("volume")
	devCmd, _ := cmd.Flags().GetString("dev")
	installCmd, _ := cmd.Flags().GetString("install")
	briefText, _ := cmd.Flags().GetString("brief")
	pullPolicy, _ := cmd.Flags().GetString("pull")
	requireMatch, _ := cmd.Flags().GetBool("require-version-match")
	noCutover, _ := cmd.Flags().GetBool("no-cutover")
	// One control plane for both halves. The CLI polls its externally reachable
	// URL; a local container may dial the same service over Docker DNS. These are
	// routes, not separate control-plane identities.
	control := controlClient(cmd)
	containerControl := strings.TrimSpace(os.Getenv("SLOPBALL_BOX_CONTROL"))
	if containerControl == "" {
		// TEMPORARY: every box currently runs beside the development compose
		// control plane. Once production has a real public control-plane domain,
		// remove this default and let the container fall back to control.Base.
		containerControl = "http://slopball-control-plane:7777"
	}

	// `box add` means "the box hosts from now on", so the container claims the
	// PIN over the incumbent. --no-cutover already means "don't move hosting
	// here", so it opts out — and then the box is a client, where a dev command
	// (and its install) has nowhere to run.
	if noCutover && (devCmd != "" || installCmd != "") {
		var dropped []string
		if installCmd != "" {
			dropped = append(dropped, fmt.Sprintf("--install %q", installCmd))
		}
		if devCmd != "" {
			dropped = append(dropped, fmt.Sprintf("--dev %q", devCmd))
		}
		warn("--no-cutover leaves hosting where it is, so the box joins as a client and %s will not run there.\n"+
			"         drop --no-cutover to have the box host + install deps + supervise the dev server, or start it yourself with `slopball box run`",
			strings.Join(dropped, " and "))
	}

	opt := box.Options{
		Container:           boxContainer(cmd, pin),
		Binary:              binary,
		PIN:                 pin,
		SeedURL:             seedURL,
		ServeOnly:           serveOnly,
		Volume:              volume,
		Dev:                 devCmd,
		Install:             installCmd,
		Brief:               briefText,
		Takeover:            !noCutover,
		BuildLocal:          buildLocal,
		PullPolicy:          pullPolicy,
		CLIVersion:          Version,
		RequireVersionMatch: requireMatch,
		ControlURL:          control.Base,
		ContainerControlURL: containerControl,
		Control:             control,
		// A BYO box is SOMEBODY ELSE'S hardware, so it inherits whatever this
		// laptop decided rather than our managed-box always-on rule. Telling it
		// explicitly (rather than letting the container read its own absent
		// file) is what makes `slopball telemetry on` here mean the box too.
		Telemetry: byoBoxTelemetry(),
	}
	if buildLocal {
		// --image is the local build tag here; "" → box's own "slopball-box".
		opt.Image = image
		if cmd.Flags().Changed("pull") {
			warn("--pull has no effect with --build-local (nothing is pulled)")
		}
		return opt, nil
	}
	if image == "" {
		image = box.DefaultImageRef(Version)
	}
	opt.ImageRef = image
	return opt, nil
}

// runBoxAdd provisions a cloud dev box. Default (plan 23): docker pull the
// published image matching this CLI's version and run it — no ship, no on-box
// build. --build-local --binary keeps the old ship+build path for developing
// slopball / registry-less boxes. By default it then flips the client-facing
// rendezvous onto the box (plan 22) so existing clients follow on their next
// sync — no re-join.
func runBoxAdd(cmd *cobra.Command, args []string) error {
	r, err := boxRunnerFn(cmd, args)
	if err != nil {
		return err
	}
	pin := resolvePin(cmd)
	if pin == "" {
		return fmt.Errorf("no session pin — run inside the session work tree, or pass --pin")
	}
	opt, err := BoxAddOptions(cmd, pin)
	if err != nil {
		return err
	}
	drain, _ := cmd.Flags().GetBool("drain")
	noCutover, _ := cmd.Flags().GetBool("no-cutover")

	seedDir, _ := cmd.Flags().GetString("seed")
	seedURL, _ := cmd.Flags().GetString("seed-url")
	if err := checkSeedFlags(seedDir, seedURL); err != nil {
		return err
	}

	b, err := provisionAndCutover(context.Background(), cmd, r, pin, opt, seedDir, drain, noCutover)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "new joins:\n\n    slopball join %s\n\n", b.JoinPIN)
	fmt.Fprintf(out, "git:     %s\ncontrol: %s\n\n", b.GitURL, controlClient(cmd).Base)
	printDevTail(cmd, args, pin, opt.Dev, b)
	return nil
}

// provisionAndCutover boots the box and (unless noCutover) flips the session
// onto it, publishing the box facts. Extracted so `box add` and the first-run
// handoff share one implementation: seed resolution, the cutover generation
// handling and PutBox are exactly the parts plan 25 got wrong twice, and a
// second copy would get them wrong a third time.
//
// Seed resolution lives HERE rather than in `box add`, because that is the whole
// point of the extraction. When it sat in the caller, the first-run handoff —
// the other caller — silently skipped it, booted the box on `canonical.Create`
// and orphaned the session's brief, contracts and scaffold on the laptop. Every
// path that provisions a box gets the same answer or none of them do.
func provisionAndCutover(ctx context.Context, cmd *cobra.Command, r box.Runner, pin string, opt box.Options, seedDir string, drain, noCutover bool) (*box.Box, error) {
	// --seed-url is an override, not a requirement: the session's canonical is
	// already on the control plane this CLI is talking to. Without this, a box
	// booted a blank canonical and its dev server died on a missing package.json.
	if opt.SeedURL == "" {
		opt.SeedURL = ResolveSeedURL(ctx, pin, r.Target(), opt.Container)
		switch {
		case opt.SeedURL != "":
			fmt.Fprintf(cmd.ErrOrStderr(), "seeding box canonical from %s\n", opt.SeedURL)
		case resolveCanonicalRemote(ctx, pin) != "":
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: the session's only canonical is the container being replaced — the new box starts blank.\n"+
				"         pass --seed-url <url>, or --volume <path> to persist canonical across re-provisions\n")
		}
	}
	seedURL := opt.SeedURL
	// Invite the box before it boots so the container can present a secret
	// rather than self-register (plan 44 ticket 05).
	if opt.MemberSecret == "" {
		inv, err := controlClient(cmd).Invite(ctx, pin, controlplane.MemberJoinRequest{
			Name: "box", Role: controlplane.RoleBox,
		})
		if err != nil {
			return nil, fmt.Errorf("invite the box: %w", err)
		}
		opt.MemberID, opt.MemberSecret = inv.MemberID, inv.Secret
	}
	b, err := box.Provision(ctx, r, opt)
	if err != nil {
		return nil, err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "box provisioned on %s (container %s)\n\n", b.Target, b.Container)

	// A local --seed rides in on the same shared answer as --seed-url: this is
	// the one place every provisioning path passes through, which is why seed
	// resolution lives here at all (plan 33).
	if seedDir != "" {
		if err := canonical.SeedRemote(ctx, b.GitURL, seedDir, sbGit.SessionIdentity("host", pin)); err != nil {
			return nil, fmt.Errorf("seed the box's canonical from %s: %w", seedDir, err)
		}
		fmt.Fprintf(out, "seeded canonical from %s\n\n", seedDir)
	}

	if !noCutover {
		// Catch up main from whatever the box seeded from — and only that. The
		// old fallback to "whatever the control plane resolves" is exactly the
		// container Provision just `docker rm -f`'d whenever resolveSeedURL
		// declined (see its doc), so catching up from it would dial an endpoint
		// that died seconds ago. Previously harmless because the poisoned
		// NewDialAddr made sameURL skip the catch-up entirely.
		from := seedURL
		client := controlClient(cmd)
		logsURL := ""
		if b.GitURL != "" {
			// Prefer announced logs once the box has registered; fall back to sibling path.
			// raw endpoint ok: republished verbatim through the cutover — the
			// control plane stores what the box announced, not this machine's
			// local resolution of it.
			if sess, err := client.Session(ctx, pin); err == nil {
				if ep, ok := sess.Endpoints[controlplane.EndpointLogs]; ok {
					logsURL = ep.URL
				}
			}
		}
		// Pass the generation the box announced at rather than letting Flip
		// re-resolve: if anything else wrote in between, that is a racing writer
		// and ErrGenerationConflict is the correct answer, not a clobber.
		res, err := cutover.Flip(ctx, cutover.Options{
			PIN: pin, NewDialAddr: b.GitURL, FromURL: from, Drain: drain,
			Control: client, LogsURL: logsURL, Generation: b.Generation,
		})
		if err != nil {
			return nil, fmt.Errorf("cutover: %w", err)
		}
		_ = client.PutBox(ctx, pin, controlplane.BoxFacts{
			Target: b.Target, Container: b.Container, Image: opt.ImageRef, Version: Version,
		})
		fmt.Fprintf(out, "cutover: control plane flipped → %s (gen %d)", res.DialAddr, res.Generation)
		if res.CaughtUp {
			fmt.Fprint(out, " (main caught up)")
		}
		if res.Drained {
			fmt.Fprint(out, " [drain]")
		}
		fmt.Fprint(out, "\nexisting clients follow on next `slopball sync` — no re-join\n\n")
	}
	return b, nil
}

// printDevTail closes `box add` with the truth about the dev server. Telling the
// operator to run `slopball box run … -- <dev command>` when --dev was supplied
// contradicts the flag, and (before the box actually hosted) was wrong advice
// besides — that container was serving nothing.
func printDevTail(cmd *cobra.Command, args []string, pin, devCmd string, b *box.Box) {
	out := cmd.OutOrStdout()
	if devCmd == "" {
		fmt.Fprintf(out, "start the dev server on the box:\n\n    slopball box run -- <dev command>\n")
		return
	}
	client := controlClient(cmd)
	ctx := context.Background()
	// Resolved, not raw: this URL is printed for a human to OPEN, and opening
	// it is a dial. `slop://<pin>/dev/` pasted into a browser is exactly the
	// failure plan 41 fixes.
	if devURL, err := devEndpoint(ctx, client, pin); err == nil && devURL != "" {
		fmt.Fprintf(out, "dev:     %s\n", devURL)
		return
	}
	sess, _ := client.Session(ctx, pin)
	logsURL := ""
	// raw endpoint ok: printed for a human, alongside the dev URL above.
	if ep, ok := sess.Endpoints[controlplane.EndpointLogs]; ok {
		logsURL = ep.URL
	}
	if logsURL == "" {
		logsURL = conductor.LogsURLFromRemote(b.GitURL)
	}
	fmt.Fprintf(out, "dev:     %q supervised on the box — logs: %s\n", devCmd, dashStr(logsURL))
	// The box publishes the dev port at provision time, so the endpoint no
	// longer waits on the repo committing a PORT — reaching here means the
	// container has not registered yet, not that the project is missing
	// something.
	fmt.Fprintf(out, "         its URL appears once the container registers with the control plane\n")
	fmt.Fprintf(out, "         watch it with:  slopball box logs\n")
}

// runElect implements plan 21: elect this laptop's harness CLI as the
// conductor runner for a remote box (conductor-on-elector), or self-elect / revoke.
func runElect(cmd *cobra.Command, _ []string) error {
	s, p, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	revoke, _ := cmd.Flags().GetBool("revoke")
	self, _ := cmd.Flags().GetBool("self")
	box, _ := cmd.Flags().GetString("box")
	name, _ := cmd.Flags().GetString("name")
	hName, _ := cmd.Flags().GetString("harness")
	model, _ := cmd.Flags().GetString("model")
	if name == "" {
		name = s.Branch
	}
	if name == "" {
		name = os.Getenv("USER")
	}
	if name == "" {
		name = "agent"
	}
	if hName == "" {
		if c := harness.FirstAvailable(); c != nil {
			hName = string(c.Name)
		} else {
			hName = "claude"
		}
	}

	if revoke {
		st, err := trust.LoadPersist(p.Root)
		if err != nil {
			return fmt.Errorf("no active election to revoke: %w", err)
		}
		if st.BoxRoot != "" {
			_ = trust.DeactivateOnBox(st.BoxRoot)
			_ = trust.WipeBox(st.BoxRoot)
		}
		_ = trust.ClearPersist(p.Root)
		fmt.Fprintln(cmd.OutOrStdout(), "revoked harness-runner election — stop conductor AI on this laptop")
		return nil
	}

	if self {
		b := trust.NewBroker(trust.RemoteBox) // may be mid-migration; SelfElect flips to local
		if err := b.SelfElect(name); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), b.LastWarning())
		_ = trust.ClearPersist(p.Root)
		return nil
	}

	// Remote box is the only case that needs election. Local/mesh sessions
	// refuse — the trust hole must never open. Derive from the session's box
	// record, never from an env a process can assert about itself.
	sess, err := controlClient(cmd).Session(context.Background(), s.PIN)
	if err != nil {
		return err
	}
	kind := trust.LocalHost
	if sess.Box != nil {
		kind = trust.RemoteBox
	}
	b := trust.NewBroker(kind)
	fmt.Fprintln(cmd.OutOrStdout(), b.WarnBeforeElect(name, hName))

	lease, err := b.Elect(trust.ElectRequest{Elector: name, Harness: hName, Model: model})
	if err != nil {
		_ = b.Close()
		return err
	}
	if box != "" {
		if _, err := trust.InstallOnBox(box, lease); err != nil {
			_ = b.Close()
			return err
		}
	}
	_ = trust.SavePersist(p.Root, trust.PersistState{
		PIN: s.PIN, Elector: lease.Elector, Harness: lease.Harness, Model: lease.Model,
		RunnerID: lease.RunnerID, BoxRoot: box,
	})
	_ = controlClient(cmd).PutConductor(context.Background(), s.PIN, controlplane.ConductorRecord{
		Elector: lease.Elector, Harness: lease.Harness, Model: lease.Model,
		RunnerID: lease.RunnerID, Active: true,
	})
	fmt.Fprintf(cmd.OutOrStdout(),
		"elected %s (%s)\nrunner id: %s\nAI roles run on THIS laptop via the %s harness — login stays here.\nNow run `slopball conductor` here (this session is %s — export SLOPBALL_PIN=%s to skip\n--pin on off-box commands). It resolves the box canonical from the control plane\nand drives merges off-box (or pass --remote <box-git-url>).\n",
		lease.Elector, lease.Harness, lease.RunnerID, lease.Harness, s.PIN, s.PIN)
	_ = b.Close()
	return nil
}

// byoBoxTelemetry is what a `box add` box is told: this laptop's own setting.
// The box has no human to ask, and its image carries no defaults.json, so the
// machine that provisioned it answers on its behalf.
func byoBoxTelemetry() string {
	if on, _ := telemetry.Resolve(); on {
		return telemetry.ModeOn
	}
	return telemetry.ModeOff
}
