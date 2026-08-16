// Package joindaemon is the long-running `slopball join` side: resolve PIN via
// the control plane, clone canonical, keep a background-fresh main mirror
// (plans/11 + 24).
package joindaemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/slopball-cli/admission"
	"github.com/nwylynko/slopball-cli/canonical"
	"github.com/nwylynko/slopball-cli/conductor"
	"github.com/nwylynko/slopball-cli/contracts"
	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/detect"
	"github.com/nwylynko/slopball-cli/devserver"
	sbGit "github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/harness"
	"github.com/nwylynko/slopball-cli/logx"
	"github.com/nwylynko/slopball-cli/netbind"
	"github.com/nwylynko/slopball-cli/placement"
	"github.com/nwylynko/slopball-cli/runtime"
	"github.com/nwylynko/slopball-cli/session"
	"github.com/nwylynko/slopball-cli/syncengine"
)

// Options for joining a session.
type Options struct {
	PIN     string
	Name    string // client branch suffix
	Control *controlplane.Client

	// ConductsHere says this process already conducts the session in the
	// foreground — the `slopball dev` wizard, which scaffolds and then runs its
	// own fleet. The daemon then adopts the conductor lease instead of racing
	// for it, so the session still has exactly one conductor and this process
	// does not stand a second fleet up beside its own.
	ConductsHere bool
}

// Joined is a live client attachment.
type Joined struct {
	Session  session.Session
	Paths    session.Paths
	Remote   string
	MemberID string
	// Name is what this member is called in the session — the same string the
	// work feed puts on "nick synced client/nick".
	Name    string
	Control *controlplane.Client
	stop    chan struct{}
	log     *logx.Logger

	// Placement is this member's half of automatic service placement (plan 30):
	// it renews what this machine serves and claims what has fallen free. Every
	// member runs one, which is what makes the creator leaving a non-event.
	Placement *placement.Loop
	Outbox    *controlplane.Outbox

	// conductsElsewhereInThisProcess is Options.ConductsHere: the process runs
	// its own foreground fleet, so this daemon holds the conductor lease for it
	// but must never start one of its own.
	conductsElsewhereInThisProcess bool

	mu          sync.Mutex
	canonical   *canonical.Host            // set once this member wins the git lease
	dev         *devserver.Supervisor      // set once it wins dev
	devHolder   *devserver.Holder          // its session-network publication, with the lease
	devRuntime  *runtime.Reconciler        // announces the dev/demo endpoints once it listens
	fleet       *conductor.Fleet           // set once it wins conductor
	fleetHost   *canonical.Host            // what the fleet drives
	publisher   *conductor.StatePublisher  // role state → control plane (plan 36 §2)
	refreshGate *conductor.RefreshGate     // when the remote canonical is re-fetched
	fleetLogs   *conductor.RemoteLogSource // the fleet's held /logs follower
	devURL      string                     // local address for the session's dev server

	// upgradeRefusal is the sentence the control plane last refused this member
	// with (plan 48): this binary is below its client-version floor. It is
	// STATE and not an exit — a refused daemon keeps its mirror, its relay-side
	// git sync and its member cycle, so the field exists to be read (log once,
	// status) rather than to end anything.
	upgradeRefusal string
}

// UpgradeRequired reports whether the control plane is refusing this member for
// being too old. The daemon keeps running either way; this is what lets a
// status surface say why the fleet view has stopped updating.
func (j *Joined) UpgradeRequired() bool { return j.UpgradeRequiredReason() != "" }

// UpgradeRequiredReason is the one sentence to show, or "" when nothing is
// refusing this member.
func (j *Joined) UpgradeRequiredReason() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.upgradeRefusal
}

// noteUpgradeRequired records the refusal and logs it ONCE. Once because the
// member cycle keeps its normal cadence — the retries are pointless while the
// binary is what it is, and are kept anyway so a rollback that lowers the floor
// self-heals without a restart — and a per-cycle warning would bury the session
// log in the same sentence every five seconds.
func (j *Joined) noteUpgradeRequired(sentence string) {
	j.mu.Lock()
	first := j.upgradeRefusal == ""
	j.upgradeRefusal = sentence
	j.mu.Unlock()
	if first {
		j.log.Warnf("%s — this machine's git sync keeps working over the relay until its last session ticket expires (up to an hour), so finish what you are pushing", sentence)
	}
}

// clearUpgradeRequired forgets the refusal after a cycle succeeds, which is what
// a rollback lowering the floor looks like from here.
func (j *Joined) clearUpgradeRequired() {
	j.mu.Lock()
	had := j.upgradeRefusal != ""
	j.upgradeRefusal = ""
	j.mu.Unlock()
	if had {
		j.log.Infof("the control plane accepts this build again — the version refusal is over")
	}
}

// hostnameOr is the machine name published with a lease claim.
func hostnameOr(def string) string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return def
}

// Join resolves the PIN via the control plane, clones work + mirror (or resumes
// them, if this machine has joined before), registers as a member, and starts a
// background mirror + event loop.
func Join(ctx context.Context, opt Options) (*Joined, error) {
	if err := controlplane.ValidatePIN(opt.PIN); err != nil {
		return nil, err
	}
	// Before anything else, because the resume path below makes a second daemon
	// on one session directory *plausible* rather than merely broken: two of them
	// would drive the same mirror and heartbeat as two separate members.
	if err := noDaemonRunningHere(opt.PIN); err != nil {
		return nil, err
	}
	name := opt.Name
	if name == "" {
		if u := os.Getenv("USER"); u != "" {
			name = u
		} else {
			name = "agent"
		}
	}
	log := logx.New("join")
	client := opt.Control
	if client == nil {
		client = controlplane.NewClient(controlplane.BaseURL(""))
	}
	hostname, _ := os.Hostname()
	sess, memberID, err := resolveJoinSession(ctx, client, opt.PIN, name, hostname, log)
	if err != nil {
		return nil, err
	}
	gitURL := ""
	// raw endpoint ok: read for the log line below and for unreachableHint, which
	// is advice ABOUT what the host published. Everything that dials goes through
	// Dialable first (see the comment there).
	if ep, ok := sess.Endpoints[controlplane.EndpointGit]; ok {
		gitURL = ep.URL
	}
	published := gitURL
	if gitURL == "" {
		return nil, fmt.Errorf("session %s has no git endpoint yet (host %s has not announced one)", opt.PIN, orUnknown(sess.HostMachine))
	}
	log.Infof("control plane ok — %s resolved: git=%s generation=%d host=%s", opt.PIN, gitURL, sess.Generation, orUnknown(sess.HostMachine))
	// Everything below hands this URL to git, so it has to be a URL git can
	// dial. On the session network the endpoint is `slop://<pin>/git/…`, which
	// names the session ROLE — correct to publish, meaningless to `git clone`.
	// Dialable is the one place that knows the difference (it stands the
	// loopback forwarder up); skipping it is how join died with "remote helper
	// 'slop' aborted session" while every published address looked right.
	dialable, err := client.Dialable(ctx, sess, gitURL)
	if err != nil {
		return nil, fmt.Errorf("reach %s for session %s: %w", gitURL, opt.PIN, err)
	}
	gitURL = dialable
	// On the PUBLISHED endpoint, never the dialable one. A `slop://` endpoint
	// always resolves to a loopback forwarder here — that is what Dialable is
	// for — so reading the resolved address made this fire on every session-
	// network join, one line after logging the correct slop:// URL, telling the
	// reader to restart a host that was configured properly.
	if hint := unreachableHint(published, client.Base); hint != "" {
		log.Warnf("%s", hint)
	}
	paths := session.ForPin(opt.PIN)
	if err := os.MkdirAll(paths.Root, 0o755); err != nil {
		return nil, err
	}
	branch := "client/" + name
	id := sbGit.SessionIdentity(name, opt.PIN)
	joined, jerr := alreadyJoinedHere(opt.PIN, paths)
	if jerr != nil {
		return nil, jerr
	}
	if joined {
		// A rejoin is the ordinary way back — a daemon that died, a laptop that
		// slept, a machine that lost the network — and it is usually happening
		// BECAUSE something moved, so following the current endpoint is the whole
		// job. Cloning here would fail on the non-empty directory and the only way
		// out was deleting the session, which discards every unpushed commit on
		// this machine.
		log.Infof("resuming the existing work tree at %s (branch %s)", paths.Work, branch)
		if err := syncengine.Repoint(ctx, paths.Work, paths.Mirror, gitURL, id); err != nil {
			return nil, fmt.Errorf("follow %s to its current git endpoint: %w%s", opt.PIN, err, diagnose(published))
		}
	} else {
		log.Infof("cloning work tree on branch %s from %s", branch, gitURL)
		if err := syncengine.CloneClient(ctx, gitURL, paths.Work, branch, id); err != nil {
			return nil, fmt.Errorf("clone work: %w%s", err, diagnose(published))
		}
		log.Debugf("cloning bare mirror from %s", gitURL)
		if err := sbGit.Run(ctx, "", "clone", "--bare", gitURL, paths.Mirror); err != nil {
			return nil, fmt.Errorf("clone mirror: %w%s", err, diagnose(published))
		}
	}
	if err := contracts.Install(paths.Work, opt.PIN); err != nil {
		return nil, fmt.Errorf("install contracts: %w", err)
	}
	meta := session.Session{
		PIN:             opt.PIN,
		Role:            session.RoleClient,
		Branch:          branch,
		HostOverlayAddr: sess.OverlayAddr,
		Capability:      detect.Probe(),
	}
	if err := meta.Save(); err != nil {
		return nil, err
	}
	_ = syncengine.SaveCursors(paths.Cursors, syncengine.Cursors{
		Endpoint: gitURL, Generation: sess.Generation,
	})
	// Presence fields the redeem/claim did not know yet. Idempotent when the
	// secret already identifies this name (invite and accept both land here).
	if mid, err := client.JoinMember(ctx, opt.PIN, controlplane.MemberJoinRequest{
		Name: name, Role: controlplane.RoleClient, Branch: branch, Machine: hostname,
		Capability: controlplane.CapFromProfile(detect.Probe()),
	}); err != nil {
		log.Warnf("member join: %v", err)
	} else if mid != "" {
		memberID = mid
	}
	if memberID != "" {
		meta.MemberID = memberID
		_ = meta.Save()
	}
	j := &Joined{
		Session: meta, Paths: paths, Remote: gitURL, MemberID: memberID, Name: name,
		Control: client, stop: make(chan struct{}), log: log,
	}
	j.conductsElsewhereInThisProcess = opt.ConductsHere
	j.Placement = j.newPlacementLoop()
	if opt.ConductsHere {
		// Adopt, do not race: this process is already the session's conductor, so
		// the lease records that before the placement loop's first round. Without
		// it the daemon claimed the lease two seconds later and stood a SECOND
		// fleet up beside the foreground one — two setup roles scaffolding the
		// same brief, with different agents.
		if err := j.Placement.Adopt(ctx, controlplane.ServiceConductor); err != nil {
			log.Warnf("could not record that this process conducts %s (%v) — another member may take the conductor", opt.PIN, err)
		}
	}
	// Plan 43: hold the session stream; cancelled in Close.
	j.Control.Watch(context.Background(), j.Session.PIN)
	if j.MemberID != "" {
		j.Outbox = controlplane.NewOutbox(j.Control, j.Session.PIN, j.MemberID)
		// The fleet this daemon may take over — or the wizard's foreground one
		// sharing this memoized client — publishes through the same cycle.
		j.Control.RegisterOutbox(j.Session.PIN, j.Outbox)
		if j.Placement != nil {
			j.Placement.Outbox = j.Outbox
		}
	}
	go j.mirrorLoop()
	go j.fleetLoop()
	if err := session.WriteLive(meta); err != nil {
		log.Warnf("could not write live marker: %v", err)
	}
	log.Infof("mirror daemon running for %s (branch %s)", opt.PIN, branch)
	return j, nil
}

func (j *Joined) mirrorLoop() {
	t := time.NewTicker(controlplane.MemberCycle)
	defer t.Stop()
	var lastMain string
	id := sbGit.SessionIdentity(strings.TrimPrefix(j.Session.Branch, "client/"), j.Session.PIN)
	admissionLog := &admission.Log{Control: j.Control, PIN: j.Session.PIN}
	updates := j.Control.Updates(j.Session.PIN)
	mirrorFloor := time.NewTimer(controlplane.Floor(controlplane.MirrorFloor, j.Control.StreamLive(j.Session.PIN)))
	defer mirrorFloor.Stop()
	fetchNow := true
	for {
		select {
		case <-j.stop:
			return
		case u, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			for _, e := range u.Events {
				if e.Kind == "main.advanced" || e.Kind == "host.cutover" {
					fetchNow = true
				}
				if e.Kind == "host.cutover" {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					j.log.Infof("host.cutover — following new canonical")
					_, _ = syncengine.FollowHost(ctx, syncengine.FollowOpts{
						Work: j.Paths.Work, Mirror: j.Paths.Mirror, PIN: j.Session.PIN,
						Cursors: j.Paths.Cursors, ID: id,
						Resolve: func(ctx context.Context, pin string) (string, int, error) {
							return j.Control.GitURL(ctx, pin)
						},
					})
					cancel()
				}
			}
			admissionLog.Stream(u.Events)
		case <-mirrorFloor.C:
			fetchNow = true
			mirrorFloor.Reset(controlplane.Floor(controlplane.MirrorFloor, j.Control.StreamLive(j.Session.PIN)))
		case <-t.C:
			if !j.Control.StreamLive(j.Session.PIN) {
				admissionLog.Poll(context.Background())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, _ = syncengine.FollowHost(ctx, syncengine.FollowOpts{
				Work: j.Paths.Work, Mirror: j.Paths.Mirror, PIN: j.Session.PIN,
				Cursors: j.Paths.Cursors, ID: id,
				Resolve: func(ctx context.Context, pin string) (string, int, error) {
					return j.Control.GitURL(ctx, pin)
				},
			})
			if fetchNow {
				if err := sbGit.Run(ctx, j.Paths.Mirror, "fetch", "origin", "+refs/heads/main:refs/heads/main"); err != nil {
					j.log.Warnf("mirror fetch: %v", err)
				}
				fetchNow = false
				if !mirrorFloor.Stop() {
					select {
					case <-mirrorFloor.C:
					default:
					}
				}
				mirrorFloor.Reset(controlplane.Floor(controlplane.MirrorFloor, j.Control.StreamLive(j.Session.PIN)))
			}
			main, _ := sbGit.Output(ctx, j.Paths.Mirror, "rev-parse", "refs/heads/main")
			main = strings.TrimSpace(main)
			if j.Outbox != nil {
				j.Outbox.SetMember(controlplane.MemberUpdate{
					MainMirrorSHA: main, MainMirrorHeight: mirrorHeight(ctx, j.Paths.Mirror),
					Branch: j.Session.Branch, Harness: localHarness(),
					Capability: controlplane.CapFromProfile(detect.Probe()),
				})
				if j.Placement != nil {
					for _, svc := range controlplane.Services {
						if j.Placement.HoldsService(svc) {
							j.Outbox.Hold(svc)
						}
					}
				}
				res, err := j.Outbox.Flush(ctx)
				if errors.Is(err, controlplane.ErrNoSession) || errors.Is(err, controlplane.ErrUnauthorized) {
					j.log.Warnf("session %s ended — stopping join daemon (%v)", j.Session.PIN, err)
					cancel()
					j.Close()
					return
				}
				// Deliberately NOT an exit path, and deliberately not a debug
				// line either: a refused member keeps its mirror, its relay-side
				// git sync and this very cadence (a rollback that lowers the
				// floor self-heals without a restart), but it is the one member
				// whose fleet view has silently stopped updating, so it is told
				// once why (plan 48).
				if errors.Is(err, controlplane.ErrUpgradeRequired) {
					j.noteUpgradeRequired(err.Error())
				} else if err != nil {
					j.log.Debugf("member cycle: %v", err)
				} else {
					j.clearUpgradeRequired()
					if j.Placement != nil {
						j.Placement.ApplyLost(ctx, res.Lost)
					}
				}
			} else if j.MemberID != "" {
				err := j.Control.Heartbeat(ctx, j.Session.PIN, j.MemberID, controlplane.MemberUpdate{
					MainMirrorSHA: main, MainMirrorHeight: mirrorHeight(ctx, j.Paths.Mirror),
					Branch: j.Session.Branch, Harness: localHarness(),
					Capability: controlplane.CapFromProfile(detect.Probe()),
				})
				if errors.Is(err, controlplane.ErrNoSession) || errors.Is(err, controlplane.ErrUnauthorized) {
					j.log.Warnf("session %s ended — stopping join daemon (%v)", j.Session.PIN, err)
					cancel()
					j.Close()
					return
				}
				if errors.Is(err, controlplane.ErrUpgradeRequired) {
					j.noteUpgradeRequired(err.Error())
				} else if err == nil {
					j.clearUpgradeRequired()
				}
			}
			if j.Placement != nil {
				if err := j.Placement.Tick(ctx); err != nil {
					if errors.Is(err, controlplane.ErrNoSession) || errors.Is(err, controlplane.ErrUnauthorized) {
						j.log.Warnf("session %s ended — stopping join daemon (%v)", j.Session.PIN, err)
						cancel()
						j.Close()
						return
					}
					j.log.Debugf("placement: %v", err)
				}
			}
			j.keepDevPublished(ctx)
			j.ensureDevForwarder(ctx)
			cancel()
			if main != "" && main != lastMain {
				if lastMain != "" {
					j.log.Infof("mirror: main advanced to %s (run `slopball sync` to integrate)", shortSHA(main))
				} else {
					j.log.Debugf("mirror: main at %s", shortSHA(main))
				}
				lastMain = main
			}
		}
	}
}

// Fleet is the conductor fleet this member currently runs, or nil when it does
// not conduct — either because it never took the conductor lease, or because it
// holds the lease for a fleet running in the foreground of this process
// (Options.ConductsHere).
//
// Exported because "what does this member conduct, and with which agent?" is a
// question about the session rather than about this package's internals, and it
// is the only place the answer exists: the role state published to the control
// plane carries state and activity but not composition, so nothing outside can
// tell a daemon that reproduced the session's elected fleet from one that
// imposed this machine's default, or a daemon adopting a foreground lease from
// one that stood a second fleet up beside it.
func (j *Joined) Fleet() *conductor.Fleet {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.fleet
}

// DevURL is the local address this member opens the session's dev server on, or
// "" until one exists.
func (j *Joined) DevURL() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.devURL
}

// ensureDevForwarder stands the dev forwarder up as soon as the endpoint
// exists, rather than when somebody goes looking for a URL (plan 41).
//
// Eager costs one relay connection per member for the session even if nobody
// opens the page. That is accepted: lazy means the console can only show a URL
// *after* you have gone looking for it, which is a politer version of the
// failure this whole plan exists to fix. The local port is derived from the PIN,
// so it survives this daemon restarting.
func (j *Joined) ensureDevForwarder(ctx context.Context) {
	j.mu.Lock()
	have := j.devURL
	j.mu.Unlock()
	if have != "" {
		return
	}
	url, err := j.Control.EndpointURL(ctx, j.Session.PIN, controlplane.EndpointDev)
	if err != nil || url == "" {
		return
	}
	j.mu.Lock()
	j.devURL = url
	j.mu.Unlock()
	// Published so `slopball site` can report THIS forwarder. A separate CLI
	// process resolving the endpoint itself would find this port taken, retry
	// to the next derived one, and print a URL that closes when it exits.
	if err := session.PublishDevURL(j.Session.PIN, url); err != nil {
		j.log.Warnf("could not publish the dev URL for `slopball site`: %v", err)
	}
	j.log.Infof("dev server: %s", url)
}

// unreachableHint names the one failure a joiner cannot diagnose from git's own
// error: the host published a loopback git URL, so "127.0.0.1" in the message
// means the HOST's machine, not this one, and the port is simply closed here.
func unreachableHint(gitURL, controlBase string) string {
	if !netbind.LoopbackURL(gitURL) || netbind.LoopbackURL(controlBase) {
		return ""
	}
	return fmt.Sprintf("the host published a loopback git endpoint (%s): that address means the host's OWN machine, so it is unreachable from here. The host must use a non-loopback control plane (so it publishes a routable direct address) or a session relay.", gitURL)
}

// diagnose annotates a clone failure, and reads the PUBLISHED endpoint for the
// same reason unreachableHint does: on the session network the URL git was given
// is slopball's own forwarder, and "the host published loopback" would be a
// confident wrong answer. A refused connection to a published loopback
// endpoint is ambiguous from here — either the host is on this machine and its
// process is gone, or (the reported bug) the host is on another machine and
// published an address that only means "itself". Name both, since git's
// "Failed to connect to 127.0.0.1" reads like a local problem either way.
func diagnose(gitURL string) string {
	if !netbind.LoopbackURL(gitURL) {
		return ""
	}
	return fmt.Sprintf("\nhint: %s is a loopback address — reachable only from the machine that published it. "+
		"Either the host process on THIS machine is not running, or the host is on another machine and must use a non-loopback control plane (or a session relay) so it advertises a routable address.", gitURL)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// resolveJoinSession is how a joiner first sees the session document (plan 44
// ticket 06). With a secret already on disk this is a GET (invite / rejoin).
// Without one it is a single redeem — no GET before it — that holds until a
// human decides and returns the session in the final frame.
func resolveJoinSession(ctx context.Context, client *controlplane.Client, pin, name, machine string, log *logx.Logger) (controlplane.Session, string, error) {
	if session.ReadMemberSecret(pin) != "" {
		sess, err := client.Session(ctx, pin)
		if err != nil {
			// One sentence, unwrapped. Wrapping it would tell somebody whose
			// slopball is simply old to go and check a PIN and a URL that are
			// both fine (plan 48).
			if errors.Is(err, controlplane.ErrUpgradeRequired) {
				return controlplane.Session{}, "", controlplane.ErrUpgradeRequired
			}
			if errors.Is(err, controlplane.ErrUnauthorized) || errors.Is(err, controlplane.ErrNoSession) {
				if localSessionState(pin) {
					return controlplane.Session{}, "", fmt.Errorf("this session has ended")
				}
				return controlplane.Session{}, "", err
			}
			return controlplane.Session{}, "", fmt.Errorf("resolve pin %s via control plane %s: %w", pin, client.Base, err)
		}
		return sess, "", nil
	}
	reqID, err := knockID(pin)
	if err != nil {
		return controlplane.Session{}, "", err
	}
	log.Infof("knocking on %s as %s@%s", pin, name, machine)
	res, err := client.Redeem(ctx, pin, controlplane.RedeemRequest{
		Name: name, Machine: machine, RequestID: reqID,
	}, func(p controlplane.RedeemPending) {
		log.Infof("%s", admission.WaitLine(pin, p.Acceptors))
	})
	// The accept landed while we were away and the secret went with the control
	// plane that minted it. The request id is spent: re-presenting it answers
	// 409 forever, so retrying is not optional — without this the machine is
	// locked out of the session permanently, holding a roster slot, with
	// nothing telling anyone why.
	if errors.Is(err, controlplane.ErrKnockSpent) {
		log.Warnf("the earlier join request for %s was accepted while this machine was disconnected, "+
			"and its secret was not delivered — knocking again, which needs one more accept", pin)
		if err := session.ClearRequestID(pin); err != nil {
			return controlplane.Session{}, "", err
		}
		if reqID, err = knockID(pin); err != nil {
			return controlplane.Session{}, "", err
		}
		res, err = client.Redeem(ctx, pin, controlplane.RedeemRequest{
			Name: name, Machine: machine, RequestID: reqID,
		}, func(p controlplane.RedeemPending) {
			log.Infof("%s", admission.WaitLine(pin, p.Acceptors))
		})
	}
	if err != nil {
		if errors.Is(err, controlplane.ErrUpgradeRequired) {
			return controlplane.Session{}, "", controlplane.ErrUpgradeRequired
		}
		if errors.Is(err, controlplane.ErrDeclined) {
			return controlplane.Session{}, "", fmt.Errorf("%w — ask someone in the session, or try again", err)
		}
		if errors.Is(err, controlplane.ErrDoorClosed) || errors.Is(err, controlplane.ErrDoorFull) {
			return controlplane.Session{}, "", err
		}
		if errors.Is(err, controlplane.ErrNoSession) {
			// Same server shape for a typo and an ended session — legibility is
			// on the client that already has local state (plan 44 ticket 09).
			if localSessionState(pin) {
				return controlplane.Session{}, "", fmt.Errorf("this session has ended")
			}
			return controlplane.Session{}, "", err
		}
		return controlplane.Session{}, "", fmt.Errorf("resolve pin %s via control plane %s: %w", pin, client.Base, err)
	}
	return res.Session, res.MemberID, nil
}

// knockID returns this machine's persisted knock id, minting one when absent.
// It is written before the redeem answers so a dropped connection re-attaches
// to the same queue entry instead of queueing a second person.
func knockID(pin string) (string, error) {
	if id := session.ReadRequestID(pin); id != "" {
		return id, nil
	}
	id, err := session.NewRequestID()
	if err != nil {
		return "", err
	}
	if err := session.WriteRequestID(pin, id); err != nil {
		return "", err
	}
	return id, nil
}

// localSessionState is true when this machine already belonged to the session —
// secret, session.json, or a work/mirror repo. A request-id alone does not count:
// knocking writes that before redeem answers (plan 44 ticket 09).
func localSessionState(pin string) bool {
	if session.ReadMemberSecret(pin) != "" {
		return true
	}
	p := session.ForPin(pin)
	if _, err := os.Stat(p.Meta); err == nil {
		return true
	}
	return isRepo(p.Work) || isRepo(p.Mirror)
}

// alreadyJoinedHere reports whether this machine can resume rather than clone.
// It insists on BOTH repos: the work tree is what holds unpushed commits, and
// the mirror is what keeps a base to integrate against, so half a session is a
// state to name rather than to guess at — an automatic re-clone of the missing
// half would be the "never build a fallback nobody asked for" mistake, and on
// the work-tree side it would delete somebody's work.
func alreadyJoinedHere(pin string, paths session.Paths) (bool, error) {
	work, mirror := isRepo(paths.Work), isRepo(paths.Mirror)
	switch {
	case work && mirror:
		return true, nil
	case !work && !mirror:
		if entries, err := os.ReadDir(paths.Work); err == nil && len(entries) > 0 {
			return false, fmt.Errorf("%s already exists and is not a slopball work tree — move it aside, then join again", paths.Work)
		}
		return false, nil
	default:
		have, missing := paths.Work, paths.Mirror
		if !work {
			have, missing = paths.Mirror, paths.Work
		}
		return false, fmt.Errorf("session %s is half-present on this machine: %s is a git repo but %s is not. "+
			"Nothing here is safe to guess at — copy anything you need out of %s, remove %s, then join again",
			pin, have, missing, paths.Work, paths.Root)
	}
}

func isRepo(dir string) bool {
	if dir == "" {
		return false
	}
	// A work tree has .git as a directory; a bare mirror has HEAD + objects at
	// its root. Checking for either keeps one helper honest about both.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		if _, err := os.Stat(filepath.Join(dir, "objects")); err == nil {
			return true
		}
	}
	return false
}

// noDaemonRunningHere refuses a join when ANOTHER process on this machine is
// already holding the session open. The marker is pid-checked, so a daemon that
// was killed does not lock anybody out — that stale-marker case is exactly the
// rejoin this exists to allow.
//
// The current process is exempt, and has to be: the wizard hosts and then joins
// itself in one process (`joinSelf`, after `hoststart.Start` has already written
// the marker), so treating our own pid as a competitor would refuse every
// first run.
func noDaemonRunningHere(pin string) error {
	m, ok := session.LiveHere(pin)
	if !ok || m.PID == os.Getpid() {
		return nil
	}
	return fmt.Errorf("a slopball daemon is already running here for %s (pid %d, started %s) — "+
		"use `slopball open %s` for a shell in the work tree, or stop that process first",
		pin, m.PID, m.StartedAt.Local().Format("15:04:05"), pin)
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// Close stops the background mirror loop.
func (j *Joined) Close() {
	if j.Control != nil {
		j.Control.CloseWatch(j.Session.PIN)
		j.Control.UnregisterOutbox(j.Session.PIN)
	}
	// Hand back anything this member was serving before leaving, so the next
	// owner takes over at once instead of waiting out a full lease TTL.
	if j.Placement != nil {
		j.Placement.ReleaseAll(context.Background())
	}
	if j.MemberID != "" && j.Control != nil {
		_ = j.Control.LeaveMember(context.Background(), j.Session.PIN, j.MemberID)
	}
	select {
	case <-j.stop:
	default:
		close(j.stop)
	}
	// Then stop what the leases had this machine serving — the dev process and
	// its holder, the git registration, the canonical's listener — exactly as
	// hoststart.Close does for a host. ReleaseAll hands the LEASES back; it
	// does not touch the processes behind them, and a dev server that outlives
	// its daemon holds the constant port against the next owner.
	for _, svc := range controlplane.Services {
		_ = j.stopService(context.Background(), svc)
	}
	j.mu.Lock()
	host := j.canonical
	j.canonical = nil
	j.mu.Unlock()
	if host != nil {
		_ = host.Close(context.Background())
	}
	_ = session.ClearLive(j.Session.PIN)
}

// mirrorHeight is how many commits this replica has on main — the comparable
// half of "who holds the freshest replica?" (plan 30). Zero when it cannot be
// read, which ranks the member last rather than failing the heartbeat.
func mirrorHeight(ctx context.Context, mirror string) int {
	out, err := sbGit.Output(ctx, mirror, "rev-list", "--count", "refs/heads/main")
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0
	}
	return n
}

// localHarness reports which agent CLI this machine could conduct with — the
// input that decides conductor eligibility. Empty means "not a candidate",
// which is the honest answer rather than a session-wide downgrade.
func localHarness() string {
	if c := harness.FirstAvailable(); c != nil {
		return string(c.Name)
	}
	return ""
}
