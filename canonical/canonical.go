// Package canonical owns the host's relocatable session artifact: a bare repo
// (the durable canonical) + a checked-out main working tree the demo runs off,
// plus the session git server clients push/fetch against (plans/03).
package canonical

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	sbGit "github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/gitserver"
	"github.com/nwylynko/slopball-cli/logx"
)

// Layout under a session's canonical/ directory.
const (
	BareDir = "bare.git"
	WorkDir = "work"
)

// Host is the one canonical copy for a session — standalone and relocatable
// (MASTERPLAN §4.4). Never entangled with the host human's personal worktree.
type Host struct {
	Root string // absolute path to canonical/
	PIN  string
	Bare string // Root/bare.git
	Work string // Root/work — checked out on main
	Srv  *gitserver.Server

	// Bind is the *direct* session-network listener mode from BindForControl
	// (see internal/netbind). The plain git HTTP listener is always loopback.
	// Set it before StartServer; empty means loopback (no direct published).
	Bind string

	// Session, when set, publishes canonical onto the session network (plan 09)
	// as well as the local interface, and makes SessionRemoteURL the address to
	// hand the control plane. Set it before StartServer.
	Session *gitserver.SessionNet

	// Remote is set for a remote-backed canonical (OpenRemote): the canonical
	// physically lives on another machine (a cloud box) and this Host is a local
	// replica the conductor drives over git. Empty for a local, on-disk host.
	Remote string
}

// SeedReadme is the only file a freshly created canonical carries. Exported
// because "is there a project on main yet?" is the question the setup role
// (plan 28) asks to pick scaffold vs adapt mode, and slopball's own seed must
// not read as somebody's repo.
func SeedReadme(pin string) string { return "# slopball session " + pin + "\n" }

// Create initializes a new canonical artifact under root. seed may be empty
// (blank project), a path to an existing directory to import, or ignored for now.
func Create(ctx context.Context, root, pin string) (*Host, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	h := &Host{
		Root: abs,
		PIN:  pin,
		Bare: filepath.Join(abs, BareDir),
		Work: filepath.Join(abs, WorkDir),
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	id := sbGit.SessionIdentity("host", pin)

	if err := sbGit.Run(ctx, "", "init", "--bare", h.Bare); err != nil {
		return nil, err
	}
	// Allow HTTP push once the session server is up.
	_ = sbGit.Run(ctx, h.Bare, "config", "http.receivepack", "true")
	_ = sbGit.Run(ctx, h.Bare, "config", "receive.denyCurrentBranch", "ignore")

	// Seed main via a temporary clone, then discard it for the durable work tree.
	tmp := filepath.Join(abs, ".seed")
	_ = os.RemoveAll(tmp)
	if err := sbGit.Run(ctx, "", "clone", h.Bare, tmp); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte(SeedReadme(pin)), 0o644); err != nil {
		return nil, err
	}
	c := &sbGit.Cmd{Dir: tmp, Env: id.EnvVars()}
	if err := c.Run(ctx, "add", "README.md"); err != nil {
		return nil, err
	}
	if err := c.Run(ctx, "commit", "-m", "seed canonical"); err != nil {
		return nil, err
	}
	if err := c.Run(ctx, "branch", "-M", "main"); err != nil {
		return nil, err
	}
	if err := c.Run(ctx, "push", "origin", "main"); err != nil {
		return nil, err
	}
	_ = os.RemoveAll(tmp)

	if err := sbGit.Run(ctx, "", "clone", "--branch", "main", h.Bare, h.Work); err != nil {
		return nil, err
	}
	h.Srv = &gitserver.Server{Bare: h.Bare}
	return h, nil
}

// Open rehydrates a Host from an existing relocatable directory.
func Open(root, pin string) (*Host, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	h := &Host{
		Root: abs,
		PIN:  pin,
		Bare: filepath.Join(abs, BareDir),
		Work: filepath.Join(abs, WorkDir),
		Srv:  &gitserver.Server{Bare: filepath.Join(abs, BareDir)},
	}
	if _, err := os.Stat(h.Bare); err != nil {
		return nil, fmt.Errorf("canonical bare missing at %s: %w", h.Bare, err)
	}
	return h, nil
}

// OpenRemote builds a local replica of a canonical that lives on another
// machine (a cloud box) so an off-box conductor can drive it: the work tree's
// origin is the box's git URL (fetch client branches / push merged main go
// straight to the box), and a local mirror bare keeps the refs the merger
// inspects (BranchesAheadOfMain / intent notes). Idempotent — reuses an
// existing replica dir. This is what makes the elected-conductor (plan 21)
// path real: canonical stays on the box, merge intelligence runs on the
// elector's laptop under its harness login.
func OpenRemote(ctx context.Context, root, pin, remoteURL string) (*Host, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	h := &Host{
		Root:   abs,
		PIN:    pin,
		Bare:   filepath.Join(abs, BareDir),
		Work:   filepath.Join(abs, WorkDir),
		Remote: remoteURL,
	}
	if _, err := os.Stat(h.Bare); err != nil {
		if err := sbGit.Run(ctx, "", "clone", "--mirror", remoteURL, h.Bare); err != nil {
			return nil, fmt.Errorf("mirror canonical from %s: %w", remoteURL, err)
		}
	}
	if _, err := os.Stat(h.Work); err != nil {
		if err := sbGit.Run(ctx, "", "clone", "--branch", "main", remoteURL, h.Work); err != nil {
			return nil, fmt.Errorf("clone canonical work from %s: %w", remoteURL, err)
		}
	}
	if err := h.Refresh(ctx); err != nil {
		return nil, err
	}
	return h, nil
}

// Refresh pulls the latest refs from the remote canonical into the local mirror
// bare. No-op for a local (on-disk) host. Call before each conductor tick so
// BranchesAheadOfMain sees freshly pushed client branches and the current main.
func (h *Host) Refresh(ctx context.Context) error {
	if h.Remote == "" || h.Bare == "" {
		return nil
	}
	return sbGit.Run(ctx, h.Bare, "remote", "update", "--prune")
}

// StartServer begins serving the bare repo over HTTP. The plain listener is
// always loopback; h.Bind governs the direct session-network listener only
// (see gitserver.Server.Bind).
func (h *Host) StartServer() (string, error) {
	if h.Srv == nil {
		h.Srv = &gitserver.Server{Bare: h.Bare}
	}
	if h.Srv.Bind == "" {
		h.Srv.Bind = h.Bind
	}
	if h.Srv.Session == nil {
		h.Srv.Session = h.Session
	}
	return h.Srv.Start("canonical.git")
}

// SessionRemoteURL is the session-network address of this canonical, empty when
// it is not on the session network. Callers announcing an endpoint should
// prefer it over RemoteURL: it names the session rather than this machine, so
// it stays correct when the git lease moves.
func (h *Host) SessionRemoteURL() string {
	if h.Srv == nil {
		return ""
	}
	return h.Srv.SessionURL()
}

// RemoteURL is the live session remote clients clone/push against.
func (h *Host) RemoteURL() string {
	if h.Remote != "" {
		return h.Remote
	}
	if h.Srv != nil && h.Srv.URL() != "" {
		return h.Srv.URL()
	}
	return h.Bare // file-path fallback
}

// Close shuts the session git server.
func (h *Host) Close(ctx context.Context) error {
	if h.Srv == nil {
		return nil
	}
	return h.Srv.Close(ctx)
}

// CreateClientBranch makes a new branch off main in the bare repo.
func (h *Host) CreateClientBranch(ctx context.Context, name string) error {
	return sbGit.Run(ctx, h.Bare, "branch", name, "main")
}

// BranchesAheadOfMain lists branches whose tip is not an ancestor of main.
func (h *Host) BranchesAheadOfMain(ctx context.Context) ([]string, error) {
	out, err := sbGit.Output(ctx, h.Bare, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if err != nil {
		return nil, err
	}
	var ahead []string
	for _, b := range strings.Split(strings.TrimSpace(out), "\n") {
		b = strings.TrimSpace(b)
		if b == "" || b == "main" {
			continue
		}
		// exit 0 = ancestor; non-zero = ahead or divergent
		if err := sbGit.Run(ctx, h.Bare, "merge-base", "--is-ancestor", b, "main"); err != nil {
			ahead = append(ahead, b)
		}
	}
	return ahead, nil
}

// MergeBase returns the merge-base of main and branch.
func (h *Host) MergeBase(ctx context.Context, branch string) (string, error) {
	out, err := sbGit.Output(ctx, h.Bare, "merge-base", "main", branch)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// SyncWorkToMain fast-forwards the demo work tree to bare's main after a merge
// advances it. The reset is unconditional: canonical/work is the machine-owned
// demo mirror ("tracks main, do not edit"), never an edit surface — humans and
// agents edit in the session's work/ tree and reach main through `slopball
// sync`. Local modifications here are slopball's own churn, above all the
// lockfile its dependency install rewrites in this very tree, and they lose to
// main by design.
//
// An earlier version skipped the reset whenever the tree was dirty. One install
// then froze the mirror forever: main advanced, origin/main moved, and the
// supervised dev server served a tree hours behind while the tick loop logged a
// warning every 2s. Being current is the mirror's whole job — see
// TestDemoMirrorTracksMainThroughInstallChurn.
//
// `reset --hard` only rewrites tracked files, so untracked installed trees
// (node_modules, .next) survive; nothing here cleans them, because reinstalling
// dependencies on every tick would be ruinous.
func (h *Host) SyncWorkToMain(ctx context.Context) error {
	c := &sbGit.Cmd{Dir: h.Work}
	if err := c.Run(ctx, "fetch", "origin", "main"); err != nil {
		return err
	}
	if logx.DebugEnabled() {
		if status, err := c.Output(ctx, "status", "--porcelain"); err == nil && strings.TrimSpace(status) != "" {
			logx.New("canonical").Debugf("demo mirror %s had local modifications, overwriting with main:\n%s", h.Work, strings.TrimSpace(status))
		}
	}
	return c.Run(ctx, "reset", "--hard", "origin/main")
}
