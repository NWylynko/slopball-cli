// Package trust hardens conductor placement for local vs remote-box hosts
// (plans/21, MASTERPLAN §10): local-only by default; election + warning for a
// cloud box. Conductor intelligence uses a harness CLI under a subscription
// login on the electing laptop (conductor-on-elector) — never provider API
// tokens, and never harness login material on the box.
package trust

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// HostKind is where canonical lives (not necessarily where the harness runs).
type HostKind string

const (
	// LocalHost — laptop / mesh / local box owned by the conductor runner.
	// Harness login stays on-machine; the trust hole never opens.
	LocalHost HostKind = "local"
	// RemoteBox — cloud container someone else owns. Needs election so AI roles
	// run on the elector's laptop (conductor-on-elector).
	RemoteBox HostKind = "remote-box"
)

// RunnerFile is the only conductor-related file written onto a remote box.
// It names who runs the harness — never API keys or subscription tokens.
const RunnerFile = ".slopball/conductor-runner.json"

// Broker owns election state for harness-CLI conductor placement.
type Broker struct {
	Kind HostKind

	mu       sync.Mutex
	elector  string
	harness  string
	model    string
	runners  map[string]string // runnerID → elector
	exports  int               // forbidden secret pushes (must stay 0)
	warnings []string
	closed   bool
	activeID string
}

// Lease is what a remote box may record: who runs the harness, which CLI.
// No secrets.
type Lease struct {
	Elector  string
	Harness  string
	Model    string
	RunnerID string
	Warning  string
}

// RunnerAuth is the on-box marker shape — intentionally has no credential fields.
type RunnerAuth struct {
	Elector  string `json:"elector"`
	Harness  string `json:"harness"`
	Model    string `json:"model,omitempty"`
	RunnerID string `json:"runner_id"`
	Active   bool   `json:"active"`
}

// NewBroker starts trust state for a host kind.
func NewBroker(kind HostKind) *Broker {
	return &Broker{Kind: kind, runners: map[string]string{}}
}

// WarningForElect is the clear human/agent warning before electing a harness
// runner for a non-owned host.
func WarningForElect(elector, harnessName string) string {
	if harnessName == "" {
		harnessName = "harness"
	}
	return fmt.Sprintf(
		"WARNING: electing %s's %s harness as the conductor runner for a host someone else owns.\n"+
			"AI roles will run on THIS laptop (where the harness is logged in); login never lands on the box.\n"+
			"Revoke anytime with `slopball elect --revoke`.",
		elector, harnessName,
	)
}

// WarnBeforeElect records and returns the cloud-election warning. LocalHost
// returns a no-op notice that election is unnecessary.
func (b *Broker) WarnBeforeElect(elector, harnessName string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var w string
	if b.Kind == LocalHost {
		w = "local host: harness login stays on this machine — election not needed"
	} else {
		w = WarningForElect(elector, harnessName)
	}
	b.warnings = append(b.warnings, w)
	return w
}

// LastWarning returns the most recent warning text.
func (b *Broker) LastWarning() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.warnings) == 0 {
		return ""
	}
	return b.warnings[len(b.warnings)-1]
}

// ElectRequest carries what the electing laptop provides — harness name only,
// never an API key.
type ElectRequest struct {
	Elector string // who is electing (agent/client name)
	Harness string // claude / codex / cursor
	Model   string // optional model override
}

// Elect registers this laptop as the conductor harness runner for a RemoteBox.
// Refuses on LocalHost. Re-election replaces the previous runner id.
func (b *Broker) Elect(req ElectRequest) (*Lease, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, fmt.Errorf("trust broker closed")
	}
	if b.Kind != RemoteBox {
		return nil, fmt.Errorf("election refused on local host — harness login must not leave this machine")
	}
	if req.Elector == "" || req.Harness == "" {
		return nil, fmt.Errorf("elect: elector and harness are required")
	}
	warn := WarningForElect(req.Elector, req.Harness)
	b.warnings = append(b.warnings, warn)

	b.runners = map[string]string{}
	id, err := mintRunnerID()
	if err != nil {
		return nil, err
	}
	b.runners[id] = req.Elector
	b.activeID = id
	b.elector = req.Elector
	b.harness = req.Harness
	b.model = req.Model

	return &Lease{
		Elector:  req.Elector,
		Harness:  req.Harness,
		Model:    req.Model,
		RunnerID: id,
		Warning:  warn,
	}, nil
}

// SelfElect is the no-remote-box host-leave path: a remaining client becomes
// the local conductor with their own on-machine harness. No secrets exported.
func (b *Broker) SelfElect(name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if name == "" {
		return fmt.Errorf("self-elect: name required")
	}
	b.Kind = LocalHost
	b.elector = name
	b.runners = map[string]string{}
	b.activeID = ""
	b.warnings = append(b.warnings, fmt.Sprintf("%s self-elected as local conductor — harness login stays on their machine", name))
	return nil
}

// Revoke invalidates a runner id. The elected laptop should stop acting.
func (b *Broker) Revoke(runnerID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.runners[runnerID]; !ok {
		return false
	}
	delete(b.runners, runnerID)
	if b.activeID == runnerID {
		b.activeID = ""
	}
	return true
}

// Active reports whether runnerID is the current elected runner.
func (b *Broker) Active(runnerID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.runners[runnerID]
	return ok && b.activeID == runnerID
}

// Elector returns the current electing client name, if any.
func (b *Broker) Elector() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.elector
}

// Harness returns the elected harness name, if any.
func (b *Broker) Harness() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.harness
}

// SecretExports is how many times a durable secret would have been pushed to a
// remote host. Must stay 0.
func (b *Broker) SecretExports() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exports
}

// RecordSecretExport marks a (forbidden) secret push — tests / defensive hooks.
func (b *Broker) RecordSecretExport() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.exports++
}

// Close clears election state.
func (b *Broker) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.runners = map[string]string{}
	b.activeID = ""
	return nil
}

func mintRunnerID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "sb_" + hex.EncodeToString(b[:]), nil
}

// InstallOnBox writes the runner marker onto the remote box work tree.
// Never writes API keys or harness login material.
func InstallOnBox(boxRoot string, lease *Lease) (string, error) {
	if lease == nil {
		return "", fmt.Errorf("install: nil lease")
	}
	if looksLikeSecret(lease.RunnerID) || looksLikeSecret(lease.Harness) {
		return "", fmt.Errorf("install: refusing secret-looking material on remote box")
	}
	auth := RunnerAuth{
		Elector:  lease.Elector,
		Harness:  lease.Harness,
		Model:    lease.Model,
		RunnerID: lease.RunnerID,
		Active:   true,
	}
	path := filepath.Join(boxRoot, RunnerFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	body, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// WipeBox removes on-box runner marker after the session.
func WipeBox(boxRoot string) error {
	path := filepath.Join(boxRoot, RunnerFile)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(filepath.Dir(path))
	return nil
}

// LoadRunnerAuth reads on-box runner marker.
func LoadRunnerAuth(boxRoot string) (RunnerAuth, error) {
	var a RunnerAuth
	b, err := os.ReadFile(filepath.Join(boxRoot, RunnerFile))
	if err != nil {
		return a, err
	}
	err = json.Unmarshal(b, &a)
	return a, err
}

// DeactivateOnBox marks the on-box runner inactive (revoke without full wipe).
func DeactivateOnBox(boxRoot string) error {
	a, err := LoadRunnerAuth(boxRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	a.Active = false
	path := filepath.Join(boxRoot, RunnerFile)
	body, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

// BoxHoldsSecret scans the box tree for a forbidden secret string (API keys,
// etc.). Used by acceptance tests.
func BoxHoldsSecret(boxRoot, secret string) (bool, error) {
	if secret == "" {
		return false, nil
	}
	found := false
	err := filepath.WalkDir(boxRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		buf := make([]byte, 64*1024)
		n, _ := io.ReadFull(f, buf)
		if strings.Contains(string(buf[:n]), secret) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found, err
}

func looksLikeSecret(s string) bool {
	return strings.HasPrefix(s, "sk-ant-") ||
		(strings.HasPrefix(s, "sk-") && len(s) > 20) ||
		strings.Contains(s, "ANTHROPIC_API_KEY")
}

// PersistFile is session-local elect state on the electing laptop (not the box).
const PersistFile = "trust-elect.json"

// PersistState is what the electing laptop keeps so `slopball elect --revoke`
// can clear the election. Never contains API keys or harness login material.
type PersistState struct {
	PIN      string `json:"pin"`
	Elector  string `json:"elector"`
	Harness  string `json:"harness"`
	Model    string `json:"model,omitempty"`
	RunnerID string `json:"runner_id"`
	BoxRoot  string `json:"box_root,omitempty"`
}

// SavePersist writes elect state under the session root.
func SavePersist(sessionRoot string, st PersistState) error {
	path := filepath.Join(sessionRoot, PersistFile)
	body, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

// LoadPersist reads elect state from the session root.
func LoadPersist(sessionRoot string) (PersistState, error) {
	var st PersistState
	b, err := os.ReadFile(filepath.Join(sessionRoot, PersistFile))
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(b, &st)
	return st, err
}

// ClearPersist removes elect state from the session root.
func ClearPersist(sessionRoot string) error {
	err := os.Remove(filepath.Join(sessionRoot, PersistFile))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
