// Package durability asynchronously mirrors main to a normal GitHub remote
// (plan 15). Optional, host-only, off the live sync path.
package durability

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sbGit "github.com/nwylynko/slopball-cli/git"
)

// Config is the optional mirror target. Empty RemoteURL disables mirroring.
type Config struct {
	RemoteURL string // e.g. https://github.com/org/repo.git
	// Token is a host-only credential (GITHUB_TOKEN / GH_TOKEN); never required of teammates.
	// It stays in the process environment for each push — never spliced into the
	// remote URL or written to .git/config (abuse-surface ticket 07).
	Token string
}

// Mirror pushes main from a bare/work repo to the durability remote asynchronously.
type Mirror struct {
	Bare   string
	Config Config

	mu       sync.Mutex
	lastPush time.Time
	pushes   int
	lastErr  error
}

// Enabled reports whether a remote is configured.
func (m *Mirror) Enabled() bool { return m != nil && m.Config.RemoteURL != "" }

// Trigger schedules an async push of main. Never blocks the live sync loop.
// Agent-driven around push boundaries (plan 15 lean on #8).
func (m *Mirror) Trigger(ctx context.Context) {
	if !m.Enabled() {
		return
	}
	go func() {
		_ = m.PushNow(context.Background())
	}()
}

// PushNow performs a synchronous mirror push (tests / shutdown flush).
func (m *Mirror) PushNow(ctx context.Context) error {
	if !m.Enabled() {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	url := m.Config.RemoteURL
	if httpRemote(url) && strings.TrimSpace(m.Config.Token) == "" {
		err := fmt.Errorf("no mirror credential: set GITHUB_TOKEN or GH_TOKEN")
		m.lastErr = err
		return err
	}

	remote := "slopball-durability"
	// Token-free URL only — a credential helper reads the token from the
	// process environment on each push. Splicing it into the remote URL used
	// to write it into .git/config for anything that can read the directory.
	_ = sbGit.Run(ctx, m.Bare, "remote", "remove", remote)
	if err := sbGit.Run(ctx, m.Bare, "remote", "add", remote, url); err != nil {
		m.lastErr = err
		return err
	}

	env := []string{"GIT_TERMINAL_PROMPT=0"}
	args := []string{"push", "--force", remote, "main:main"}
	if tok := strings.TrimSpace(m.Config.Token); tok != "" {
		// Keep the token in environ (owner-readable), never in argv. Rejected
		// alternative: http.extraHeader, which lands in ps the same way -e did.
		env = append(env, mirrorTokenEnv+"="+tok)
		args = append([]string{
			"-c", "credential.helper=",
			"-c", "credential.helper=" + mirrorCredentialHelper,
		}, args...)
	}
	c := &sbGit.Cmd{Dir: m.Bare, Env: env}
	if err := c.Run(ctx, args...); err != nil {
		m.lastErr = err
		return err
	}
	m.lastPush = time.Now()
	m.pushes++
	m.lastErr = nil
	return nil
}

// mirrorTokenEnv is what the credential-helper shim reads. Distinct from
// GITHUB_TOKEN so hermetic git Env can carry exactly the value we set without
// depending on whether the parent process exported the public name.
const mirrorTokenEnv = "SLOPBALL_MIRROR_TOKEN"

// mirrorCredentialHelper is an in-line git credential helper. git shells it,
// so $SLOPBALL_MIRROR_TOKEN expands from the push process's environment.
const mirrorCredentialHelper = `!f() { echo username=x-access-token; echo password=$` + mirrorTokenEnv + `; }; f`

func httpRemote(url string) bool {
	return strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://")
}

// Stats for tests / status.
func (m *Mirror) Stats() (pushes int, last time.Time, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pushes, m.lastPush, m.lastErr
}

// SeedFrom clones a GitHub repo into destBare (bookend: start from last snapshot).
func SeedFrom(ctx context.Context, remoteURL, destBare string) error {
	if err := os.MkdirAll(filepath.Dir(destBare), 0o755); err != nil {
		return err
	}
	if err := sbGit.Run(ctx, "", "clone", "--bare", remoteURL, destBare); err != nil {
		return fmt.Errorf("seed from mirror: %w", err)
	}
	return nil
}

// LoadConfig builds durability config from an explicit mirror URL (wizard /
// --mirror). The push token is always read from this machine's standard names.
func LoadConfig(remoteURL string) Config {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		tok = os.Getenv("GH_TOKEN")
	}
	return Config{RemoteURL: strings.TrimSpace(remoteURL), Token: tok}
}
