package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// LiveMarker is written while a host or join process is running on this machine.
type LiveMarker struct {
	PID       int       `json:"pid"`
	Role      Role      `json:"role"`
	Branch    string    `json:"branch,omitempty"`
	StartedAt time.Time `json:"startedAt"`
}

// LiveEntry is one live session discovered under $SLOPBALL_HOME.
type LiveEntry struct {
	PIN  string
	Live LiveMarker
}

// Live is the on-disk liveness file for a session.
func (p Paths) Live() string { return filepath.Join(p.Root, "live.json") }

// WriteLive records that this process is holding the session open.
func WriteLive(s Session) error {
	return WriteLiveMarker(s.PIN, LiveMarker{
		PID:       os.Getpid(),
		Role:      s.Role,
		Branch:    BranchLabel(s.Branch),
		StartedAt: time.Now().UTC(),
	})
}

// WriteLiveMarker writes an explicit marker (tests).
func WriteLiveMarker(pin string, m LiveMarker) error {
	p := ForPin(pin)
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.Live(), b, 0o644)
}

// ClearLive removes the liveness marker for a PIN.
func ClearLive(pin string) error {
	err := os.Remove(ForPin(pin).Live())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// LiveHere reports whether THIS machine is currently holding one session open,
// pid-checked. A stale marker (the daemon was killed, the laptop lost power) is
// deleted and reads as not-live, which is what lets a rejoin proceed instead of
// being locked out by the corpse of the process it is replacing.
func LiveHere(pin string) (LiveMarker, bool) {
	path := ForPin(pin).Live()
	b, err := os.ReadFile(path)
	if err != nil {
		return LiveMarker{}, false
	}
	var m LiveMarker
	if err := json.Unmarshal(b, &m); err != nil {
		_ = os.Remove(path)
		return LiveMarker{}, false
	}
	if !processAlive(m.PID) {
		_ = os.Remove(path)
		return LiveMarker{}, false
	}
	return m, true
}

// LivePins lists sessions with a live marker whose pid is still running.
// Stale markers are deleted. Order: most recently started first.
func LivePins() ([]LiveEntry, error) {
	root := filepath.Join(Home(), "sessions")
	ents, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []LiveEntry
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		pin := e.Name()
		m, ok := LiveHere(pin)
		if !ok {
			continue
		}
		out = append(out, LiveEntry{PIN: pin, Live: m})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Live.StartedAt.After(out[j].Live.StartedAt)
	})
	return out, nil
}

// BranchLabel is the human-facing name from a git branch (client/alice → alice).
func BranchLabel(branch string) string {
	if branch == "" {
		return ""
	}
	if i := strings.LastIndex(branch, "/"); i >= 0 {
		return branch[i+1:]
	}
	return branch
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks existence without delivering a signal (POSIX).
	return p.Signal(syscall.Signal(0)) == nil
}
