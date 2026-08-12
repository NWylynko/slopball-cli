package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nwylynko/slopball-cli/box"
	"github.com/nwylynko/slopball-cli/canonical"
	"github.com/nwylynko/slopball-cli/conductor"
	"github.com/nwylynko/slopball-cli/console"
	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/firstrun"
	sbGit "github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/harness"
	"github.com/nwylynko/slopball-cli/hoststart"
	"github.com/nwylynko/slopball-cli/joindaemon"
	"github.com/nwylynko/slopball-cli/runfile"
	"github.com/nwylynko/slopball-cli/session"
	"github.com/nwylynko/slopball-cli/trust"
	"github.com/spf13/cobra"
)

// interactive decides whether to ask the questions. Tri-state on the flag:
// --interactive forces on, --interactive=false forces off, unset = auto.
//
// Auto requires a real terminal on both ends and a run that is not scripted.
// Note the flag only exists on `root`: the internal `_host` verb never carries
// it, so "did a machine boot this?" is a fact about which verb ran rather than
// a condition to evaluate (plan 27 paying for itself).
func interactive(cmd *cobra.Command) bool {
	f := cmd.Flags().Lookup("interactive")
	if f == nil {
		return false // `_host`: a booting box has nobody to answer
	}
	if f.Changed {
		on, _ := cmd.Flags().GetBool("interactive")
		return on
	}
	if once, _ := cmd.Flags().GetBool("once"); once {
		return false
	}
	if serveOnly, _ := cmd.Flags().GetBool("serve-only"); serveOnly {
		return false
	}
	return isCharDevice(os.Stdin) && isCharDevice(os.Stdout)
}

// isCharDevice reports whether f is a terminal, without a new dependency.
func isCharDevice(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// resolveFirstRun builds the session plan: flags first, then this machine's
// remembered pre-fills, then detection — and the prompt only fills what is
// still a gap. Returns the plan and whether any question was actually asked.
func resolveFirstRun(cmd *cobra.Command, seedDir string) (firstrun.Plan, bool, error) {
	plan := firstrun.Detect(seedDir)
	asked := map[string]bool{}

	conductorFlag, _ := cmd.Flags().GetString("conductor")
	modelFlag, _ := cmd.Flags().GetString("model")
	if conductorFlag != "" {
		plan.SetAllAgents(firstrun.Agent{Harness: conductorFlag, Model: modelFlag})
	}
	perRole := false
	for flag, set := range map[string]*firstrun.Agent{
		"agent-merger": &plan.Merger, "agent-watcher": &plan.Watcher, "agent-setup": &plan.Setup,
	} {
		v, _ := cmd.Flags().GetString(flag)
		if v == "" {
			continue
		}
		perRole = true
		*set = parseAgent(v)
	}
	if conductorFlag != "" || perRole {
		asked[firstrun.QAgents] = true
	}
	if brief, _ := cmd.Flags().GetString("brief"); brief != "" {
		plan.Brief, asked[firstrun.QBrief] = brief, true
	}
	// --box is the MANAGED answer (plan 37): a bool, no target. --box-ssh is BYO.
	if cmd.Flags().Changed("box") {
		plan.Box, _ = cmd.Flags().GetBool("box")
		asked[firstrun.QBox] = true
	}
	if target, _ := cmd.Flags().GetString("box-ssh"); target != "" {
		plan.BoxTarget, asked[firstrun.QBox] = target, true
	}
	if v, _ := cmd.Flags().GetString("install"); v != "" {
		plan.Install, asked[firstrun.QInstall] = strings.Fields(v), true
	}
	if v, _ := cmd.Flags().GetString("dev"); v != "" {
		plan.Dev, asked[firstrun.QDev] = strings.Fields(v), true
	}
	if v, _ := cmd.Flags().GetString("mirror"); v != "" {
		plan.MirrorURL, asked[firstrun.QMirror] = v, true
	}

	if !interactive(cmd) {
		return plan, false, nil
	}
	plan, err := firstrun.Ask(cmd.InOrStdin(), cmd.OutOrStdout(), plan, asked, firstrun.Recall())
	if err != nil {
		return plan, true, err
	}
	// Remembered for the *next new session's* prompts only — never authoritative.
	_ = firstrun.SaveDefaults(plan)
	return plan, true, nil
}

// parseAgent reads a `harness[:model]` flag value.
func parseAgent(v string) firstrun.Agent {
	name, model, _ := strings.Cut(v, ":")
	return firstrun.Agent{Harness: strings.TrimSpace(name), Model: strings.TrimSpace(model)}
}

// agentsOption renders the plan for hoststart, dropping roles with no agent so
// they fall through to the shared lookup.
func agentsOption(p firstrun.Plan) map[string]controlplane.RoleAgent {
	out := map[string]controlplane.RoleAgent{}
	for role, a := range p.Agents() {
		if a.Harness != "" {
			out[role] = controlplane.RoleAgent{Harness: a.Harness, Model: a.Model}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// preflightBox dials the target before any session exists. Failure is fatal,
// never a quiet drop-back to a local host: "I asked for a box and got a laptop"
// is worse than an error, and a typo found after cutover is the expensive one.
func preflightBox(ctx context.Context, cmd *cobra.Command, target string, out io.Writer) error {
	r, err := boxRunnerForTarget(cmd, target)
	if err != nil {
		return fmt.Errorf("box %s: %w", target, err)
	}
	ver, err := r.Run(ctx, "docker version --format '{{.Server.Version}}'")
	if err != nil {
		return fmt.Errorf("box %s is not usable: %w\n"+
			"       check the ssh target and that docker is installed and running there", target, err)
	}
	fmt.Fprintf(out, "    ✓ ssh ok — docker %s on %s\n", strings.TrimSpace(ver), r.Target())
	return nil
}

// scaffoldNow runs the setup role once, synchronously, so the project exists
// before anything that depends on the tree: install/dev detection, and a host
// that would otherwise supervise a dev server in an empty directory (plan 26's
// note — nothing restarts a dev server that dies).
//
// The canonical may be local (this laptop hosts) or remote (the box hosts and
// the fleet drives it from here — plan 32). conductor.Setup already picks
// Host.Remote over Host.Bare, so one implementation covers both and the two
// paths cannot drift.
//
// A failure prints and continues, matching the existing dev-server tolerance:
// the session stays up and the joined agents can build it themselves — their
// contracts already carry the brief.
//
// It runs its OWN state publisher for the duration. The wizard scaffolds in
// front of the conductor loop, and the conductor loop is the only other thing
// that starts one — so the longest-running work in a session was also the only
// work nothing reported, and every dashboard in that session (including this
// laptop's own console, one tab away) read `setup ○ starting` until the project
// was already committed.
func scaffoldNow(ctx context.Context, cmd *cobra.Command, out io.Writer, host *canonical.Host, pin string, plan firstrun.Plan) {
	if plan.Brief == "" {
		return
	}
	agent := testHooks().SetupAgent
	if agent == nil {
		brain := harness.Lookup(plan.Setup.Harness, plan.Setup.Model)
		if brain == nil {
			fmt.Fprintf(out, "  setup: no agent CLI available — the brief is on main and every joined agent's\n"+
				"         contract carries it, so the first teammate with a harness can build it.\n")
			return
		}
		agent = conductor.HarnessScaffolder(brain)
	}
	fmt.Fprintf(out, "\n  setup (%s): working from %q…\n", plan.Setup.Harness, plan.Brief)
	client := controlClient(cmd)
	board := &conductor.StateBoard{}
	role := &conductor.Setup{
		Host: host, ID: sbGit.SessionIdentity("conductor", pin),
		Agent: agent, Harness: plan.Setup.Harness, Control: client, PIN: pin,
		States: board,
	}
	// Stopped as soon as the tick returns: the conductor loop builds a fleet
	// with its own board a moment later, and two publishers writing the same
	// record would fight over what the roles are doing.
	pubCtx, stopPublisher := context.WithCancel(ctx)
	publisher := &conductor.StatePublisher{
		Control: client, PIN: pin, Board: board,
		Record: conductor.SessionRecord(ctx, client, pin, agentsOption(plan)),
	}
	go publisher.Run(pubCtx)
	err := role.Tick(ctx)
	stopPublisher()
	if err != nil {
		fmt.Fprintf(out, "  setup failed: %v — the session is still up; ask an agent to build it\n", err)
		return
	}
	fmt.Fprintln(out, "  setup: committed to main")
}

// recordRunCommands resolves blank install/dev answers against the freshly
// scaffolded tree and commits them to canonical. From there every future host —
// the box serving it now, a migration survivor, a second box — reads the same
// commands with no flags threaded anywhere.
func recordRunCommands(ctx context.Context, cmd *cobra.Command, out io.Writer, host *canonical.Host, pin string, plan firstrun.Plan) firstrun.Plan {
	// The scaffold landed on main, not in the host's demo mirror; refresh it so
	// detection sees the project that was just committed. For a remote canonical
	// the mirror has to be updated from the box first.
	if err := host.Refresh(ctx); err != nil {
		fmt.Fprintf(out, "warning: could not refresh canonical: %v\n", err)
	}
	_ = host.SyncWorkToMain(ctx)
	plan = firstrun.ResolveCommands(plan, host.Work)
	if len(plan.Install) == 0 && len(plan.Dev) == 0 {
		return plan
	}
	fmt.Fprintf(out, "  detected: install %q  dev %q\n",
		strings.Join(plan.Install, " "), strings.Join(plan.Dev, " "))
	if err := runfile.Commit(ctx, host, sbGit.SessionIdentity("host", pin),
		plan.Install, plan.Dev); err != nil {
		fmt.Fprintf(out, "warning: could not record %s (a future host will re-detect): %v\n", runfile.Path, err)
	}
	return plan
}

// standUpOnBox is the remote-first path (plan 32): when the operator names a
// box, the box is the session's FIRST host. Nothing is created here and moved
// there.
//
// The old flow stood a full local host up at generation 1, scaffolded into it,
// had the box take over (2), cut over (3), and repointed the laptop it had just
// finished hosting from — three generations and a canonical transfer to reach a
// state known before anything started. The cutover machinery is not wrong; it is
// load-bearing for `box add` on a live session with real clients holding real
// branches. It is simply not needed to answer a question asked before the
// session exists, and every step of it was a step that could be skipped or got
// wrong.
//
// The election matters as much as the placement: the box has no harness login
// (§10), so a laptop that simply walked away would leave the fleet mechanical
// and the setup role — which has no mechanical mode — unable to run at all.
func standUpOnBox(ctx context.Context, cmd *cobra.Command, plan firstrun.Plan, once bool) error {
	out := cmd.OutOrStdout()
	// PIN starts empty: the create Claim mints it. The console draws first with
	// "minting session…" and Announce fills the join line when the name lands
	// (abuse-surface ticket 11). The box is then handed that minted PIN.
	var pin string
	opt, err := BoxAddOptions(cmd, "")
	if err != nil {
		return err
	}
	// Fixed, not asked (plan 29 §7): the harness login stays on this laptop, so
	// the box serves and supervises but runs no on-box conductor. Install/dev
	// are deliberately absent — the box reads .slopball/run.json once the setup
	// role commits it. Takeover is required because the laptop Claims first
	// (plan 44 ticket 05); the box becomes host at generation 2.
	opt.ServeOnly, opt.Takeover = true, true
	opt.Dev, opt.Install = "", ""
	opt.Brief = plan.Brief

	client := controlClient(cmd)
	ann, lv := newAnnouncer(), &leaver{}
	defer func() { _ = lv.Leave(context.Background()) }()

	// The console comes up BEFORE any of this (plan 36 §3, extended). Booting a
	// box is a wait on somebody else's machine that runs to minutes, and it used
	// to happen on a blank terminal behind one log line — the dashboard renders
	// `box provisioning…` off the session record for exactly this. The scaffold
	// then streams into its own tab with `setup ● working` beside it, which
	// replaces the wall of text at the moment it is worst.
	work := func(ctx context.Context, out io.Writer) error {
		// Claim first: create the session and this laptop's member secret so
		// RequestBox / Invite are attributable (plan 44 ticket 05). The server
		// mints the PIN; we announce it the moment it returns.
		hostname, _ := os.Hostname()
		claim, err := client.Claim(ctx, controlplane.ClaimRequest{
			HostMachine: hostname,
			MemberName:  clientName(), MemberRole: controlplane.RoleClient,
		})
		if err != nil {
			return fmt.Errorf("claim the session before asking for a box: %w", err)
		}
		pin = claim.Session.PIN
		opt.PIN = pin
		ann.Announce(console.MemberMsg{PIN: pin})
		b, err := provisionSessionBox(ctx, cmd, client, pin, plan, opt)
		if err != nil {
			// Fatal, with NO fallback to hosting locally. "I asked for a box and got
			// a laptop" is worse than an error. End the Claim-created session so it
			// does not leak as a live empty PIN.
			_ = client.EndSession(context.Background(), pin)
			return err
		}
		fmt.Fprintf(out, "box provisioned on %s (container %s)\n\n", b.Target, b.Container)
		// What the box published is the session's own address, and with the session
		// network on (the default) that is `slop://<pin>/git/canonical.git` — a name
		// for a session ROLE, not a URL git can dial. Dialable is the one place that
		// knows the difference, and every local git tool below has to go through it:
		// this branch used to hand the raw address to `git clone --mirror` and die
		// with `remote helper 'slop' aborted session` the moment the box came up.
		//
		// Resolved once, so the forwarder outlives every caller below rather than
		// being rebuilt per caller. The line printed for humans keeps the published
		// address deliberately: it stays true when the git lease migrates, and a
		// loopback forwarder port does not.
		sess, err := client.Session(ctx, pin)
		if err != nil {
			return fmt.Errorf("read back the session the box just claimed: %w", err)
		}
		gitURL, err := client.Dialable(ctx, sess, b.GitURL)
		if err != nil {
			return fmt.Errorf("reach the box's canonical at %s: %w", b.GitURL, err)
		}
		// The seeded project goes onto the box's main BEFORE the setup role reads it
		// (plan 33). A seeded session is not a blank one, and the setup role picks
		// scaffold-vs-adapt from what is on main — arriving after it has scaffolded
		// would be a project buried under a generated one.
		if seedDir, _ := cmd.Flags().GetString("seed"); seedDir != "" {
			if err := canonical.SeedRemote(ctx, gitURL, seedDir, sbGit.SessionIdentity("host", pin)); err != nil {
				return fmt.Errorf("seed the box's canonical from %s: %w", seedDir, err)
			}
			fmt.Fprintf(out, "seeded canonical from %s\n", seedDir)
		}

		fmt.Fprintf(out, "session live. share with your team:\n\n    slopball join %s\n\n", pin)
		fmt.Fprintf(out, "git:     %s\ncontrol: %s\n\n", b.GitURL, client.Base)

		// This laptop is a member like any other — plan 30's premise is that the
		// device which created the session is not special.
		j, err := joinSelf(ctx, cmd, pin, out)
		if err != nil {
			return fmt.Errorf("join the session this laptop just created: %w", err)
		}
		// This laptop's daemon is what leaving has to stop — its Close hands the
		// leases back, drops the member and clears the live marker.
		lv.holds(j.Close)
		// Which branch this laptop edits on, and where, exists only now — and the
		// screen asking the question has been up since before the box booted.
		ann.Announce(console.MemberMsg{
			PIN: pin, MemberID: j.MemberID, Branch: j.Session.Branch, WorkPath: j.Paths.Work,
		})
		if err := electSelf(cmd, pin, plan); err != nil {
			return fmt.Errorf("elect this machine as the harness runner: %w", err)
		}

		// The setup role runs HERE, under this laptop's harness login, against
		// the box's canonical. Synchronously, so install/dev detection below has
		// a real project to read (plan 29's ordering, unchanged).
		remote, err := canonical.OpenRemote(ctx, filepath.Join(session.ForPin(pin).Root, "replica"), pin, gitURL)
		if err != nil {
			return fmt.Errorf("open the box's canonical: %w", err)
		}
		defer remote.Close(context.Background())
		scaffoldNow(ctx, cmd, out, remote, pin, plan)
		recordRunCommands(ctx, cmd, out, remote, pin, plan)
		if once {
			return nil
		}
		return conductorLoop(ctx, cmd, pin, gitURL)
	}
	if once {
		// --once is scripting: no console, no interception, today's output.
		return work(ctx, out)
	}

	return runConsole(ctx, cmd, consoleSession{
		PIN: pin, Me: clientName(),
		// This laptop holds the harness login, so this is where the agent tabs
		// have a live stream to show (§10).
		Elector:  true,
		Leave:    lv.Leave,
		Work:     work,
		Announce: ann,
	})
}

// joinSelf registers this laptop as an ordinary client of the session it just
// created, named from $USER exactly as `slopball join` names it. Calling it
// "host" would name a machine that hosts nothing — the confusion plan 16 spent a
// whole plan killing.
func joinSelf(ctx context.Context, cmd *cobra.Command, pin string, out io.Writer) (*joindaemon.Joined, error) {
	j, err := joindaemon.Join(ctx, joindaemon.Options{
		PIN: pin, Name: clientName(), Control: controlClient(cmd),
		// The wizard scaffolds and then conducts in this same process
		// (scaffoldNow, then conductorLoop). ConductsHere makes the daemon hold
		// the conductor lease for that fleet instead of standing up a second one
		// beside it — which is how two setup roles ended up racing on one brief.
		ConductsHere: true,
	})
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "you are %s — edit here: %s\n\n", j.Session.Branch, j.Paths.Work)
	return j, nil
}

// clientName is this machine's member name.
func clientName() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "agent"
}

// electSelf elects this machine as the session's harness runner, the same act
// `slopball elect` performs — shared so the wizard cannot drift from it.
func electSelf(cmd *cobra.Command, pin string, plan firstrun.Plan) error {
	p := session.ForPin(pin)
	name := os.Getenv("USER")
	if name == "" {
		name = "host"
	}
	primary := plan.Merger
	if primary.Harness == "" {
		primary = firstrun.Agent{Harness: "claude"}
	}
	b := trust.NewBroker(trust.RemoteBox)
	lease, err := b.Elect(trust.ElectRequest{Elector: name, Harness: primary.Harness, Model: primary.Model})
	if err != nil {
		_ = b.Close()
		return err
	}
	defer b.Close()
	_ = trust.SavePersist(p.Root, trust.PersistState{
		PIN: pin, Elector: lease.Elector, Harness: lease.Harness, Model: lease.Model,
		RunnerID: lease.RunnerID,
	})
	roles := map[string]controlplane.RoleAgent{}
	for role, a := range plan.Agents() {
		if a.Harness != "" {
			roles[role] = controlplane.RoleAgent{Harness: a.Harness, Model: a.Model}
		}
	}
	_ = controlClient(cmd).PutConductor(context.Background(), pin, controlplane.ConductorRecord{
		Elector: lease.Elector, Harness: lease.Harness, Model: lease.Model,
		RunnerID: lease.RunnerID, Roles: roles, Active: true,
	})
	fmt.Fprintf(cmd.OutOrStdout(), "elected %s — merger=%s  error-watcher=%s  setup=%s\n",
		lease.Elector, plan.Merger, plan.Watcher, plan.Setup)
	return nil
}

// startLocalDev installs and supervises on this laptop — the no-box terminal
// state. The commands are whatever the wizard resolved, which by then means the
// scaffolded project's own.
func startLocalDev(ctx context.Context, cmd *cobra.Command, running *hoststart.Running, plan firstrun.Plan) {
	if len(plan.Install) == 0 && len(plan.Dev) == 0 {
		return
	}
	if err := running.StartDev(ctx, plan.Install, plan.Dev); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
		return
	}
	if len(plan.Dev) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "dev: %q supervised here\n", strings.Join(plan.Dev, " "))
	}
}

// warnMirrorCredential notes when a mirror URL is set but this machine has no
// push token. The URL itself is passed to hoststart.Options — never via env.
func warnMirrorCredential(cmd *cobra.Command, plan firstrun.Plan) {
	if plan.MirrorURL == "" {
		return
	}
	if name, _ := firstrun.MirrorCredential(); name == "" {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: mirroring paused: no credential here (set GITHUB_TOKEN or GH_TOKEN and restart)\n")
	}
}

// seedOrURL is the directory the mirror pre-fill reads its origin from. Only a
// local --seed has one to read; a --seed-url is already the URL.
func seedOrURL(seedDir, seedURL string) string {
	if seedDir != "" {
		return seedDir
	}
	if seedURL != "" {
		// Not a directory, but enough for question 2 to switch to adapt wording.
		return seedURL
	}
	return ""
}

// testBrains wires the test setup agent into the fleet so a first-run test can
// exercise the whole flow without a real (billed) model call. nil in production.
func testBrains() map[string]*harness.Client {
	agent := testHooks().SetupAgent
	if agent == nil {
		return nil
	}
	return map[string]*harness.Client{
		"setup": {Name: "fake", Bin: "fake", AgentFn: func(ctx context.Context, dir, prompt string, _ io.Writer) error {
			// AgentFn is below the point where activity is derived from the
			// stream, so a fake reports none — the fleet's own board still
			// carries the mode/brief line.
			return agent(ctx, dir, prompt, nil)
		}},
	}
}

// provisionSessionBox is the fork between plan 37's two honest flavors, and the
// only place in the wizard that knows there are two.
//
//   - MANAGED (plan.Box): the control plane provisions it. This machine sends a
//     PIN and a brief, polls one JSON record, and gets back a git URL. It never
//     runs docker, never holds an ssh key, and is not the box's machine.
//   - BYO (plan.BoxTarget, `--box-ssh`): provisioning runs here over ssh, onto a
//     machine the operator owns. Unchanged, and still first-class — §11 wants
//     both, and this is the tier that bills nobody.
func provisionSessionBox(ctx context.Context, cmd *cobra.Command, client *controlplane.Client,
	pin string, plan firstrun.Plan, opt box.Options) (*box.Box, error) {
	if plan.BoxTarget != "" {
		inv, err := client.Invite(ctx, pin, controlplane.MemberJoinRequest{
			Name: "box", Role: controlplane.RoleBox,
		})
		if err != nil {
			return nil, fmt.Errorf("invite the box: %w", err)
		}
		opt.MemberID, opt.MemberSecret = inv.MemberID, inv.Secret
		r, err := boxRunnerForTarget(cmd, plan.BoxTarget)
		if err != nil {
			return nil, err
		}
		b, err := box.Provision(ctx, r, opt)
		if err != nil {
			return nil, err
		}
		// BYO facts land in the SAME record a managed box ends up in, so the
		// monitor, the console and slopdebug learn one shape.
		_ = client.PutBox(ctx, pin, controlplane.BoxFacts{
			Target: b.Target, Container: b.Container, Image: b.Image, Version: Version,
		})
		return b, nil
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "asking %s for a box…\n", client.Base)
	if _, err := client.RequestBox(ctx, pin, controlplane.BoxRequest{
		Brief: plan.Brief, SeedURL: opt.SeedURL,
	}); err != nil {
		return nil, fmt.Errorf("ask the control plane for a box: %w", err)
	}
	rec, err := client.WaitForBox(ctx, pin, 10*time.Minute)
	if err != nil {
		return nil, err
	}
	return &box.Box{
		Target: rec.Target, Container: rec.Container, Image: rec.Image,
		JoinPIN: pin, GitURL: rec.GitURL, ControlURL: client.Base,
	}, nil
}
