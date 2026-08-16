package joindaemon

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nwylynko/slopball-cli/canonical"
	"github.com/nwylynko/slopball-cli/conductor"
	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/detect"
	"github.com/nwylynko/slopball-cli/devserver"
	sbGit "github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/gitserver"
	"github.com/nwylynko/slopball-cli/harness"
	"github.com/nwylynko/slopball-cli/hoststart"
	"github.com/nwylynko/slopball-cli/migrate"
	"github.com/nwylynko/slopball-cli/netbind"
	"github.com/nwylynko/slopball-cli/placement"
	"github.com/nwylynko/slopball-cli/runfile"
	"github.com/nwylynko/slopball-cli/runtime"
	"github.com/nwylynko/slopball-cli/sessionnet"
)

// newPlacementLoop builds this client's half of automatic placement (plan 30).
// Every joined member runs one, which is what makes "the device that created
// the session is not special" true: when the creator goes away its leases
// expire, and whichever survivor ranks first simply takes them.
func (j *Joined) newPlacementLoop() *placement.Loop {
	return &placement.Loop{
		Control:  j.Control,
		PIN:      j.Session.PIN,
		MemberID: j.MemberID,
		Name:     strings.TrimPrefix(j.Session.Branch, "client/"),
		Machine:  hostnameOr("this machine"),
		Start:    j.startService,
		Stop:     j.stopService,
		Serves: func(service string) bool {
			// A process already conducting in the foreground holds the lease for
			// that fleet (Options.ConductsHere) and must never start a second one
			// here — including on the resume path, after a lost lease.
			return !(service == controlplane.ServiceConductor && j.conductsElsewhereInThisProcess)
		},
		OnChange: func(service, detail string) {
			j.log.Infof("placement: %s %s", service, detail)
		},
	}
}

// startService brings one service up on this client. The machinery is the
// existing machinery — canonical + gitserver, devserver.Supervisor, the
// conductor fleet — this only decides which of it runs here.
func (j *Joined) startService(ctx context.Context, service string) error {
	switch service {
	case controlplane.ServiceGit:
		return j.promoteToCanonical(ctx)
	case controlplane.ServiceDev:
		return j.startDev(ctx)
	case controlplane.ServiceConductor:
		return j.startConductor(ctx)
	}
	return nil
}

func (j *Joined) stopService(_ context.Context, service string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	switch service {
	case controlplane.ServiceDev:
		// The relay registration goes with the lease, before the process: a
		// holder still registered for a service this member no longer runs is
		// the same lie as a held lease for a service it does not serve, and it
		// locks the next owner out ("first live holder wins").
		if j.devHolder != nil {
			_ = j.devHolder.Close()
			j.devHolder = nil
		}
		j.devRuntime = nil
		if j.dev != nil {
			_ = j.dev.Stop()
			j.dev = nil
		}
	case controlplane.ServiceConductor:
		// Close the held /logs follower with the fleet: a lease handed back and
		// reclaimed would otherwise leave one connection per election alive.
		if j.fleetLogs != nil {
			j.fleetLogs.Close()
			j.fleetLogs = nil
		}
		j.fleet, j.publisher, j.refreshGate = nil, nil, nil
	case controlplane.ServiceGit:
		// Canonical bytes stay on disk — they are the session's local copy —
		// but the session-network registration must go with the lease. Leaving
		// it up would make the relay refuse the next owner's register
		// (abuse-surface ticket 15).
		if j.canonical != nil && j.canonical.Srv != nil {
			j.canonical.Srv.UnpublishSession()
		}
		j.log.Infof("no longer serving canonical — clients follow the control plane's git endpoint")
	}
	return nil
}

// promoteToCanonical turns this client's mirror into the session's canonical
// and serves it. This is plan 16's reconstruction, reached automatically by a
// lease expiring instead of by a human running a migration.
func (j *Joined) promoteToCanonical(ctx context.Context) error {
	j.mu.Lock()
	already := j.canonical
	j.mu.Unlock()
	if already != nil {
		return nil
	}
	profile := detect.Probe()
	// On the session network, or not at all: the address this member publishes
	// is read by machines on other networks, so it has to name the session's
	// git ROLE (slop://…) — the same registration the host it replaces held.
	// A session with no relay keeps the direct address, as before.
	sessNet, err := j.sessionNetFor(ctx, "git")
	if err != nil {
		return fmt.Errorf("promote mirror to canonical: %w", err)
	}
	res, err := migrate.Run(ctx, migrate.Request{
		PIN: j.Session.PIN,
		Survivors: []migrate.Survivor{{
			Name:    strings.TrimPrefix(j.Session.Branch, "client/"),
			Machine: hostnameOr("this machine"),
			Mirror:  j.Paths.Mirror,
			Work:    j.Paths.Work,
			Branch:  j.Session.Branch,
			Profile: profile,
		}},
		Control:    j.Control,
		SessionNet: sessNet,
	})
	if err != nil {
		return fmt.Errorf("promote mirror to canonical: %w", err)
	}
	j.mu.Lock()
	j.canonical = res.Canonical
	j.mu.Unlock()
	if su := res.Canonical.SessionRemoteURL(); su != "" {
		j.log.Infof("serving canonical for %s from this machine — %s (session network; local listener %s)", j.Session.PIN, su, res.Canonical.RemoteURL())
	} else {
		j.log.Infof("serving canonical for %s from this machine — %s", j.Session.PIN, res.Canonical.RemoteURL())
	}
	return nil
}

// sessionNetFor is the join daemon's half of what hoststart.sessionNetFor is
// for the host: the session-network registration a service this member takes
// over must hold. nil, nil when the control plane names no relay — a
// same-machine session, where the direct address is the address.
func (j *Joined) sessionNetFor(ctx context.Context, service string) (*gitserver.SessionNet, error) {
	sess, err := j.Control.Session(ctx, j.Session.PIN)
	if err != nil {
		return nil, err
	}
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
	bind := netbind.BindForControl(j.Control.Base)
	return &gitserver.SessionNet{
		Relay: sess.RelayAddr, PIN: sess.PIN, Key: key, Context: context.WithoutCancel(ctx),
		Direct: hoststart.DirectIsPublishable(bind),
		Ticket: j.Control.HolderTicket(sess.PIN, service),
	}, nil
}

// startDev supervises the project's own dev command (plan 29's run.json) — the
// reason dev failover is possible at all: the command belongs to the session,
// not to the machine that happened to type it.
func (j *Joined) startDev(ctx context.Context) error {
	work := j.Paths.Work
	j.mu.Lock()
	if j.canonical != nil {
		work = j.canonical.Work
	}
	j.mu.Unlock()

	declared := runfile.Read(work)
	install := runfile.Resolve(nil, declared.Install, func() []string { return devserver.DetectInstall(work) })
	dev := runfile.Resolve(nil, declared.Dev, func() []string { return devserver.DetectDev(work) })
	if len(dev) == 0 {
		return fmt.Errorf("this project declares no dev command (%s) and none could be detected", runfile.Path)
	}
	logs := &devserver.LogBuffer{}
	if len(install) > 0 {
		if err := devserver.Install(ctx, work, install,
			logs.Writer(devserver.StreamStderr, devserver.PhaseInstall)); err != nil {
			j.log.Warnf("install failed (starting dev anyway): %v", err)
		}
	}
	sup := &devserver.Supervisor{WorkDir: work, Command: dev, Logs: logs}
	// The process outlives this call: it rides the dev LEASE (stopService) or
	// the daemon (Close), never the caller's context. That context is the
	// mirror loop's 30s tick, and a supervisor started on it was killed at the
	// end of the cycle that started it — restarted on the next, killed again.
	if err := sup.Start(context.WithoutCancel(ctx)); err != nil {
		return err
	}
	// Publish it the way the host publishes its own: a holder on the session
	// network for the dev service, and the dev/demo endpoints announced once
	// something is listening on the constant port (ticket 21). Without both,
	// the control plane keeps whatever the previous owner published — for
	// session wioqg5 that was the departed box's slop://…/dev, so every dial
	// answered no-holder while this member held the lease and ran the process.
	sess, err := j.Control.Session(ctx, j.Session.PIN)
	if err != nil {
		_ = sup.Stop()
		return fmt.Errorf("start dev: read session: %w", err)
	}
	rt := &runtime.Reconciler{
		WorkDir: work, Dev: sup, Control: j.Control, PIN: j.Session.PIN, Generation: sess.Generation,
	}
	var holder *devserver.Holder
	if sessNet, err := j.sessionNetFor(ctx, "dev"); err != nil {
		_ = sup.Stop()
		return fmt.Errorf("start dev: %w", err)
	} else if sessNet != nil {
		port, why := runtime.LocalDevPort(work)
		if port <= 0 {
			_ = sup.Stop()
			return fmt.Errorf("start dev: no local dev port to publish — %s", why)
		}
		opt := devserver.HolderOptions{
			Relay: sessNet.Relay, PIN: sessNet.PIN, Key: sessNet.Key, LocalPort: port, Ticket: sessNet.Ticket,
		}
		if sessNet.Direct {
			if dln, advHost, err := netbind.ListenAdvertise(netbind.BindForControl(j.Control.Base), 0); err == nil {
				opt.DirectListener = dln
				opt.DirectAdvertise = net.JoinHostPort(advHost, strconv.Itoa(dln.Addr().(*net.TCPAddr).Port))
			}
		}
		h, err := devserver.StartHolder(context.WithoutCancel(ctx), opt)
		if err != nil {
			// A refused registration is the failure, not a degraded mode: the
			// lease goes back so a member that can publish takes it.
			_ = sup.Stop()
			return fmt.Errorf("start dev: %w", err)
		}
		holder = h
		rt.SetSessionDev(h.URL(), h.Direct())
	}
	j.mu.Lock()
	j.dev, j.devHolder, j.devRuntime = sup, holder, rt
	j.mu.Unlock()
	rt.AnnounceDev(ctx)
	j.log.Infof("dev server running here: %s", strings.Join(dev, " "))
	return nil
}

// keepDevPublished is the dev owner's per-cycle duty, the same two things the
// host loop does for its dev server (hoststart.KeepDevAlive): re-announce the
// endpoint while the process is alive — publication waits for something to be
// listening, and a cold start becomes listening after the first tick — and
// restart a process that exited. Announced at the session's CURRENT
// generation: this member's own git takeover bumps it, and an announcement at
// the old one is rejected.
func (j *Joined) keepDevPublished(ctx context.Context) {
	j.mu.Lock()
	sup, rt := j.dev, j.devRuntime
	j.mu.Unlock()
	if sup == nil || rt == nil {
		return
	}
	if sess, err := j.Control.Session(ctx, j.Session.PIN); err == nil && sess.Generation > 0 {
		rt.SetGeneration(sess.Generation)
	}
	if sup.Running() {
		rt.AnnounceDev(ctx)
		return
	}
	if !sup.NeedsRestart() {
		return
	}
	if err := sup.Restart(context.WithoutCancel(ctx)); err != nil {
		j.log.Warnf("dev server %q could not be restarted: %v", strings.Join(sup.Command, " "), err)
		return
	}
	j.log.Infof("dev server %q had exited — restarted it", strings.Join(sup.Command, " "))
	rt.AnnounceDev(ctx)
}

// startConductor runs the fleet against whatever canonical the session
// currently has — locally if we also hold git, over the network otherwise. The
// harness login never moves; the work comes to the login (§10).
func (j *Joined) startConductor(ctx context.Context) error {
	j.mu.Lock()
	host := j.canonical
	j.mu.Unlock()

	if host == nil {
		url, _, err := j.Control.GitURL(ctx, j.Session.PIN)
		if err != nil {
			return fmt.Errorf("no canonical to conduct: %w", err)
		}
		host, err = canonical.OpenRemote(ctx, filepath.Join(j.Paths.Root, "replica"), j.Session.PIN, url)
		if err != nil {
			return err
		}
	}
	// The session's own composition decides the agents, not this machine's
	// default: taking the conductor lease over must reproduce the fleet the
	// session elected. This machine's harness only fills roles nobody named.
	built := conductor.BuildSessionFleet(ctx, conductor.SessionFleetSpec{
		Host: host, ID: sbGit.SessionIdentity("conductor", j.Session.PIN),
		Control: j.Control, PIN: j.Session.PIN, Fallback: harness.FirstAvailable(),
	})
	if !built.HasIntelligence() {
		// Hand the lease straight back so a member that CAN conduct takes it,
		// rather than holding a conductor that resolves nothing.
		return fmt.Errorf("no harness CLI on this machine for any role the session named")
	}
	j.mu.Lock()
	j.fleet, j.fleetHost, j.publisher, j.fleetLogs = built.Fleet, host, built.Publisher, built.Logs
	j.mu.Unlock()
	j.log.Infof("conducting %s from this machine — %s", j.Session.PIN, built.Summary())
	return nil
}

// fleetLoop ticks the conductor fleet, on its own goroutine and its own
// context. Both halves of that matter and neither is incidental:
//
// The context carries NO deadline. The mirror loop's does — 30s, so a hung git
// fetch cannot wedge the heartbeat — and the fleet used to tick inside it,
// which killed every scaffold at exactly 30 seconds (`signal: killed`) when a
// create-next-app run takes 30–90. Roles bound their own work instead: setup
// gives one scaffold the fixed setupTimeout (10m), which is the bound
// that actually knows what it is bounding.
//
// The goroutine is what makes that safe. A 10-minute scaffold on the mirror
// loop would stall the 2s heartbeat and this member would look offline to the
// session — losing the very leases it is mid-way through serving.
func (j *Joined) fleetLoop() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-j.stop
		cancel()
	}()
	// Role state samples BESIDE the tick rather than inside it. A tick that
	// blocks for a 40-second AI resolve is exactly the one whose state has to
	// reach every other member's dashboard (plan 36 §2).
	go j.publishRoleState(ctx)
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-j.stop:
			return
		case <-t.C:
			// Serial by construction: one tick finishes before the next starts, so
			// two scaffolds can never run against the same tree.
			j.tickFleet(ctx)
		}
	}
}

// publishRoleState samples whatever fleet this member currently conducts. It
// reads the publisher every tick rather than capturing it, because the
// conductor lease can arrive (or leave) long after this loop starts.
func (j *Joined) publishRoleState(ctx context.Context) {
	t := time.NewTicker(conductor.StatePublishInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.mu.Lock()
			p := j.publisher
			j.mu.Unlock()
			p.Publish(ctx)
		}
	}
}

// tickFleet runs one conductor round when this member holds that lease.
func (j *Joined) tickFleet(ctx context.Context) {
	j.mu.Lock()
	fleet, host := j.fleet, j.fleetHost
	j.mu.Unlock()
	if fleet == nil {
		return
	}
	// The fleet ticks every 2s for local git work; only the network half is
	// gated on a pushed frame plus a floor (plan 43). A local host's Refresh is
	// a no-op, so the gate lets it through unchanged.
	if host != nil {
		j.mu.Lock()
		if j.refreshGate == nil {
			j.refreshGate = conductor.NewRefreshGate(j.Control, j.Session.PIN, host.RemoteURL() != "")
		}
		gate := j.refreshGate
		j.mu.Unlock()
		if gate.Due(time.Now()) {
			_ = host.Refresh(ctx)
		}
	}
	// TickRoles starts the roles and returns: this is a loop, so a role that is
	// still running from the last round is caught by the next one, and a
	// minutes-long setup scaffold never holds up the merger behind it.
	if err := fleet.TickRoles(ctx); err != nil {
		j.log.Warnf("conductor tick: %v", err)
	}
	if err := fleet.TickAfter(ctx); err != nil {
		j.log.Warnf("conductor tick: %v", err)
	}
}
