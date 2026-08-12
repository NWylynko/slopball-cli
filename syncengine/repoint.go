package syncengine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sbGit "github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/logx"
)

var repointLog = logx.New("repoint")

const prevRemote = "origin-prev"

// Cursors is the small on-disk cache under sessions/<pin>/cursors.json.
// Endpoint is the last DialAddr we successfully followed (or confirmed).
// Generation is the control-plane host generation last observed (plan 24).
type Cursors struct {
	Endpoint   string `json:"endpoint,omitempty"`
	Generation int    `json:"generation,omitempty"`
}

// OriginURL returns the configured URL for remote "origin" in repo.
func OriginURL(ctx context.Context, repo string) (string, error) {
	out, err := sbGit.Output(ctx, repo, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Repoint retargets origin on the work tree and (when set) the bare mirror to
// newRemote. Verifies the new endpoint with ls-remote before committing; on
// failure leaves origin untouched. Stashes the prior URL as origin-prev for
// rollback inspection. Idempotent when origin already equals newRemote.
func Repoint(ctx context.Context, work, mirror, newRemote string, id sbGit.Identity) error {
	_ = id // reserved for future authenticated remotes
	if newRemote == "" {
		return fmt.Errorf("repoint: empty remote")
	}
	cur, err := OriginURL(ctx, work)
	if err != nil {
		return fmt.Errorf("repoint: read origin: %w", err)
	}
	if urlsEqual(cur, newRemote) {
		return nil
	}
	// Probe before rewriting — a dead/hostile endpoint must not break the client.
	if err := sbGit.Run(ctx, "", "ls-remote", newRemote); err != nil {
		return fmt.Errorf("repoint: new endpoint unreachable, leaving origin at %s: %w", cur, err)
	}
	if err := setOrigin(ctx, work, cur, newRemote); err != nil {
		return err
	}
	if mirror != "" {
		mcur, _ := OriginURL(ctx, mirror)
		if err := setOrigin(ctx, mirror, mcur, newRemote); err != nil {
			// Roll work back so the pair stays consistent.
			_ = sbGit.Run(ctx, work, "remote", "set-url", "origin", cur)
			return fmt.Errorf("repoint mirror: %w", err)
		}
	}
	repointLog.Infof("origin %s → %s", cur, newRemote)
	return nil
}

func setOrigin(ctx context.Context, repo, prev, next string) error {
	if prev != "" && !urlsEqual(prev, next) {
		_ = sbGit.Run(ctx, repo, "remote", "remove", prevRemote)
		_ = sbGit.Run(ctx, repo, "remote", "add", prevRemote, prev)
	}
	return sbGit.Run(ctx, repo, "remote", "set-url", "origin", next)
}

// FollowOpts configures FollowHost — auto-repoint from the control plane.
type FollowOpts struct {
	Work, Mirror, PIN, Cursors string
	// Resolve must come from the trusted control plane for this PIN. A raw
	// DialAddr handed out-of-band must not be passed here (plan 22 trust note).
	// Generation is the host generation at resolve time (0 if unknown).
	Resolve func(ctx context.Context, pin string) (dialAddr string, generation int, err error)
	ID      sbGit.Identity
}

// FollowHost re-resolves the PIN and, when generation/URL moved, calls Repoint.
// Generation is compared first (plan 24); URL second. Steady state costs one
// resolve and no git traffic. Resolve failures never block — sync keeps using
// the current origin. ls-remote failure during Repoint likewise leaves origin
// alone and returns (false, nil).
func FollowHost(ctx context.Context, opt FollowOpts) (repointed bool, err error) {
	if opt.Resolve == nil || opt.PIN == "" {
		return false, nil
	}
	dial, gen, err := opt.Resolve(ctx, opt.PIN)
	if err != nil || dial == "" {
		return false, nil
	}
	cached := LoadCursors(opt.Cursors)
	if gen > 0 && cached.Generation == gen && urlsEqual(cached.Endpoint, dial) {
		return false, nil
	}
	if gen == 0 && urlsEqual(cached.Endpoint, dial) {
		return false, nil
	}
	origin, _ := OriginURL(ctx, opt.Work)
	if !urlsEqual(origin, dial) {
		if err := Repoint(ctx, opt.Work, opt.Mirror, dial, opt.ID); err != nil {
			repointLog.Warnf("follow %s: %v (keeping current origin)", opt.PIN, err)
			return false, nil
		}
		repointed = true
	}
	_ = SaveCursors(opt.Cursors, Cursors{Endpoint: dial, Generation: gen})
	return repointed, nil
}

// CatchUp pushes main from fromURL into toURL so a relocated canonical picks up
// post-seed merges before the rendezvous flip (plan 22 drain / catch-up).
func CatchUp(ctx context.Context, fromURL, toURL string) error {
	if fromURL == "" || toURL == "" {
		return fmt.Errorf("catch-up: need both from and to URLs")
	}
	if urlsEqual(fromURL, toURL) {
		return nil
	}
	tmp, err := os.MkdirTemp("", "slopball-catchup-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	bare := filepath.Join(tmp, "bare.git")
	if err := sbGit.Run(ctx, "", "clone", "--bare", fromURL, bare); err != nil {
		return fmt.Errorf("catch-up clone: %w", err)
	}
	if err := sbGit.Run(ctx, bare, "push", toURL, "+refs/heads/main:refs/heads/main"); err != nil {
		return fmt.Errorf("catch-up push main: %w", err)
	}
	return nil
}

// LoadCursors reads cursors.json; missing file → zero value.
func LoadCursors(path string) Cursors {
	if path == "" {
		return Cursors{}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Cursors{}
	}
	var c Cursors
	_ = json.Unmarshal(b, &c)
	return c
}

// SaveCursors writes cursors.json, creating the parent dir.
func SaveCursors(path string, c Cursors) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func urlsEqual(a, b string) bool {
	return strings.TrimRight(strings.TrimSpace(a), "/") == strings.TrimRight(strings.TrimSpace(b), "/")
}
