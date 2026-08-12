package joindaemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/nwylynko/slopball-cli/canonical"
	"github.com/nwylynko/slopball-cli/conductor"
	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/detect"
	"github.com/nwylynko/slopball-cli/devserver"
	sbGit "github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/harness"
	"github.com/nwylynko/slopball-cli/migrate"
	"github.com/nwylynko/slopball-cli/placement"
	"github.com/nwylynko/slopball-cli/runfile"
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
	res, err := migrate.Run(ctx, migrate.Request{
		PIN: j.Session.PIN,
		Survivors: []migrate.Survivor{{
			Name:    strings.TrimPrefix(j.Session.Branch, "client/"),
			Mirror:  j.Paths.Mirror,
			Work:    j.Paths.Work,
			Branch:  j.Session.Branch,
			Profile: profile,
		}},
		Control: j.Control,
	})
	if err != nil {
		return fmt.Errorf("promote mirror to canonical: %w", err)
	}
	j.mu.Lock()
	j.canonical = res.Canonical
	j.mu.Unlock()
	j.log.Infof("serving canonical for %s from this machine — %s", j.Session.PIN, res.Canonical.RemoteURL())
	return nil
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
	if err := sup.Start(ctx); err != nil {
		return err
	}
	j.mu.Lock()
	j.dev = sup
	j.mu.Unlock()
	j.log.Infof("dev server running here: %s", strings.Join(dev, " "))
	return nil
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
	if err := fleet.TickAll(ctx); err != nil {
		j.log.Warnf("conductor tick: %v", err)
	}
}
