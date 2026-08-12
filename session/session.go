// Package session owns slopball's on-disk layout for a session and the metadata
// that describes this machine's role in it. It is the "session/PIN + join"
// vertical's foundation; the join daemon, git server, and mirror build on these
// paths. See MASTERPLAN §4.4 (canonical is a relocatable artifact) and §5.8 (the
// three copies a laptop host holds).
package session

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/nwylynko/slopball-cli/detect"
)

// Role is the part a machine plays in a session. A laptop host is both at once
// (MASTERPLAN §4.4: "a normal client + a box operator").
type Role string

const (
	RoleHost   Role = "host"
	RoleClient Role = "client"
)

// Home is slopball's root state directory. Override with $SLOPBALL_HOME (used by
// tests and by anyone keeping session state off the default path).
func Home() string {
	if h := os.Getenv("SLOPBALL_HOME"); h != "" {
		return h
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".slopball")
	}
	return ".slopball"
}

// Paths is the on-disk layout for one session, keyed by PIN. The three working
// copies mirror the three a laptop host holds (MASTERPLAN §5.8):
//
//	canonical/ — the relocatable canonical + running box (host/box role; §4.4)
//	mirror/    — the join daemon's background-fresh copy of main (client role; §5.8)
//	work/      — this client's dev-branch working tree (client role)
//
// A pure client omits canonical/; a pure cloud-box host omits work/. Keeping the
// full layout uniform is what makes host migration relocatable (§4.2).
type Paths struct {
	Root      string // sessions/<pin>
	Meta      string // session.json
	Canonical string // canonical/
	Mirror    string // mirror/
	Work      string // work/
	Cursors   string // cursors.json — sync / merge-base cursors
}

// ForPin returns the path layout for a session PIN.
func ForPin(pin string) Paths {
	root := filepath.Join(Home(), "sessions", pin)
	return Paths{
		Root:      root,
		Meta:      filepath.Join(root, "session.json"),
		Canonical: filepath.Join(root, "canonical"),
		Mirror:    filepath.Join(root, "mirror"),
		Work:      filepath.Join(root, "work"),
		Cursors:   filepath.Join(root, "cursors.json"),
	}
}

// Session is the persisted metadata for a hosted or joined session — enough for
// a restarted daemon or a rejoining client to pick the session back up.
type Session struct {
	PIN             string `json:"pin"`
	Role            Role   `json:"role"`
	Branch          string `json:"branch"`          // this client's dev branch (empty for a pure host)
	HostOverlayAddr string `json:"hostOverlayAddr"` // where the session git server lives on the overlay
	// MemberID is this machine's control-plane member id. Service leases are
	// held by member (plan 30), so a verb like `slopball take` needs to know
	// which member this machine is without re-joining to find out.
	MemberID   string         `json:"memberId,omitempty"`
	Capability detect.Profile `json:"capability,omitempty"`
}

// Save writes the session metadata, creating the session directory tree.
func (s Session) Save() error {
	p := ForPin(s.PIN)
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.Meta, b, 0o644)
}

// Load reads the session metadata for a PIN.
func Load(pin string) (Session, error) {
	var s Session
	b, err := os.ReadFile(ForPin(pin).Meta)
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}
