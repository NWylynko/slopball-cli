// Package migrate handles host vanish → ranked pick → canonical reconstruction
// (plan 16). No auto-promotion: Rank proposes, humans (or a test callback) pick.
package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nwylynko/slopball-cli/canonical"
	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/detect"
	sbGit "github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/gitserver"
	"github.com/nwylynko/slopball-cli/netbind"
)

// HostProvider conjures a machine to rebuild canonical on when a session has
// no survivors at all. It is the seam migrate owns and a provisioner
// implements — internal/cloudbox does — which is the direction that matters:
// the provisioner is control-plane code, and a client binary that named its
// types would ship a docker provisioner to every teammate (plan 49, ticket 01).
//
// migrate names a STACK and the provider resolves its own image, so image
// catalogs stay entirely on the provisioner's side. Today migrate has exactly
// one stack to name and it is the literal "static": this arm runs when there
// is no survivor left to probe, so there is no detected profile to read a
// runtime off, and a plain static host is the thing that can serve canonical
// while people rejoin. Widening that is a real design step (something has to
// remember what the session's stack was), not a signature change.
type HostProvider interface {
	ProvisionForStack(ctx context.Context, stack string) (*ProvisionedHost, error)
}

// ProvisionedHost is everything migrate needs to know about a machine a
// HostProvider just conjured: what to call the new host, and where canonical
// should land on it. Deliberately not a mirror of any provisioner's own box
// type — migrate reads these two fields and nothing else.
type ProvisionedHost struct {
	ID      string // the new host is recorded as "cloud:<id>"
	WorkDir string // canonical is rebuilt in <WorkDir>/canonical
}

// Survivor is a remaining client that can become host.
type Survivor struct {
	Name    string
	Machine string // hostname, recorded as the session's host machine when this survivor is picked
	Mirror  string // path to bare main mirror (fresh replica)
	Work    string // client work tree (has outstanding branch)
	Branch  string
	Profile detect.Profile
}

// Request is a stop-the-world migration.
type Request struct {
	PIN          string
	Survivors    []Survivor
	Control      *controlplane.Client
	HostProvider HostProvider                              // optional escape hatch: used only when Survivors is empty
	Pick         func(ranked []Survivor) (Survivor, error) // nil → pick ranked[0] (tests only)
	DetectGone   func() bool                               // confirmation; nil → trust caller
	// SessionNet, when set, puts the reconstructed canonical on the session
	// network: the new host registers as the session's git holder and the
	// control plane is told `slop://<pin>/git/canonical.git` — the address
	// that names the ROLE — instead of this machine's listener.
	//
	// Without it Run publishes a machine address, which is what it did before
	// the session network existed (plan 16), and what session wioqg5's
	// survivor did on 2026-08-16: it took the git lease from a departed box and
	// published http://127.0.0.1:63231/canonical.git into a shared control
	// plane, so every later `slopball join` dialled its own localhost, and the
	// conductor's forwarder — which names the role — found no holder. The
	// caller that has a relay hands one in; a session with no relay still gets
	// the direct address, as before.
	SessionNet *gitserver.SessionNet
}

// Result of a completed migration.
type Result struct {
	NewHost   Survivor
	Canonical *canonical.Host
	// Provisioned reports that no survivor could host, so the migration went
	// through Request.HostProvider and ProvisionedHost names what it got.
	Provisioned     bool
	ProvisionedHost *ProvisionedHost
}

// Detection holds flaky-network hysteresis state.
type Detection struct {
	FailThreshold int           // consecutive failures before "gone"
	Window        time.Duration // failures older than this are dropped
	fails         []time.Time
}

// ObserveReachability records a ping result. Returns true when host should be
// considered gone (no false positive on a single blip).
func (d *Detection) ObserveReachability(ok bool) bool {
	if d.FailThreshold <= 0 {
		d.FailThreshold = 3
	}
	if d.Window <= 0 {
		d.Window = 15 * time.Second
	}
	now := time.Now()
	if ok {
		d.fails = nil
		return false
	}
	d.fails = append(d.fails, now)
	// Drop old
	cut := now.Add(-d.Window)
	i := 0
	for i < len(d.fails) && d.fails[i].Before(cut) {
		i++
	}
	d.fails = d.fails[i:]
	return len(d.fails) >= d.FailThreshold
}

// Run freezes the session story: rank survivors, pick, reconstruct canonical,
// re-point the control plane at the new host.
func Run(ctx context.Context, req Request) (*Result, error) {
	if len(req.Survivors) == 0 {
		// Nobody left to host: the escape hatch is a machine that does not
		// exist yet.
		if req.HostProvider == nil {
			return nil, fmt.Errorf("no survivors and no host provider")
		}
		box, err := req.HostProvider.ProvisionForStack(ctx, "static")
		if err != nil {
			return nil, err
		}
		host, err := canonical.Create(ctx, filepath.Join(box.WorkDir, "canonical"), req.PIN)
		if err != nil {
			return nil, err
		}
		host.Bind = bindFor(req.Control)
		host.Session = req.SessionNet
		url, _ := host.StartServer()
		if su := host.SessionRemoteURL(); su != "" {
			url = su
		}
		if url == "" {
			url = host.Bare
		}
		if req.Control != nil {
			if err := flipControl(ctx, req.Control, req.PIN, url, "cloud:"+box.ID); err != nil {
				return nil, err
			}
		}
		return &Result{
			// "cloud:" is a recorded value, not an identifier — it is the name
			// this session will carry for its new host, so it stays as it is
			// even though the seam above stopped being cloud-specific.
			NewHost:         Survivor{Name: "cloud:" + box.ID},
			Canonical:       host,
			Provisioned:     true,
			ProvisionedHost: box,
		}, nil
	}

	ranked := rankSurvivors(req.Survivors)
	pickFn := req.Pick
	if pickFn == nil {
		pickFn = func(r []Survivor) (Survivor, error) { return r[0], nil }
	}
	chosen, err := pickFn(ranked)
	if err != nil {
		return nil, err
	}

	// Reconstruct canonical into a new relocatable dir next to the survivor.
	dest := filepath.Join(filepath.Dir(chosen.Mirror), "canonical-new")
	_ = os.RemoveAll(dest)
	host, err := reconstruct(ctx, dest, req.PIN, ranked, chosen)
	if err != nil {
		return nil, err
	}
	host.Bind = bindFor(req.Control)
	host.Session = req.SessionNet
	url, err := host.StartServer()
	if req.SessionNet != nil && err != nil {
		// On the session network a refused registration is the whole failure:
		// publishing the loopback listener instead would make the session read
		// as live while nobody off this machine can reach it (the same rule
		// hoststart applies, for the same reason).
		return nil, fmt.Errorf("serve the reconstructed canonical on the session network: %w", err)
	}
	if su := host.SessionRemoteURL(); su != "" {
		url = su
	}
	if url == "" {
		url = "file://" + host.Bare
	}
	if req.Control != nil {
		if err := flipControl(ctx, req.Control, req.PIN, url, chosen.Machine); err != nil {
			return nil, err
		}
	}
	return &Result{NewHost: chosen, Canonical: host}, nil
}

// bindFor keeps a migrated canonical as reachable as the one it replaces: the
// new host publishes its git URL into the same control plane every survivor
// reads, so a loopback bind would strand them exactly as it strands joiners.
func bindFor(cp *controlplane.Client) string {
	if cp == nil {
		return ""
	}
	return netbind.BindForControl(cp.Base)
}

// flipControl announces the new canonical on the control plane.
// TODO(plan 24): delegate to cutover.Flip once cutover accepts *controlplane.Client.
//
// hostMachine names the machine canonical now lives on. The record used to
// keep the departed host's name across a migration, so a joiner was told
// `host=cloudchamber` by a session a laptop had been serving for minutes.
func flipControl(ctx context.Context, cp *controlplane.Client, pin, newGitURL, hostMachine string) error {
	sess, err := cp.Session(ctx, pin)
	if err != nil {
		return fmt.Errorf("migrate cutover session: %w", err)
	}
	if _, err := cp.Cutover(ctx, pin, controlplane.CutoverRequest{
		NewGitURL:   newGitURL,
		Generation:  sess.Generation,
		HostMachine: hostMachine,
	}); err != nil {
		return fmt.Errorf("migrate cutover: %w", err)
	}
	return nil
}

func rankSurvivors(ss []Survivor) []Survivor {
	profiles := make([]detect.Profile, len(ss))
	for i, s := range ss {
		p := s.Profile
		p.Hostname = s.Name
		profiles[i] = p
	}
	rankedP := detect.Rank(profiles)
	byName := map[string]Survivor{}
	for _, s := range ss {
		byName[s.Name] = s
	}
	out := make([]Survivor, 0, len(ss))
	for _, p := range rankedP {
		if s, ok := byName[p.Hostname]; ok {
			s.Profile = p
			out = append(out, s)
		}
	}
	return out
}

// reconstruct builds a new canonical from the freshest main among mirrors and
// re-publishes outstanding client branches.
func reconstruct(ctx context.Context, dest, pin string, all []Survivor, chosen Survivor) (*canonical.Host, error) {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, err
	}
	// Pick freshest main by commit timestamp across mirrors.
	best := chosen.Mirror
	var bestTime int64
	for _, s := range all {
		t := commitTime(ctx, s.Mirror, "main")
		if t >= bestTime {
			bestTime = t
			best = s.Mirror
		}
	}
	bare := filepath.Join(dest, canonical.BareDir)
	if err := sbGit.Run(ctx, "", "clone", "--bare", best, bare); err != nil {
		return nil, fmt.Errorf("clone freshest main: %w", err)
	}
	_ = sbGit.Run(ctx, bare, "config", "http.receivepack", "true")

	// Publish each survivor's branch tip into the new bare.
	for _, s := range all {
		if s.Branch == "" || s.Work == "" {
			continue
		}
		tmp := filepath.Join(dest, ".import-"+s.Name)
		_ = os.RemoveAll(tmp)
		if err := sbGit.Run(ctx, "", "clone", s.Work, tmp); err != nil {
			continue
		}
		c := &sbGit.Cmd{Dir: tmp}
		_ = c.Run(ctx, "remote", "remove", "new")
		_ = c.Run(ctx, "remote", "add", "new", bare)
		_ = c.Run(ctx, "push", "new", "HEAD:"+s.Branch)
		_ = os.RemoveAll(tmp)
	}

	work := filepath.Join(dest, canonical.WorkDir)
	if err := sbGit.Run(ctx, "", "clone", "--branch", "main", bare, work); err != nil {
		return nil, err
	}
	return &canonical.Host{
		Root: dest,
		PIN:  pin,
		Bare: bare,
		Work: work,
	}, nil
}

func commitTime(ctx context.Context, bare, ref string) int64 {
	out, err := sbGit.Output(ctx, bare, "log", "-1", "--format=%ct", ref)
	if err != nil {
		return 0
	}
	var t int64
	fmt.Sscanf(out, "%d", &t)
	return t
}
