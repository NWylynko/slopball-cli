// Package hoststart wires `slopball` (no args): seed canonical, start git +
// conductor + register into the control plane (plans/10 + 24).
package hoststart

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nwylynko/slopball-cli/admission"
	"github.com/nwylynko/slopball-cli/boxexec"
	"github.com/nwylynko/slopball-cli/canonical"
	"github.com/nwylynko/slopball-cli/conductor"
	"github.com/nwylynko/slopball-cli/contracts"
	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/detect"
	"github.com/nwylynko/slopball-cli/devserver"
	"github.com/nwylynko/slopball-cli/durability"
	sbGit "github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/gitserver"
	"github.com/nwylynko/slopball-cli/harness"
	"github.com/nwylynko/slopball-cli/logx"
	"github.com/nwylynko/slopball-cli/netbind"
	"github.com/nwylynko/slopball-cli/placement"
	"github.com/nwylynko/slopball-cli/runfile"
	"github.com/nwylynko/slopball-cli/runtime"
	"github.com/nwylynko/slopball-cli/session"
	"github.com/nwylynko/slopball-cli/sessionnet"
	"github.com/nwylynko/slopball-cli/syncengine"
)

// ErrJoinAsClient means the control plane says this machine must join as a client
// (session generation is ahead of the local cache). Message explains why.
type ErrJoinAsClient struct {
	Message string
	Session controlplane.Session
}

func (e *ErrJoinAsClient) Error() string { return e.Message }

// Options controls a host-start.
type Options struct {
	PIN       string // empty → generate
	Name      string // this laptop's client-branch name (default "host")
	SeedDir   string
	SeedURL   string
	Conductor string
	Model     string
	// Brief is the one-line "what are we building?" answer. It is committed to
	// main as .slopball/brief.md when main carries none — from there the setup
	// role scaffolds from it and every joined agent's contract quotes it
	// (plan 28). Never overwrites a brief the session already agreed on.
	Brief string
	// Brain is the one harness client every role shares — the "all roles"
	// shorthand, and still what a plain `--conductor claude` produces.
	Brain *harness.Client
	// Agents is the session's per-role fleet composition (plan 29), keyed
	// "merger" / "error-watcher" / "setup". Roles named here get their own
	// harness client; roles absent fall back to Brain / Conductor. Published to
	// the control plane so a machine electing itself conductor later reproduces
	// the same fleet without being told.
	Agents map[string]controlplane.RoleAgent
	// Brains overrides the resolved client per role — the test seam that keeps
	// unit tests from shelling out to a real CLI.
	Brains     map[string]*harness.Client
	Control    *controlplane.Client // nil → controlplane.BaseURL("")
	DevCommand []string
	// InstallCommand overrides DetectInstall for first-boot + post-merge
	// installs (plan 26). Empty with a DevCommand still runs DetectInstall so a
	// lockfile-bearing seed installs before the supervised process starts.
	InstallCommand []string
	// ServeOnly means this host serves canonical and tracks main but runs no
	// fleet — the cloud-box shape. It never claims the conductor lease, because
	// the harness login belongs to a laptop (§10).
	ServeOnly bool
	// Takeover claims the PIN even when the session's generation is ahead of
	// this machine's — the one authorized promotion (plan 25), set only by
	// `slopball box add`. Without it a fresh box (generation 0) is demoted to a
	// client and never hosts, never seeds canonical, never runs DevCommand.
	Takeover bool
	// MirrorURL is the optional durability remote (wizard / --mirror). Empty
	// disables mirroring. The push token is always read from GITHUB_TOKEN /
	// GH_TOKEN on this machine — never from the control plane.
	MirrorURL string
	// BoxBoot is set when this process is the internal `_host` boot path — the
	// dedicated box container. It publishes the session-network exec service so
	// members can `slopball box run` without ssh.
	BoxBoot bool
}

// Fleet role keys, shared with controlplane.ConductorRecord.Roles and the
// first-run plan so all three name the same three roles.
const (
	roleMerger  = "merger"
	roleWatcher = "error-watcher"
	roleSetup   = "setup"
)

var fleetRoles = []string{roleMerger, roleWatcher, roleSetup}

// resolveBrains picks one harness client per role. Precedence per role:
// the Brains test seam → the role's own Agents entry → the shared Brain →
// Conductor/Model. A role whose chosen CLI is missing on this machine is
// reported and left nil (mechanical), never substituted with another provider.
func resolveBrains(opt Options, log *logx.Logger) map[string]*harness.Client {
	shared := opt.Brain
	if shared == nil {
		shared = harness.Lookup(opt.Conductor, opt.Model)
	}
	out := make(map[string]*harness.Client, len(fleetRoles))
	for _, role := range fleetRoles {
		if c, ok := opt.Brains[role]; ok {
			out[role] = c
			continue
		}
		a, ok := opt.Agents[role]
		if !ok || a.Harness == "" {
			out[role] = shared
			continue
		}
		if !harness.IsAvailable(a.Harness) {
			log.Warnf("role %s asked for %s, which is not installed here — running it mechanically. "+
				"Install %s (or elect a machine that has it) to restore the session's choice.", role, a.Harness, a.Harness)
			out[role] = nil
			continue
		}
		out[role] = harness.Lookup(a.Harness, a.Model)
	}
	return out
}

func harnessName(c *harness.Client) string {
	if c == nil {
		return ""
	}
	return string(c.Name)
}

// publishAgents records the session's fleet composition on the control plane so
// a later elector reproduces it. Fire-and-forget: this is discovery state, and
// a session must not fail to start because it could not be written.
func publishAgents(ctx context.Context, client *controlplane.Client, pin string, opt Options) {
	roles := map[string]controlplane.RoleAgent{}
	for _, role := range fleetRoles {
		switch a, ok := opt.Agents[role]; {
		case ok && a.Harness != "":
			roles[role] = a
		case opt.Conductor != "":
			roles[role] = controlplane.RoleAgent{Harness: opt.Conductor, Model: opt.Model}
		}
	}
	if len(roles) == 0 || client == nil {
		return
	}
	primary := roles[roleMerger]
	_ = client.PutConductor(ctx, pin, controlplane.ConductorRecord{
		Harness: primary.Harness, Model: primary.Model, Roles: roles,
	})
}

// Running is a live hosted session.
type Running struct {
	PIN     string
	Host    *canonical.Host
	Fleet   *conductor.Fleet
	Runtime *runtime.Reconciler
	Dev     *devserver.Supervisor
	Control *controlplane.Client
	GitURL  string
	// GitDirect is the machine address the session git server also answers on,
	// republished on every heartbeat. Leaving it off the heartbeat would blank
	// it in the control plane two seconds after it was announced, and the
	// direct path would silently never be taken.
	GitDirect  string
	DemoURL    string
	ControlURL string
	MemberID   string
	WorkDir    string
	Branch     string
	Machine    string // hostname this host claimed under
	// InstallCommand is the operator override from Options (may be empty —
	// post-merge then re-detects). Stored so the host tick reuses the same
	// command instead of silently falling back to DetectInstall.
	InstallCommand []string
	// Placement holds this host's leases for the services it serves (plan 30).
	Placement *placement.Loop
	// sessionNet is what the git server joined, reused for the dev holder
	// (plan 41). Nil when no relay is configured — then dev keeps publishing
	// today's machine address, byte for byte.
	sessionNet  *gitserver.SessionNet
	bind        string
	devHolder   *devserver.Holder
	devHolderM  sync.Mutex
	execHolder  *boxexec.Holder
	execHolderM sync.Mutex
	gen         atomic.Int64 // control-plane generation this host writes at
	logs        *devserver.LogBuffer
	log         *logx.Logger
	stopHB      chan struct{}
	demoted     chan struct{} // closed when this machine must stop hosting
	stopReason  atomic.Value  // string: why demoted closed, for the operator-facing message
	Outbox      *controlplane.Outbox
	// stoppedGitPublish is set once we stop depositing the git endpoint after
	// losing the lease, so the stand-down log line fires once (ticket 18).
	stoppedGitPublish atomic.Bool
}

// StopReason explains why Demoted fired. The two causes need different advice —
// a session that moved is one you can `slopball join`, an expired one is not —
// so the host loop reads this rather than assuming a takeover.
func (r *Running) StopReason() string {
	s, _ := r.stopReason.Load().(string)
	return s
}

// stopHosting closes the demoted channel once, recording why.
func (r *Running) stopHosting(reason string) {
	select {
	case <-r.demoted:
		return // already stopping; first reason wins
	default:
	}
	r.stopReason.Store(reason)
	close(r.demoted)
}

// Generation is the control-plane generation this host writes its endpoints at.
// It is not fixed for the life of the process: `box add` boots the box (which
// takes over at generation N) and then flips the cutover from the operator's
// laptop, which lands the same host on N+1.
func (r *Running) Generation() int { return int(r.gen.Load()) }

// Start creates canonical, serves git, claims the PIN on the control plane,
// announces endpoints, and launches the heartbeat. Caller ticks the fleet.
//
// An empty opt.PIN is a create: the control plane mints the name and this
// process uses what comes back (abuse-surface ticket 11). A handed PIN
// (box boot, resume, takeover) is validated and presented as before.
func Start(ctx context.Context, opt Options) (*Running, error) {
	pin := opt.PIN
	handed := pin != ""
	if handed {
		if err := controlplane.ValidatePIN(pin); err != nil {
			return nil, err
		}
	}

	log := logx.New("host")
	var localGen int
	if handed {
		localGen = syncengine.LoadCursors(session.ForPin(pin).Cursors).Generation
	}

	client := opt.Control
	if client == nil {
		client = controlplane.NewClient(controlplane.BaseURL(""))
	}

	// Say out loud whether the control plane answered. Printing its URL and
	// carrying on (the old behaviour) left the operator unable to tell a live
	// session from one nobody can ever join.
	log.Infof("control plane %s — connecting", client.Base)
	hCtx, hCancel := context.WithTimeout(ctx, 10*time.Second)
	health, herr := client.Health(hCtx)
	hCancel()
	if herr != nil {
		return nil, fmt.Errorf("control plane %s unreachable: %w", client.Base, herr)
	}
	log.Infof("control plane ok — %s (db %s)", client.Base, health.DB)

	// An invited box is handed its secret via the environment (plan 44 ticket
	// 05). Plant it before Claim so Takeover + JoinMember can present it as
	// Bearer rather than trying to self-register.
	if secret := strings.TrimSpace(os.Getenv("SLOPBALL_MEMBER_SECRET")); secret != "" {
		if !handed {
			return nil, fmt.Errorf("SLOPBALL_MEMBER_SECRET set but no PIN was handed — box boot requires --pin")
		}
		id := strings.TrimSpace(os.Getenv("SLOPBALL_MEMBER_ID"))
		if err := session.WriteMembership(pin, id, secret); err != nil {
			return nil, fmt.Errorf("plant invited membership: %w", err)
		}
		// Tell the client outright rather than relying on it re-reading disk.
		// A client pins the identity it was last minted, so a client that has
		// already claimed something would otherwise keep presenting that and
		// silently ignore the identity this box was invited under.
		client.RememberMembership(pin, id, secret)
	}

	// A control plane on another machine means peers may want a *direct*
	// session-network dial — BindForControl is the sole producer of that mode
	// ("auto" when remote and something routable exists). The plain git HTTP
	// listener is always loopback; what gets published into the control plane
	// is the session address (or the direct address), never the plain one.
	bind := netbind.BindForControl(client.Base)
	remoteControl := !netbind.LoopbackURL(client.Base)
	switch {
	case bind == "auto":
		ip, _ := netbind.AdvertiseHostMode("auto")
		log.Infof("control plane is remote — direct session dials will use %s; plain git stays on loopback", ip)
	case bind == "" && remoteControl:
		_, err := netbind.RoutableIP()
		log.Warnf("control plane is remote but this machine has no routable address (%v) — no direct dial; clients reach git via the relay", err)
	}

	hostname, _ := os.Hostname()
	stack := detect.Probe().DefaultStack()
	name := defaultMemberName(opt)
	memberRole := controlplane.RoleHost
	if opt.ServeOnly {
		memberRole = controlplane.RoleBox
	}
	overlay := ""
	if pin != "" {
		overlay = "slop-" + pin + ".host"
	}
	// An empty-disk TAKEOVER seeds before it claims. The claim demotes the
	// incumbent — deletes its leases, and it stands down at once, taking its
	// relay registration with it — and the incumbent's canonical is exactly
	// what an empty disk has to clone. Seeding after the claim raced the
	// stand-down and lost: session wioqg5's replacement box died with
	// "Recv failure: Connection reset by peer" mid-clone. So the clone lands
	// first, on a session that is still being served, and the switch below
	// then finds a canonical on disk and resumes it.
	if opt.Takeover && pin != "" && opt.SeedURL == "" && !canonicalExists(session.ForPin(pin).Canonical) {
		seedURL, err := reseedSource(ctx, client, pin, session.ReadMemberID(pin), log)
		if err != nil {
			return nil, err
		}
		if seedURL != "" {
			log.Infof("no canonical on disk for %s — reseeding from the session's freshest replica at %s before taking over", pin, seedURL)
			if _, err := seedFromRemote(ctx, session.ForPin(pin).Canonical, pin, seedURL); err != nil {
				log.Errorf("canonical setup failed for %s: %v", pin, err)
				return nil, err
			}
		}
	}

	claim, err := client.Claim(ctx, controlplane.ClaimRequest{
		PIN: pin, HostMachine: hostname, LocalGeneration: localGen,
		OverlayAddr: overlay,
		Stack:       &controlplane.StackInfo{Runtime: stack.Runtime, Version: stack.Version, PkgMgr: stack.PkgMgr},
		Harness:     opt.Conductor, SeedURL: opt.SeedURL,
		Takeover: opt.Takeover, MemberName: name, MemberRole: memberRole,
	})
	if err != nil {
		return nil, fmt.Errorf("control plane claim: %w", err)
	}
	pin = claim.Session.PIN
	if err := controlplane.ValidatePIN(pin); err != nil {
		return nil, fmt.Errorf("control plane returned an invalid pin %q: %w", pin, err)
	}
	paths := session.ForPin(pin)
	if err := os.MkdirAll(paths.Root, 0o755); err != nil {
		return nil, err
	}
	if claim.JoinOnly {
		return nil, &ErrJoinAsClient{Message: claim.Message, Session: claim.Session}
	}
	if claim.Takeover {
		log.Infof("took over hosting %s at generation %d — the previous host is now demoted", pin, claim.Session.Generation)
	}
	if claim.Created {
		log.Infof("session minted as %s", pin)
	}

	var host *canonical.Host
	switch {
	case canonicalExists(paths.Canonical):
		log.Infof("resuming existing canonical for %s at %s", pin, paths.Canonical)
		host, err = canonical.Open(paths.Canonical, pin)
		if err == nil {
			err = ensureWork(ctx, host)
		}
	case opt.SeedURL != "":
		log.Infof("seeding canonical for %s from %s", pin, opt.SeedURL)
		host, err = seedFromRemote(ctx, paths.Canonical, pin, opt.SeedURL)
	default:
		// No canonical on disk. On a container that is the ROUTINE case — the
		// disk is ephemeral and every sleep wakes to a fresh image — so the
		// question is never "create?" but "has this session got history
		// somewhere else?". Creating on top of a session that does is the one
		// failure reachability can never see: an empty repo answers perfectly.
		var seedURL string
		seedURL, err = reseedSource(ctx, client, pin, claim.MemberID, log)
		switch {
		case err != nil:
			// Loud and fatal. The git lease and the git endpoint are both taken
			// further down, so nothing has been published on the way here.
			return nil, err
		case seedURL != "":
			log.Infof("no canonical on disk for %s — reseeding from the session's freshest replica at %s", pin, seedURL)
			host, err = seedFromRemote(ctx, paths.Canonical, pin, seedURL)
		default:
			log.Infof("creating fresh canonical for %s at %s", pin, paths.Canonical)
			host, err = canonical.Create(ctx, paths.Canonical, pin)
		}
	}
	if err != nil {
		log.Errorf("canonical setup failed for %s: %v", pin, err)
		return nil, err
	}
	if opt.SeedDir != "" {
		if err := seedFromDir(ctx, host, opt.SeedDir); err != nil {
			_ = host.Close(ctx)
			return nil, err
		}
	}
	// Nothing below this line may run against a historyless canonical: the next
	// hundred lines publish the git endpoint and claim the git lease, which is
	// how the rest of the session learns where its truth lives.
	if err := requireHistory(ctx, host); err != nil {
		_ = host.Close(ctx)
		return nil, err
	}
	host.Bind = bind
	// Join the session network when this control plane runs a relay (plan 09).
	// The published address then names the session, not this machine — which is
	// what makes a client-isolated network joinable and a lease migration
	// invisible to every client's `origin`.
	var sessNet *gitserver.SessionNet
	if sn, err := sessionNetFor(ctx, claim.Session, bind, client.HolderTicket(claim.Session.PIN, "git")); err != nil {
		log.Warnf("not joining the session network: %v — clients must reach %s directly", err, bind)
	} else if sn != nil {
		host.Session = sn
		sessNet = sn
	}
	gitURL, err := host.StartServer()
	if err != nil && opt.Takeover && sessNet != nil && errors.Is(err, sessionnet.ErrHolderBusy) {
		// A takeover has just DEMOTED the incumbent — the claim above deleted
		// its leases — and it stands down on its next member cycle, taking its
		// relay registration with it. Until then the relay is right to refuse
		// us ("first live holder wins", ticket 15). This is a wait for that
		// stand-down, not a retry loop: it ends when the slot frees or when the
		// lease TTL has passed — after which the incumbent is not standing down
		// and the refusal is real. Session wioqg5's replacement box died here.
		gitURL, err = waitForIncumbentToStandDown(ctx, log, host, time.Duration(controlplane.DefaultLeaseTTL)*time.Second)
	}
	if err != nil {
		// No bare-path fallback. `file://<host.Bare>` names a directory that
		// exists on this machine only, so publishing it makes the session read
		// as live while every joiner dies on "does not appear to be a git
		// repository" — with the real cause (a refused relay registration, a
		// listener that would not bind) only in this process's own log. Since
		// plan 45 the relay verifies tickets, so a refusal is a configuration
		// away rather than a freak event. Fail here, where the reason is known.
		_ = host.Close(ctx)
		return nil, fmt.Errorf("serve session git for %s: %w", pin, err)
	}
	log.Infof("serving session git at %s", gitURL)
	if sessionURL := host.SessionRemoteURL(); sessionURL != "" {
		log.Infof("session git also reachable at %s (session network — no direct route needed)", sessionURL)
		gitURL = sessionURL
	}
	if remoteControl && netbind.LoopbackURL(gitURL) {
		log.Errorf("git endpoint %s is loopback but the control plane %s is remote: clients on other machines will fail to clone. The plain listener is always loopback — this session needs a relay (BindForControl publishes a direct address when something routable exists).", gitURL, client.Base)
	}

	// Land the brief on main before the client tree is cloned, so this machine's
	// own agent contract already quotes it (plan 28 §2/§5). A brief already on
	// main wins — the session agreed on that one.
	if opt.Brief != "" {
		if err := conductor.WriteBrief(ctx, host, sbGit.SessionIdentity("host", pin), opt.Brief); err != nil {
			log.Warnf("could not record the brief on main (the setup role has nothing to act on): %v", err)
		} else {
			log.Infof("brief recorded on main: %s", opt.Brief)
		}
	}

	// Contracts follow the brief onto main, before the client tree is cloned and
	// before the setup role runs (plan 31). On main rather than only on client
	// branches, so the scaffolding agent builds against the protocol its
	// teammates follow — and so a scaffold that writes its own AGENTS.md
	// collides while the setup role can still reconcile the two.
	if err := conductor.WriteContracts(ctx, host, sbGit.SessionIdentity("host", pin)); err != nil {
		log.Warnf("could not record the agent contracts on main (clients still get them locally): %v", err)
	}

	// Both writes land on main from their own temp clone, which leaves the demo
	// mirror an ancestor of main — and anything that then commits in the mirror
	// and pushes is rejected as non-fast-forward. The tick loop would fix this
	// two seconds later; that is too late for the dev-server detection and the
	// run-file read that happen below.
	if err := host.SyncWorkToMain(ctx); err != nil {
		log.Warnf("could not refresh the demo mirror after recording session files: %v", err)
	}

	gen := claim.Session.Generation
	_ = syncengine.SaveCursors(paths.Cursors, syncengine.Cursors{Endpoint: gitURL, Generation: gen})

	// Durability mirror URL: wizard/--mirror, or the session's published
	// EndpointMirror on re-host. Token is attached later from this machine only.
	mirrorURL := opt.MirrorURL
	if mirrorURL == "" {
		if sess, err := client.Session(ctx, pin); err == nil {
			// raw endpoint ok: mirror is a GitHub (or similar) HTTPS URL published
			// for durability — never a slop:// session address, and never dialled
			// as a session service; it is handed to git push as a remote.
			if ep, ok := sess.Endpoints[controlplane.EndpointMirror]; ok {
				mirrorURL = ep.URL
			}
		}
	}

	if err := announceEndpoints(ctx, client, pin, gen, gitURL, host.Srv.SessionDirect(), mirrorURL); err != nil {
		if errors.Is(err, controlplane.ErrDemoted) {
			_ = host.Close(ctx)
			return nil, &ErrJoinAsClient{
				Message: fmt.Sprintf("session %s moved while announcing — joining as a client", pin),
			}
		}
		log.Warnf("announce endpoints: %v", err)
	}

	branch := "client/" + name
	if err := ensureClientTree(ctx, host, gitURL, paths, branch, name, pin, gen); err != nil {
		log.Warnf("client dev tree setup failed (host still serves; edit canonical/work as a fallback): %v", err)
		branch = ""
	} else {
		log.Debugf("client dev tree ready at %s on %s", paths.Work, branch)
	}

	meta := session.Session{
		PIN:             pin,
		Role:            session.RoleHost,
		Branch:          branch,
		HostOverlayAddr: "slop-" + pin + ".host",
		Capability:      detect.Probe(),
	}
	if err := meta.Save(); err != nil {
		_ = host.Close(ctx)
		return nil, err
	}
	workTree := host.Work
	if branch != "" {
		workTree = paths.Work
	}

	logs := &devserver.LogBuffer{}
	if host.Srv != nil {
		host.Srv.Logs = logs.String
		host.Srv.LogsSelect = func(stream, phase string) string {
			return logs.Select(devserver.Stream(stream), devserver.Phase(phase))
		}
		// …and followable, which is what the console's dev tab reads.
		host.Srv.LogsSince = logs.Since
		host.Srv.LogsSubscribe = logs.Subscribe
	}
	// How this project installs and starts: explicit flag → the project's own
	// committed .slopball/run.json → detection (plan 29). The committed file is
	// why a migrated host supervises the same process the dead one did, with no
	// flags anywhere.
	declared := runfile.ReadFromMain(ctx, host)
	installCmd := runfile.Resolve(opt.InstallCommand, declared.Install, nil)
	devCmd := runfile.Resolve(opt.DevCommand, declared.Dev, nil)
	if len(opt.DevCommand) == 0 && len(declared.Dev) > 0 {
		log.Infof("dev command from the project itself (%s): %s", runfile.Path, strings.Join(devCmd, " "))
	} else if len(opt.DevCommand) > 0 || len(opt.InstallCommand) > 0 {
		// An operator who typed --dev/--install has told us a project fact.
		// Record it in canonical so the NEXT host — a migration survivor, a box,
		// plan 30's next lease holder — supervises the same process without the
		// flag. Today that command dies with the machine that was given it.
		if err := runfile.Commit(ctx, host, sbGit.SessionIdentity("host", pin), installCmd, devCmd); err != nil {
			log.Warnf("could not record %s (a future host will re-detect): %v", runfile.Path, err)
		}
	}
	dev := &devserver.Supervisor{WorkDir: host.Work, Logs: logs}

	id := sbGit.SessionIdentity("host", pin)
	// One client per role (plan 29). Each resolves independently so a session
	// can run merger=claude / setup=codex; a role the caller said nothing about
	// falls back to the shared Brain (the "all roles" shorthand). A role whose
	// chosen CLI is missing here is REPORTED and runs mechanically — never
	// quietly swapped for a different provider than the session chose.
	brains := resolveBrains(opt, log)
	brain := brains[roleMerger]
	var hName string
	if brain != nil {
		hName = string(brain.Name)
	}
	// Durability mirror (plan 15): optional, host-only, off the live sync path.
	// URL resolved above (Options or published EndpointMirror); token from this machine.
	mirror := &durability.Mirror{Bare: host.Bare, Config: durability.LoadConfig(mirrorURL)}
	if mirror.Enabled() {
		log.Infof("durability mirror enabled → %s", mirror.Config.RemoteURL)
	}
	merger := &conductor.Merger{Host: host, ID: id, Resolve: conductor.HarnessResolver(brains[roleMerger]), Harness: harnessName(brains[roleMerger]), Control: client, PIN: pin, Mirror: mirror}
	rt := &runtime.Reconciler{WorkDir: host.Work, Dev: dev, Control: client, PIN: pin, Generation: gen}
	// The health probe follows the endpoint the reconciler announces, so it
	// cannot end up watching a port nothing was ever meant to serve — and it
	// stays disabled until the project actually declares one.
	watcher := &conductor.ErrorWatcher{Host: host, Logs: logs, ID: id, Fix: conductor.HarnessFixer(brains[roleWatcher]), Harness: harnessName(brains[roleWatcher]), Control: client, PIN: pin,
		Health: conductor.HTTPHealthDynamic(rt.DevURL)}
	setup := &conductor.Setup{
		Host: host, ID: id, Agent: conductor.HarnessScaffolder(brains[roleSetup]),
		Harness: harnessName(brains[roleSetup]), Control: client, PIN: pin,
	}
	// The fleet composition is session state: publish it where a machine that
	// elects itself conductor later can read it back (plan 29).
	publishAgents(ctx, client, pin, opt)
	fleet := &conductor.Fleet{
		// merger and watcher hold host.Work, which the host loop resets with
		// SyncWorkToMain the moment the tick returns, so the tick waits for them.
		// setup scaffolds for minutes in a private temp clone — detached, or the
		// merge hot path stops for as long as an agent takes to run
		// create-next-app.
		Roles:    []conductor.Role{merger, watcher},
		Detached: []conductor.Role{setup},
		After:    []conductor.Role{rt},
	}

	// A serve-only host IS the cloud box: it serves canonical and tracks main
	// under no harness login (§10). Registering it as an ordinary host left
	// controlplane.RoleBox assigned by nothing at all, which silently disabled
	// both halves of plan 30's ranking — the box was never excluded from the
	// conductor, and never outranked a laptop for git or dev.
	// memberRole was already decided before Claim so a fresh creator is minted
	// with the right role (plan 44).
	role := memberRole
	memberID := claim.MemberID
	if memberID == "" {
		var err error
		memberID, err = client.JoinMember(ctx, pin, controlplane.MemberJoinRequest{
			Name: name, Role: role, Branch: branch, Machine: hostname,
			Harness: hName, Capability: controlplane.CapFromProfile(detect.Probe()),
		})
		if err != nil {
			log.Warnf("member join: %v", err)
		}
	} else {
		// Creator was minted by the claim (plan 44). Fill presence fields the
		// claim did not know yet — capability, branch, harness — without minting
		// a second member.
		if err := client.Heartbeat(ctx, pin, memberID, controlplane.MemberUpdate{
			Branch: branch, Harness: hName, Role: role,
			Capability: controlplane.CapFromProfile(detect.Probe()),
		}); err != nil {
			log.Warnf("member update after claim: %v", err)
		}
	}

	r := &Running{
		PIN: pin, Host: host, Fleet: fleet, Runtime: rt, Dev: dev,
		Control: client, GitURL: gitURL, GitDirect: host.Srv.SessionDirect(),
		DemoURL:    "file://" + host.Work,
		ControlURL: client.Base, MemberID: memberID,
		WorkDir: workTree, Branch: branch, Machine: hostname,
		InstallCommand: installCmd,
		logs:           logs,
		log:            log,
		stopHB:         make(chan struct{}), demoted: make(chan struct{}),
		sessionNet: sessNet, bind: bind,
	}
	r.gen.Store(int64(gen))

	// Automatic placement (plan 30): this machine holds a lease for each service
	// it actually serves, and renews it every tick. When it goes away the leases
	// expire and a survivor takes them — no human, no election ceremony.
	// Record which member this machine is, so `slopball take` / `hand-off` can
	// speak for it later without re-joining (plan 30).
	if memberID != "" {
		meta.MemberID = memberID
		_ = meta.Save()
	}
	r.Placement = &placement.Loop{
		Control: client, PIN: pin, MemberID: memberID, Name: name, Machine: hostname,
		// The dev publication rides the LEASE, not the process: losing dev must
		// take the session-network registration down with it.
		Stop: func(ctx context.Context, service string) error {
			if service == controlplane.ServiceDev {
				r.stopDevHolder()
			}
			if service == controlplane.ServiceGit {
				r.stopGitHolder()
			}
			return nil
		},
		OnChange: func(service, detail string) { log.Infof("placement: %s %s", service, detail) },
		// What this process can actually run, as opposed to what it ranks first
		// for. Start for git republishes the session-network tunnel after a
		// stand-down; the loopback listener is already up from StartServer.
		Start: func(ctx context.Context, service string) error {
			if service == controlplane.ServiceGit {
				return r.startGitHolder()
			}
			return nil
		},
		Serves: func(service string) bool {
			switch service {
			case controlplane.ServiceGit:
				return true // this process is serving canonical right now
			case controlplane.ServiceDev:
				return r.Dev != nil && len(r.Dev.Command) > 0
			case controlplane.ServiceConductor:
				return !opt.ServeOnly // no harness login on a box (§10)
			}
			return false
		},
	}
	r.claimServedLeases(ctx, opt)

	// Install + supervise now when the commands are already known. The first-run
	// wizard (plan 29) instead calls StartDev *after* the setup role scaffolds,
	// because DetectInstall/DetectDev cannot answer for a project that does not
	// exist yet — an empty tree gets `npm run dev` exit 127 and nothing restarts it.
	if len(installCmd) > 0 || len(devCmd) > 0 {
		if err := r.StartDev(ctx, installCmd, devCmd); err != nil {
			_ = host.Close(ctx)
			return nil, err
		}
	}
	// Plan 43: one stream down, one cycle up.
	r.Control.Watch(ctx, pin)
	if r.MemberID != "" {
		r.Outbox = controlplane.NewOutbox(r.Control, pin, r.MemberID)
		// Registered on the client, so the conductor fleet this process runs
		// deposits into the same cycle instead of opening its own connection
		// every 2s.
		r.Control.RegisterOutbox(pin, r.Outbox)
		if r.Placement != nil {
			r.Placement.Outbox = r.Outbox
		}
	}
	go r.heartbeat(ctx, logs)
	if err := session.WriteLive(meta); err != nil {
		log.Warnf("could not write live marker: %v", err)
	}
	if opt.BoxBoot {
		r.startExecHolder(ctx, log)
	}
	return r, nil
}

// AdoptDeclaredRun picks up `.slopball/run.json` when it lands on main *after*
// this host started, and is a no-op once a dev server is supervised here.
//
// runfile.ReadFromMain used to be consulted exactly once, at Start. That held
// while a host only ever booted from a canonical that already carried a project.
// It stops holding the moment a host comes up against an empty canonical and the
// project arrives afterwards — which is every remote-first session. The run file
// is committed precisely so a later host inherits the commands; a host that only
// looks once is not a later host, it is the same one going deaf.
func (r *Running) AdoptDeclaredRun(ctx context.Context) error {
	if r.Dev != nil && len(r.Dev.Command) > 0 {
		return nil // already supervising; main advancing is PostMergeInstall's business
	}
	declared := runfile.ReadFromMain(ctx, r.Host)
	if len(declared.Dev) == 0 {
		return nil
	}
	log := r.log
	if log == nil {
		log = logx.New("host")
	}
	log.Infof("the project now declares its own run commands (%s) — installing and supervising: %s",
		runfile.Path, strings.Join(declared.Dev, " "))
	return r.StartDev(ctx, runfile.Resolve(r.InstallCommand, declared.Install, nil), declared.Dev)
}

// StartDev installs dependencies and then supervises the dev server, in that
// order (plan 26) and in the canonical work tree. Safe to call after Start when
// the commands were not knowable at start time; both may be empty.
//
// Errors only for an interrupted install: a failed install still starts the dev
// server (the existing tolerance), because a visible broken dev server beats a
// session that refused to come up.
func (r *Running) StartDev(ctx context.Context, install, dev []string) error {
	log := r.log
	if log == nil {
		log = logx.New("host")
	}
	// First-boot install (plan 26): PostMergeInstall is diff-gated and never
	// fires for a lockfile that was already in the seed. Run here, in the same
	// tree the supervisor uses, before the supervised process starts — and
	// stream into the same LogBuffer so /logs + docker logs see it.
	if len(install) > 0 || len(dev) > 0 {
		cmd := install
		if len(cmd) == 0 {
			cmd = devserver.DetectInstall(r.Host.Work)
		}
		if len(cmd) > 0 {
			log.Infof("installing deps: %s (in %s)", strings.Join(cmd, " "), r.Host.Work)
			// Pass the resolved cmd, not the argument — otherwise Install re-runs
			// detection and the line we just logged is only incidentally the
			// command that runs.
			// Phase-tagged: an install's output is not the product failing, and
			// the watcher filters the whole phase out.
			if err := devserver.Install(ctx, r.Host.Work, cmd,
				r.logs.Writer(devserver.StreamStderr, devserver.PhaseInstall)); err != nil {
				// exec reports a context kill as "signal: killed" (*exec.ExitError),
				// which never wraps context.Canceled — so ask the context, not the
				// error. Otherwise a Ctrl-C mid-install falls through to starting
				// the dev server and joining the fleet during shutdown.
				if ctx.Err() != nil {
					return fmt.Errorf("install interrupted: %w", ctx.Err())
				}
				log.Warnf("install failed (continuing): %v", err)
			}
		}
		r.InstallCommand = cmd
	}
	if len(dev) == 0 {
		return nil
	}
	r.Dev.Command = dev
	if err := r.Dev.Start(ctx); err != nil {
		log.Warnf("dev server did not start (continuing): %v", err)
		return nil
	}
	log.Infof("dev server started: %s (in %s)", strings.Join(dev, " "), r.Host.Work)
	go watchDevStartup(ctx, r.Dev, dev, r.logs, log)
	r.startDevHolder(ctx, log)
	// On this machine the site is the dev process itself, not a forwarder to
	// one — 127.0.0.1 rather than localhost because a dev server that bound
	// only v4 is unreachable on a box where localhost resolves ::1 first.
	if err := session.PublishDevURL(r.PIN, fmt.Sprintf("http://127.0.0.1:%d", runtime.DevPort)); err != nil {
		log.Warnf("could not publish the dev URL for `slopball site`: %v", err)
	}
	if r.Runtime != nil {
		// A seeded repo usually already declares PORT, and main may not advance
		// for minutes — publish the dev URL now rather than when it happens to.
		r.Runtime.AnnounceDev(ctx)
	}
	return nil
}

// KeepDevAlive is the host tick's liveness pass (plan 34): a supervised dev
// process that exited on its own is started again, and the restart is said out
// loud. Nothing else in slopball ever asked whether the process was alive, so a
// dev server that fell over — because the project had not merged yet, because
// the box OOM-killed it, because a bad commit crashed it — stayed dead for the
// rest of the session while the host went on ticking healthily.
//
// It acts on one signal and one only: the OS process exited. A live process that
// has not answered yet is a cold vite/next build, and touching it is the failure
// mode this plan exists to avoid — see devserver's liveness doc.
//
// mainAdvanced clears a tripped breaker. A new commit is a new reason to believe
// a dev command might work now: the project it was waiting for may have just
// landed, or the commit that broke it may have just been fixed. It is not itself
// a restart trigger — that is every tick, which is what stops the demo staying
// down through the quiet stretch of a session when nobody is merging.
func (r *Running) KeepDevAlive(ctx context.Context, mainAdvanced bool) {
	if r.Dev == nil {
		return
	}
	if mainAdvanced {
		r.Dev.ClearGiveUp()
	}
	// Re-announce while the process is alive: ticket 21 only publishes once
	// something is listening, and a cold vite/next start becomes listening
	// after the first tick.
	if r.Dev.Running() && r.Runtime != nil {
		r.Runtime.AnnounceDev(ctx)
	}
	if !r.Dev.NeedsRestart() {
		return
	}
	log := r.log
	if log == nil {
		log = logx.New("host")
	}
	command := strings.Join(r.Dev.Command, " ")
	lasted := r.Dev.RanFor().Round(time.Millisecond)
	err := r.Dev.Restart(ctx)
	switch {
	case errors.Is(err, devserver.ErrGaveUp):
		// Loud, once, and in the LogBuffer as well as the host's stderr — the
		// error-watcher reads /logs, not the terminal somebody walked away from.
		msg := fmt.Sprintf("dev server %q exited immediately %d times in a row — not restarting it again. "+
			"Fix the command (or the project) and merge; the next commit on main gives it another go. Its last output:\n%s",
			command, r.Dev.ShortExits(), r.logTail(20))
		log.Errorf("%s", msg)
		r.writeDevLog("slopball: " + msg)
	case err != nil:
		log.Warnf("dev server %q could not be restarted: %v", command, err)
	default:
		msg := fmt.Sprintf("dev server %q had exited after %s — restarted it (attempt %d)",
			command, lasted, r.Dev.ShortExits())
		log.Infof("%s", msg)
		r.writeDevLog("slopball: " + msg)
		// A new process behind the same port: re-announce, or slopdebug/monitor
		// and the error-watcher stay pointed at a port nothing is listening on.
		if r.Runtime != nil {
			r.Runtime.AnnounceDev(ctx)
		}
		go watchDevStartup(ctx, r.Dev, r.Dev.Command, r.devLogs(), log)
	}
}

// writeDevLog puts a slopball-side line in front of the error-watcher, which
// reads the dev LogBuffer (/logs) and never the host's stderr.
func (r *Running) writeDevLog(line string) {
	if b := r.devLogs(); b != nil {
		// Tagged as ours: one of these messages quotes logTail(20) back into the
		// buffer, so an untagged write would hand the watcher its own narration
		// as a fresh error and it would re-fire on itself forever.
		b.WriteLine(devserver.StreamSlopball, devserver.PhaseDev, line)
	}
}

func (r *Running) devLogs() *devserver.LogBuffer {
	if r.logs != nil {
		return r.logs
	}
	if r.Dev != nil {
		return r.Dev.Logs
	}
	return nil
}

func (r *Running) logTail(n int) string {
	if b := r.devLogs(); b != nil {
		return b.Tail(n)
	}
	return ""
}

// watchDevStartup turns a dev server that dies into one line in the log. On a
// cloud box that log is `docker logs <container>`, which is the only place
// anyone can see why nothing answers on :3000. Restarting it is KeepDevAlive's
// job, on the host tick, bounded so a crash loop cannot replace a visible
// failure with an endless one.
func watchDevStartup(ctx context.Context, dev *devserver.Supervisor, command []string, logs *devserver.LogBuffer, log *logx.Logger) {
	select {
	case <-ctx.Done():
		return
	case <-dev.Done():
	}
	err := dev.Err()
	if err == nil { // stopped on purpose (Reload / shutdown)
		return
	}
	log.Warnf("dev server %q exited after %s: %v — nothing will answer on its port. Its last output:\n%s",
		strings.Join(command, " "), dev.RanFor().Round(time.Millisecond), err, logs.Tail(20))
}

func announceEndpoints(ctx context.Context, client *controlplane.Client, pin string, gen int, gitURL, gitDirect, mirrorURL string) error {
	gh, gp := splitURL(gitURL)
	if err := client.PutEndpoint(ctx, pin, controlplane.EndpointGit, controlplane.EndpointPut{
		URL: gitURL, Host: gh, Port: gp, Direct: gitDirect, Source: "host", Generation: gen,
	}); err != nil {
		return err
	}
	logsURL := logsURLFromGit(gitURL)
	if logsURL != "" {
		if err := client.PutEndpoint(ctx, pin, controlplane.EndpointLogs, controlplane.EndpointPut{
			URL: logsURL, Generation: gen, Source: "host",
		}); err != nil {
			return err
		}
	}
	// Durability mirror: publish the remote URL so migration/reconstruct can find
	// the safety net without an env var. URL only — a credential must never enter
	// the control plane, so Config.Token is deliberately not carried here.
	if cfg := durability.LoadConfig(mirrorURL); cfg.RemoteURL != "" {
		if err := client.PutEndpoint(ctx, pin, controlplane.EndpointMirror, controlplane.EndpointPut{
			URL: cfg.RemoteURL, Generation: gen, Source: "host",
		}); err != nil {
			return err
		}
	}
	return nil
}

func (r *Running) heartbeat(ctx context.Context, logs *devserver.LogBuffer) {
	t := time.NewTicker(controlplane.MemberCycle)
	defer t.Stop()
	log := logx.New("host")
	admissionLog := &admission.Log{Control: r.Control, PIN: r.PIN}
	updates := r.Control.Updates(r.PIN)
	for {
		select {
		case <-r.stopHB:
			return
		case <-ctx.Done():
			return
		case u, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			admissionLog.Stream(u.Events)
		case <-t.C:
			// With no stream, the frames that carry knocks are not arriving —
			// so a person standing at the door would otherwise go unmentioned
			// on exactly the paths (--once, --no-console, non-TTY) that have no
			// users tab to notice them on.
			if !r.Control.StreamLive(r.PIN) {
				admissionLog.Poll(ctx)
			}
			r.Control.FlushBuffer(context.Background())
			hbCtx, cancel := context.WithTimeout(context.Background(), controlplane.MemberCycle)
			err := r.cycle(hbCtx)
			cancel()
			if errors.Is(err, controlplane.ErrDemoted) {
				if gen, ok := r.adoptCutoverOntoSelf(context.Background()); ok {
					log.Infof("generation advanced to %d by a cutover that left us hosting — continuing as host", gen)
					continue
				}
				log.Warnf("demoted: control plane rejected stale generation %d — stopping host role", r.Generation())
				r.stopHosting("moved")
				return
			}
			if errors.Is(err, controlplane.ErrNoSession) || errors.Is(err, controlplane.ErrUnauthorized) {
				log.Warnf("session %s ended — stopping host role (%v); canonical is still at %s",
					r.PIN, err, r.Host.Root)
				r.stopHosting("expired")
				return
			}
			if err != nil {
				log.Warnf("member cycle: %v", err)
			} else {
				log.Infof("member cycle — git endpoint %s (gen %d)", r.GitURL, r.Generation())
			}
			_ = logs
		}
	}
}

// cycle is one member uplink: deposit endpoint + presence, renew held leases
// via the outbox, Flush, apply losses (plan 43).
func (r *Running) cycle(ctx context.Context) error {
	if r.Outbox == nil {
		// No member id yet — fall back to today's separate writes.
		if r.Placement == nil || r.Placement.HoldsService(controlplane.ServiceGit) {
			err := r.Control.PutEndpoint(ctx, r.PIN, controlplane.EndpointGit, controlplane.EndpointPut{
				URL: r.GitURL, Direct: r.GitDirect, Generation: r.Generation(), Source: "host-heartbeat",
			})
			if err != nil {
				return err
			}
			r.stoppedGitPublish.Store(false)
		} else if !r.stoppedGitPublish.Swap(true) {
			logx.New("host").Infof("%s: no longer hold git — stopping endpoint publication", r.PIN)
		}
		if r.MemberID != "" {
			_ = r.Control.Heartbeat(ctx, r.PIN, r.MemberID, controlplane.MemberUpdate{})
		}
		return nil
	}
	r.Outbox.SetGeneration(r.Generation())
	r.Outbox.SetMember(controlplane.MemberUpdate{})
	// Only the git-lease holder announces the endpoint. Publishing after a
	// stand-down would advertise a tunnel we just tore down (ticket 18).
	if r.Placement == nil || r.Placement.HoldsService(controlplane.ServiceGit) {
		r.Outbox.SetEndpoint(controlplane.EndpointGit, controlplane.EndpointPut{
			URL: r.GitURL, Direct: r.GitDirect, Generation: r.Generation(), Source: "host-heartbeat",
		})
		r.stoppedGitPublish.Store(false)
	} else if !r.stoppedGitPublish.Swap(true) {
		// Say so once — retrying every cycle would drown the log and look like
		// a flapping publisher rather than a deliberate stand-down.
		logx.New("host").Infof("%s: no longer hold git — stopping endpoint publication", r.PIN)
	}
	if r.Placement != nil {
		for _, svc := range controlplane.Services {
			if r.Placement.HoldsService(svc) {
				r.Outbox.Hold(svc)
			}
		}
	}
	res, err := r.Outbox.Flush(ctx)
	if err != nil {
		return err
	}
	if res.Conflict {
		return controlplane.ErrDemoted
	}
	if r.Placement != nil {
		r.Placement.ApplyLost(ctx, res.Lost)
	}
	return nil
}

// adoptCutoverOntoSelf decides whether a rejected generation means "someone took
// the host role" (demote) or "a cutover moved the session onto the canonical we
// are already serving" (keep hosting at the new generation).
//
// The second case is `slopball box add`: the box takes over from inside the
// container, then the operator's laptop flips the cutover — bumping a generation
// the box has no other way to learn. Reading that as a demotion killed the box's
// host role seconds after it started, with the control plane still pointing at
// it, so nothing ever served canonical or the dev server.
//
// All three conditions are required, and each rules out a real steal:
// host_machine still us (a takeover claim sets it to the new host in the same
// transaction as the bump), the git endpoint still ours, and that endpoint
// written by a cutover rather than by another host announcing. Without the last
// one, two hosts sharing a hostname could each keep re-adopting and fight over
// the endpoint forever.
func (r *Running) adoptCutoverOntoSelf(ctx context.Context) (int, bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sess, err := r.Control.Session(ctx, r.PIN)
	if err != nil {
		return 0, false
	}
	if sess.Generation <= r.Generation() || sess.HostMachine != r.Machine {
		return 0, false
	}
	// raw endpoint ok: compared against the string this host itself published.
	ep, ok := sess.Endpoints[controlplane.EndpointGit]
	if !ok || !sameURL(ep.URL, r.GitURL) || ep.Source != "cutover" {
		return 0, false
	}
	r.gen.Store(int64(sess.Generation))
	if r.Runtime != nil {
		r.Runtime.SetGeneration(sess.Generation)
	}
	_ = syncengine.SaveCursors(session.ForPin(r.PIN).Cursors, syncengine.Cursors{
		Endpoint: r.GitURL, Generation: sess.Generation,
	})
	return sess.Generation, true
}

func sameURL(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

// Demoted returns a channel closed when a heartbeat gets 409.
func (r *Running) Demoted() <-chan struct{} { return r.demoted }

func ensureClientTree(ctx context.Context, host *canonical.Host, gitURL string, paths session.Paths, branch, name, pin string, gen int) error {
	id := sbGit.SessionIdentity(name, pin)
	if _, err := os.Stat(filepath.Join(paths.Work, ".git")); err == nil {
		return contracts.Install(paths.Work, pin)
	}
	_ = host.CreateClientBranch(ctx, branch)
	if err := syncengine.CloneClient(ctx, host.Bare, paths.Work, branch, id); err != nil {
		return err
	}
	if err := contracts.Install(paths.Work, pin); err != nil {
		return err
	}
	_ = syncengine.SaveCursors(paths.Cursors, syncengine.Cursors{Endpoint: gitURL, Generation: gen})
	return nil
}

// claimServedLeases records what this host is already serving. It is a claim,
// not a race we might lose: a host that just took the PIN is by definition the
// member serving canonical, and the lease is how every other member learns that
// without asking. Best-effort — a session must not fail to start because the
// placement table could not be written.
func (r *Running) claimServedLeases(ctx context.Context, opt Options) {
	services := []string{controlplane.ServiceGit}
	if len(r.Dev.Command) > 0 {
		services = append(services, controlplane.ServiceDev)
	}
	if !opt.ServeOnly {
		services = append(services, controlplane.ServiceConductor)
	}
	for _, svc := range services {
		if err := r.Placement.Adopt(ctx, svc); err != nil {
			logx.New("host").Debugf("claim %s lease: %v", svc, err)
		}
	}
}

// Close stops the hosted session.
func (r *Running) Close(ctx context.Context) error {
	select {
	case <-r.stopHB:
	default:
		close(r.stopHB)
	}
	if r.Control != nil {
		r.Control.CloseWatch(r.PIN)
		r.Control.UnregisterOutbox(r.PIN)
	}
	// Hand the services back before leaving, so a survivor picks them up at
	// once rather than after a full lease TTL.
	if r.Placement != nil {
		r.Placement.ReleaseAll(context.Background())
	}
	if r.MemberID != "" && r.Control != nil {
		_ = r.Control.LeaveMember(context.Background(), r.PIN, r.MemberID)
	}
	r.stopDevHolder()
	r.stopExecHolder()
	_ = r.Dev.Stop()
	_ = session.ClearLive(r.PIN)
	return r.Host.Close(ctx)
}

// startDevHolder publishes the supervised dev server onto the session network
// (plan 41). git joined it and dev never did, so a member who joined from
// another network synced fine and then could not open the page.
//
// No relay configured → nothing happens and the machine address stays the
// endpoint, exactly as git behaves. LocalDevPort always resolves to the
// constant; the holder starts immediately and endpoint publication waits for
// something listening (ticket 21).
func (r *Running) startDevHolder(ctx context.Context, log *logx.Logger) {
	if r.sessionNet == nil || r.Runtime == nil {
		return
	}
	r.devHolderM.Lock()
	defer r.devHolderM.Unlock()
	if r.devHolder != nil {
		return
	}
	port, why := runtime.LocalDevPort(r.Host.Work)
	if port <= 0 {
		log.Warnf("dev server is not on the session network — %s", why)
		return
	}
	opt := devserver.HolderOptions{
		Relay: r.sessionNet.Relay, PIN: r.PIN, Key: r.sessionNet.Key, LocalPort: port,
	}
	if r.Control != nil {
		opt.Ticket = r.Control.HolderTicket(r.PIN, "dev")
	}
	// Direct-dial parity with git: a routable host is reached without the relay
	// hop, and a loopback address is still never published (see
	// DirectIsPublishable).
	if r.sessionNet.Direct {
		if dln, advHost, err := netbind.ListenAdvertise(r.bind, 0); err == nil {
			opt.DirectListener = dln
			opt.DirectAdvertise = net.JoinHostPort(advHost, strconv.Itoa(dln.Addr().(*net.TCPAddr).Port))
		} else {
			log.Debugf("no direct listener for the dev server: %v", err)
		}
	}
	h, err := devserver.StartHolder(context.WithoutCancel(ctx), opt)
	if err != nil {
		log.Warnf("dev server not published on the session network: %v", err)
		return
	}
	r.devHolder = h
	r.Runtime.SetSessionDev(h.URL(), h.Direct())
	r.Runtime.AnnounceDev(ctx)
}

// stopDevHolder unpublishes. A holder still registered for a service this
// member no longer runs is the same lie as a held lease for a service it does
// not serve, so this runs on the dev lease being lost as well as on shutdown.
func (r *Running) stopDevHolder() {
	r.devHolderM.Lock()
	h := r.devHolder
	r.devHolder = nil
	r.devHolderM.Unlock()
	if h == nil {
		return
	}
	_ = h.Close()
	if r.Runtime != nil {
		r.Runtime.SetSessionDev("", "")
	}
}

// stopGitHolder tears the session-network git tunnel down when the lease moves.
// The loopback listener and on-disk canonical stay — only the relay registration
// must go, or "first live holder wins" locks the next owner out (ticket 15).
func (r *Running) stopGitHolder() {
	if r.Host == nil || r.Host.Srv == nil {
		return
	}
	r.Host.Srv.UnpublishSession()
	if strings.HasPrefix(r.GitURL, "slop://") {
		r.GitURL = r.Host.RemoteURL()
	}
	r.GitDirect = ""
	log := r.log
	if log == nil {
		log = logx.New("host")
	}
	log.Infof("no longer publishing git on the session network — lease moved")
}

// startGitHolder republishes git onto the session network after a stand-down
// reclaim. No-op when this host never joined a relay.
func (r *Running) startGitHolder() error {
	if r.sessionNet == nil || r.Host == nil || r.Host.Srv == nil {
		return nil
	}
	if err := r.Host.Srv.PublishSession(); err != nil {
		return err
	}
	if u := r.Host.SessionRemoteURL(); u != "" {
		r.GitURL = u
	}
	r.GitDirect = r.Host.Srv.SessionDirect()
	return nil
}

// startExecHolder publishes remote command execution for the session's box.
func (r *Running) startExecHolder(ctx context.Context, log *logx.Logger) {
	if r.sessionNet == nil || r.Host == nil {
		return
	}
	r.execHolderM.Lock()
	defer r.execHolderM.Unlock()
	if r.execHolder != nil {
		return
	}
	h, err := boxexec.StartHolder(context.WithoutCancel(ctx), boxexec.HolderOptions{
		Relay:   r.sessionNet.Relay,
		PIN:     r.PIN,
		Key:     r.sessionNet.Key,
		WorkDir: r.Host.Work,
		Ticket: func() (string, error) {
			if r.Control == nil {
				return "", nil
			}
			return r.Control.HolderTicket(r.PIN, boxexec.Service)()
		},
		OnShutdown: func() {
			r.stopHosting("box removed")
		},
	})
	if err != nil {
		log.Warnf("box exec not published on the session network: %v", err)
		return
	}
	r.execHolder = h
	log.Infof("box exec published on the session network for %s", r.PIN)
}

func (r *Running) stopExecHolder() {
	r.execHolderM.Lock()
	h := r.execHolder
	r.execHolder = nil
	r.execHolderM.Unlock()
	if h != nil {
		_ = h.Close()
	}
}

// PrintJoin writes the human-facing join blurb.
func (r *Running) PrintJoin() string {
	stack := detect.Probe().DefaultStack()
	return fmt.Sprintf("detected %s %s + %s — starting session (change?)\n\nsession live. share with your team:\n\n    slopball join %s\n\ndemo url:  %s\ngit:       %s\ncontrol:   %s\n",
		stack.Runtime, stack.Version, stack.PkgMgr, r.PIN, r.DemoURL, r.GitURL, r.ControlURL)
}

// NewPIN is kept for callers that still need a local random name (tests that
// claim under the fixture seam). Fresh creates leave the PIN empty and use
// what the control plane mints (abuse-surface ticket 11).
func NewPIN() (string, error) { return controlplane.MintPIN() }

func canonicalExists(root string) bool {
	_, err := os.Stat(filepath.Join(root, canonical.BareDir))
	return err == nil
}

func ensureWork(ctx context.Context, host *canonical.Host) error {
	if _, err := os.Stat(filepath.Join(host.Work, ".git")); err == nil {
		return nil
	}
	_ = os.RemoveAll(host.Work)
	return sbGit.Run(ctx, "", "clone", "--branch", "main", host.Bare, host.Work)
}

func seedFromDir(ctx context.Context, host *canonical.Host, dir string) error {
	id := sbGit.SessionIdentity("host", host.PIN)
	// The same guard the remote path applies (plan 33) — a local seed must not
	// be the laxer of the two.
	if err := canonical.PreflightSeedDir(dir); err != nil {
		return err
	}
	if err := canonical.CopyTree(dir, host.Work); err != nil {
		return err
	}
	c := &sbGit.Cmd{Dir: host.Work, Env: id.EnvVars()}
	if err := c.Run(ctx, "add", "-A"); err != nil {
		return err
	}
	status, _ := c.Output(ctx, "status", "--porcelain")
	if status == "" {
		return nil
	}
	if err := c.Run(ctx, "commit", "-m", "seed from "+filepath.Base(dir)); err != nil {
		return err
	}
	return c.Run(ctx, "push", "origin", "main")
}

// reseedSource answers what an empty disk should do, and it has exactly three
// answers. It returns a URL to seed from, "" to create a genuinely new
// canonical, or an error that stops the host coming up at all.
//
// The discriminator is whether the SESSION has ever had history, not whether
// this machine has: a box booting into a session its laptop created seconds ago
// legitimately has nothing to seed from and must create (that is the whole
// managed-box path), while a container waking from sleep into a session with
// real work in it must never publish an empty main. Evidence of history is any
// member that has ever reported a mirror, or a published convergence — both are
// written by the ordinary session loop, so neither needs a new fact.
//
// A session that HAS history and offers no live replica is dead, and says so.
// Creating there is the lie this exists to delete.
func reseedSource(ctx context.Context, client *controlplane.Client, pin, self string, log *logx.Logger) (string, error) {
	sess, err := client.Session(ctx, pin)
	if err != nil {
		return "", fmt.Errorf("no canonical on disk for %s and the control plane could not say whether the session has any: %w", pin, err)
	}
	hadHistory := sess.Convergence != nil && sess.Convergence.MainSHA != ""
	var freshest *controlplane.Member
	for i := range sess.Members {
		m := &sess.Members[i]
		if m.MainMirrorHeight == 0 && m.MainMirrorSHA == "" {
			continue
		}
		hadHistory = true
		if m.ID == self || !m.Online || m.State == controlplane.MemberLeft {
			continue
		}
		if freshest == nil || m.MainMirrorHeight > freshest.MainMirrorHeight {
			freshest = m
		}
	}
	if !hadHistory {
		return "", nil
	}
	if freshest == nil {
		return "", fmt.Errorf("no canonical on disk for %s and no live member holds a replica of main — "+
			"this session's code is gone from every machine that is still here. Nothing was published: "+
			"a fresh empty canonical would look healthy and serve nobody's work", pin)
	}
	url, err := client.EndpointURL(ctx, pin, controlplane.EndpointGit)
	if err != nil {
		return "", fmt.Errorf("no canonical on disk for %s and the session's git endpoint could not be resolved to seed from "+
			"(%s holds the freshest replica at height %d): %w", pin, freshest.Name, freshest.MainMirrorHeight, err)
	}
	log.Infof("%s: seeding from %s (freshest replica, height %d)", pin, freshest.Name, freshest.MainMirrorHeight)
	return url, nil
}

// requireHistory is the ordering rule made mechanical: the git endpoint is
// published and the git lease claimed further down Start, so a canonical that
// reached this point with no commits would be advertised as the session's
// truth. Every path above (open, seed, create) leaves at least one commit on
// main, so this only ever fires on a path that silently half-worked.
func requireHistory(ctx context.Context, host *canonical.Host) error {
	c := &sbGit.Cmd{Dir: host.Bare}
	if _, err := c.Output(ctx, "rev-parse", "--verify", "main"); err != nil {
		return fmt.Errorf("canonical at %s has no history on main — refusing to serve it as the session's truth: %w", host.Bare, err)
	}
	return nil
}

func seedFromRemote(ctx context.Context, root, pin, url string) (*canonical.Host, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	bare := filepath.Join(abs, canonical.BareDir)
	work := filepath.Join(abs, canonical.WorkDir)
	if err := sbGit.Run(ctx, "", "clone", "--bare", url, bare); err != nil {
		return nil, fmt.Errorf("seed from url: %w", err)
	}
	_ = sbGit.Run(ctx, bare, "config", "http.receivepack", "true")
	if err := sbGit.Run(ctx, "", "clone", "--branch", "main", bare, work); err != nil {
		_ = os.RemoveAll(work)
		if err := sbGit.Run(ctx, "", "clone", bare, work); err != nil {
			return nil, err
		}
		c := &sbGit.Cmd{Dir: work}
		_ = c.Run(ctx, "branch", "-M", "main")
		_ = c.Run(ctx, "push", "origin", "main")
	}
	return &canonical.Host{
		Root: abs, PIN: pin, Bare: bare, Work: work,
		Srv: &gitserver.Server{Bare: bare},
	}, nil
}

func splitURL(raw string) (host string, port int) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", 0
	}
	h := u.Hostname()
	p := u.Port()
	if p == "" {
		return h, 0
	}
	n, _ := strconv.Atoi(p)
	return h, n
}

// sessionNetFor builds the git server's session-network config from what the
// control plane handed back at claim time. No relay means no session network —
// a same-machine session needs none, and inventing one would be a fallback
// nobody asked for.
func sessionNetFor(ctx context.Context, sess controlplane.Session, bind string, ticket func() (string, error)) (*gitserver.SessionNet, error) {
	if sess.RelayAddr == "" {
		return nil, nil
	}
	key, err := sessionnet.ParseKey(sess.SessionKey)
	if err != nil {
		return nil, err
	}
	if key.Zero() {
		return nil, fmt.Errorf("the control plane names relay %s but minted no session key", sess.RelayAddr)
	}
	return &gitserver.SessionNet{
		Relay: sess.RelayAddr, PIN: sess.PIN, Key: key, Context: context.WithoutCancel(ctx),
		Direct: DirectIsPublishable(bind),
		Ticket: ticket,
	}, nil
}

// waitForIncumbentToStandDown re-attempts the session-network registration
// while the relay answers busy, up to bound. See the call site for why this
// exists and why the bound is the lease TTL.
func waitForIncumbentToStandDown(ctx context.Context, log *logx.Logger, host *canonical.Host, bound time.Duration) (string, error) {
	log.Infof("the previous git holder is still registered — waiting for it to stand down (up to %s)", bound)
	deadline := time.Now().Add(bound)
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		url, err := host.StartServer()
		if err == nil {
			log.Infof("the previous git holder stood down — serving")
			return url, nil
		}
		if !errors.Is(err, sessionnet.ErrHolderBusy) || time.Now().After(deadline) {
			return "", err
		}
	}
}

// DirectIsPublishable decides whether this host has an address worth putting in
// the session record for other machines to try (plan 38 step 4).
//
// Exported because it is a decision rule with consequences on somebody else's
// laptop and no other door onto it: the only production caller is sessionNetFor,
// itself reachable only through a whole Start, so the choice between "publish an
// address" and "publish nothing" would otherwise be observable only by standing
// up a session — far too much machinery to hold a three-branch rule still.
//
// A LOOPBACK one is worse than none, and not merely useless: another machine
// reading 127.0.0.1:<port> dials its OWN localhost, where it may well find an
// unrelated service listening. That peer then fails the handshake — and the
// handshake failure is deliberately NOT retried via the relay, because a
// reachable peer that cannot authenticate is an auth problem rather than a
// routing one. So a loopback direct address would turn a working session into a
// hard error on somebody else's laptop. Publish nothing instead: the relay is
// the mechanism and always works.
// A machine whose provisioner told it there is nowhere to be dialled publishes
// nothing, whatever its own interfaces look like — see netbind.AdvertiseNone.
// Regression: TestAProvisionerCanSayThisMachineHasNoDirectAddress.
func DirectIsPublishable(bind string) bool {
	if netbind.DirectSuppressed() {
		return false
	}
	host, err := netbind.AdvertiseHostMode(bind)
	if err != nil || host == "" {
		return false
	}
	return !netbind.LoopbackURL("http://" + host)
}

// logsURLFromGit derives the /logs sibling of a git endpoint. It handles both a
// machine address and a slop:// session address, because the off-box
// error-watcher polls whichever one the host published.
func logsURLFromGit(gitURL string) string {
	if sessionnet.IsSessionURL(gitURL) {
		pin, service, _, err := sessionnet.ParseURL(gitURL)
		if err != nil {
			return ""
		}
		return sessionnet.FormatURL(pin, service, "logs")
	}
	u, err := url.Parse(gitURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/logs"
}

// defaultMemberName is how this host registers on the control plane. A box is
// not a teammate — it names itself `box` (plan 42); mesh laptops use $USER.
func defaultMemberName(opt Options) string {
	if opt.Name != "" {
		return opt.Name
	}
	if opt.ServeOnly {
		return "box"
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "host"
}
