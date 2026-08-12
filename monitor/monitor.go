// Package monitor gives a live, read-only view of a slopball session for
// hands-on testing. Prefer the control-plane view (--json, one resolved PIN);
// --local keeps the on-disk path for machines with session state but no
// control-plane reachability.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nwylynko/slopball-cli/controlplane"
	sbGit "github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/placement"
	"github.com/nwylynko/slopball-cli/reach"
	"github.com/nwylynko/slopball-cli/runtime"
	"github.com/nwylynko/slopball-cli/session"
)

// Status is one session's live snapshot.
type Status struct {
	PIN        string
	Role       string
	Branch     string
	Main       string
	Head       string
	Ahead      int
	AheadNames []string
	Registered bool
	GitURL     string
	GitUp      bool
	// GitPath is how this process most recently reached the git service —
	// "direct <addr>" or "relay <addr>" (plan 38 step 4). A dual path whose
	// choice you cannot see is the thing that wastes an hour on stage.
	GitPath string
	Remote  string
	DevURL  string
	DevUp   bool
	Note    string
	// Services is where each leased service currently runs (plan 30), keyed by
	// service name. Once services can move on their own, "where is everything
	// right now?" has to be a question with a one-glance answer.
	Services map[string]string
	// Members is the roster as the control plane knows it, carrying each
	// member's RAW build version (plan 48 step 4). Populated only from the
	// control plane — a laptop cannot know what its teammates are running.
	Members []MemberLine
}

// MemberLine is one member on the monitor's roster: who, in what role, running
// which build.
//
// Version is deliberately raw and deliberately unjudged. The floor is a
// constant compiled into the control plane, and a below-floor verdict rendered
// here would be a SECOND copy of it — one that can disagree with the copy that
// actually refuses, on a laptop that may be a release behind. The verdict lives
// in `slopball-control admin versions`, which evaluates against its own
// compiled-in floor. Blank means the member has never reported a version (a
// binary from before the header, or one that has not called since); it renders
// as a dash, never as an empty cell, because blank is "old or unknown" and an
// empty cell reads as "fine".
type MemberLine struct {
	Name    string
	Role    string
	Version string
	Online  bool
}

// Snapshot discovers every session under home and returns its status.
// controlBase is the control-plane URL (empty → skip registration/git probes).
func Snapshot(ctx context.Context, home, controlBase string) []Status {
	pins := Discover(home)
	out := make([]Status, 0, len(pins))
	var client *controlplane.Client
	if controlBase != "" {
		client = controlplane.NewClient(controlBase)
	}
	for _, pin := range pins {
		out = append(out, statusFor(ctx, home, pin, client))
	}
	return out
}

// Discover lists the session PINs that have state under home/sessions.
func Discover(home string) []string {
	root := filepath.Join(home, "sessions")
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var pins []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "session.json")); err == nil {
			pins = append(pins, e.Name())
		}
	}
	return pins
}

func statusFor(ctx context.Context, home, pin string, client *controlplane.Client) Status {
	st := Status{PIN: pin, Main: "-", Head: "-"}
	meta, err := loadSession(home, pin)
	if err != nil {
		st.Note = "no session.json: " + err.Error()
		return st
	}
	st.Role = string(meta.Role)
	st.Branch = meta.Branch
	paths := forPin(home, pin)

	bare := filepath.Join(paths.Canonical, "bare.git")
	work := paths.Work
	if isDir(filepath.Join(paths.Canonical, "work")) {
		work = filepath.Join(paths.Canonical, "work")
	}
	if isDir(bare) {
		st.Main = shortSha(ctx, bare, "main")
		st.AheadNames = branchesAhead(ctx, bare)
		st.Ahead = len(st.AheadNames)
	} else if isDir(paths.Mirror) {
		st.Main = shortSha(ctx, paths.Mirror, "main")
	}
	if isDir(work) {
		st.Head = shortSha(ctx, work, "HEAD")
	}
	if isDir(filepath.Join(paths.Root, "replica")) {
		st.Remote = "remote canonical (replica present)"
	}

	if client != nil {
		if sess, err := client.Session(ctx, pin); err == nil {
			st.Registered = true
			now := time.Now()
			st.Services = map[string]string{}
			for _, svc := range controlplane.Services {
				st.Services[svc] = placement.Describe(sess, svc, now)
			}
			for _, m := range sess.Members {
				st.Members = append(st.Members, MemberLine{
					Name: m.Name, Role: m.Role, Version: m.Version, Online: m.Online,
				})
			}
			// raw endpoint ok: shown as published; the probe uses Dialable.
			if ep, ok := sess.Endpoints[controlplane.EndpointGit]; ok {
				// Show the published address — it is what the session actually
				// advertises — but PROBE something dialable. A `slop://` URL
				// probed literally always reads as down, which would make the
				// monitor call every healthy session-network host dead.
				st.GitURL = ep.URL
				git := reach.ProbeSessionService(ctx, client, sess, pin, reach.ServiceGit)
				st.GitUp = git.Reachable
				st.GitPath = git.Via
			}
			// Dev is the one endpoint a human OPENS, so the monitor shows the
			// resolved address rather than the published one (plan 41) — a
			// `slop://` URL on screen is something nobody can click. git is the
			// other way round: it is published for machines, and `origin` is
			// resolved where it is used.
			if devURL, err := client.EndpointURL(ctx, pin, controlplane.EndpointDev); err == nil && devURL != "" {
				st.DevURL = devURL
				st.DevUp = reach.ProbeSessionService(ctx, client, sess, pin, reach.ServiceDev).Reachable
			}
			if sess.Convergence != nil {
				if st.Main == "-" && sess.Convergence.MainSHA != "" {
					st.Main = short(sess.Convergence.MainSHA)
				}
				st.AheadNames = sess.Convergence.BranchesAhead
				st.Ahead = len(st.AheadNames)
			}
		} else if st.Note == "" {
			st.Note = "control: " + err.Error()
		}
	}
	if st.DevURL == "" {
		if url := devURL(work); url != "" {
			st.DevURL = url
			st.DevUp = reach.ProbeHTTP(ctx, url)
		}
	}
	return st
}

func branchesAhead(ctx context.Context, bare string) []string {
	out, err := sbGit.Output(ctx, bare, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if err != nil {
		return nil
	}
	var ahead []string
	for _, b := range strings.Split(strings.TrimSpace(out), "\n") {
		b = strings.TrimSpace(b)
		if b == "" || b == "main" {
			continue
		}
		if err := sbGit.Run(ctx, bare, "merge-base", "--is-ancestor", b, "main"); err != nil {
			ahead = append(ahead, b)
		}
	}
	return ahead
}

func shortSha(ctx context.Context, repo, ref string) string {
	out, err := sbGit.Output(ctx, repo, "rev-parse", "--short", ref)
	if err != nil {
		return "-"
	}
	if s := strings.TrimSpace(out); s != "" {
		return s
	}
	return "-"
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func devURL(work string) string {
	port, _ := runtime.LocalDevPort(work)
	if port <= 0 || !runtime.LocalPortListening(port) {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func loadSession(home, pin string) (session.Session, error) {
	var s session.Session
	b, err := os.ReadFile(filepath.Join(home, "sessions", pin, "session.json"))
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}

func forPin(home, pin string) session.Paths {
	root := filepath.Join(home, "sessions", pin)
	return session.Paths{
		Root:      root,
		Canonical: filepath.Join(root, "canonical"),
		Mirror:    filepath.Join(root, "mirror"),
		Work:      filepath.Join(root, "work"),
	}
}
