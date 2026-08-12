// Package runtime reconciles non-file runtime state as main advances (plans/20):
// migrations against the live DB, shared ports/.env, gated reseed, and process restart.
package runtime

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/devserver"
	sbGit "github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/logx"
	"github.com/nwylynko/slopball-cli/netbind"
)

// SharedEnvPath is the tracked shared runtime config. Host materializes .env from
// it; clients agree via git (no silent host/client port divergence).
const SharedEnvPath = ".slopball/runtime.env"

// ReseedSignal in a push intent authorizes destructive reset/reseed. Never inferred.
const ReseedSignal = "slopball:reseed"

// Paths that count as "migration files" when they change on main.
var migrationPrefixes = []string{
	"migrations/",
	"prisma/migrations/",
	"drizzle/",
	"supabase/migrations/",
	"db/migrations/",
	"db/migrate/",
}

// Options configure a one-shot Reconcile between two main tips.
type Options struct {
	WorkDir    string
	MigrateCmd []string // override auto-detect
	ReseedCmd  []string // override; empty = no-op even with signal
	Dev        *devserver.Supervisor
	GitRun     func(ctx context.Context, dir string, args ...string) (string, error)
}

// Result is what Reconcile did for one main advance.
type Result struct {
	Migrated   bool
	Reseeded   bool
	EnvApplied bool
	Restarted  bool
}

// Reconciler is a conductor After-role: on each Tick, if main advanced since the
// last tick, apply runtime hooks (migrations / shared env / gated reseed) and
// restart the supervised process when live state changed.
type Reconciler struct {
	WorkDir    string
	MigrateCmd []string
	ReseedCmd  []string
	Dev        *devserver.Supervisor
	GitRun     func(ctx context.Context, dir string, args ...string) (string, error)
	// Control plane announcement (plan 24) — optional.
	Control    *controlplane.Client
	PIN        string
	Generation int

	mu       sync.Mutex
	lastHEAD string
	lastDev  string
	// sessionDevURL / sessionDevDirect are what the dev holder publishes, when
	// this member holds the dev lease and a relay is configured.
	sessionDevURL    string
	sessionDevDirect string
	saidWrongBind    bool // ticket 21: one diagnostic when running but not on DevPort
	Migrations       int
	Reseeds          int
	EnvApplies       int
	Restarts         int
}

func (r *Reconciler) Name() string { return "runtime" }

// Tick baselines on first call, then reconciles each subsequent main advance.
func (r *Reconciler) Tick(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	gitRun := r.git()
	head, err := gitRun(ctx, r.WorkDir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	head = strings.TrimSpace(head)
	if r.lastHEAD == "" {
		r.lastHEAD = head
		return nil
	}
	if head == r.lastHEAD {
		return nil
	}
	res, err := Reconcile(ctx, Options{
		WorkDir:    r.WorkDir,
		MigrateCmd: r.MigrateCmd,
		ReseedCmd:  r.ReseedCmd,
		Dev:        r.Dev,
		GitRun:     gitRun,
	}, r.lastHEAD, head)
	if err != nil {
		// Don't advance past a failed range — retry it next tick so a transient
		// migration/reseed failure recovers instead of being skipped forever.
		// The failure is already in the dev log for the error-watcher.
		return err
	}
	r.lastHEAD = head
	r.account(res)
	r.announceDev(ctx)
	return nil
}

// ReconcileFromTo applies runtime hooks for an explicit old→new main range
// (emulator / host loop after SyncWorkToMain). Updates lastHEAD.
func (r *Reconciler) ReconcileFromTo(ctx context.Context, oldHEAD, newHEAD string) (*Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	oldHEAD = strings.TrimSpace(oldHEAD)
	newHEAD = strings.TrimSpace(newHEAD)
	if oldHEAD == "" || newHEAD == "" || oldHEAD == newHEAD {
		if newHEAD != "" {
			r.lastHEAD = newHEAD
		}
		return &Result{}, nil
	}
	res, err := Reconcile(ctx, Options{
		WorkDir:    r.WorkDir,
		MigrateCmd: r.MigrateCmd,
		ReseedCmd:  r.ReseedCmd,
		Dev:        r.Dev,
		GitRun:     r.git(),
	}, oldHEAD, newHEAD)
	r.lastHEAD = newHEAD
	if err != nil {
		return res, err
	}
	r.account(res)
	// The host loop drives this method rather than Tick, so announcing only there
	// meant a live host never published its dev URL.
	r.announceDev(ctx)
	return res, nil
}

// DevURL is the dev endpoint most recently announced, or empty before something
// is listening on LocalDevPort (ticket 21). The error-watcher's health probe
// reads it so the probe and the endpoint everyone else dials can never disagree.
func (r *Reconciler) DevURL() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastDev
}

// AnnounceDev publishes the dev/demo endpoints when something is listening on
// LocalDevPort. Callers use it at startup and on every host tick so a cold
// start becomes published once the process accepts connections — never via a
// grace timer.
func (r *Reconciler) AnnounceDev(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.announceDev(ctx)
}

// SetGeneration follows the host onto a new control-plane generation. Announcing
// at the old one is rejected, so a Reconciler left behind by a cutover would go
// quiet — no dev URL for the rest of the session.
// SetSessionDev records the session address the dev holder publishes, and the
// machine address peers may dial directly (plan 41). With one set, the dev
// endpoint names the session's dev SERVICE instead of this machine — which is
// what makes it openable from another network, and what keeps it true when the
// dev lease migrates.
//
// With none set the endpoint stays byte-for-byte today's http://<host>:<port>:
// the session network is not a new requirement for a same-machine session.
func (r *Reconciler) SetSessionDev(url, direct string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessionDevURL, r.sessionDevDirect = url, direct
}

func (r *Reconciler) SetGeneration(gen int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Generation = gen
}

func (r *Reconciler) account(res *Result) {
	if res == nil {
		return
	}
	if res.Migrated {
		r.Migrations++
	}
	if res.Reseeded {
		r.Reseeds++
	}
	if res.EnvApplied {
		r.EnvApplies++
	}
	if res.Restarted {
		r.Restarts++
	}
}

func (r *Reconciler) git() func(ctx context.Context, dir string, args ...string) (string, error) {
	if r.GitRun != nil {
		return r.GitRun
	}
	return sbGit.Output
}

func (r *Reconciler) announceDev(ctx context.Context) {
	if r.Control == nil || r.PIN == "" || r.Generation <= 0 {
		return
	}
	log := logx.New("runtime")
	// The same resolution the session-network dev holder uses, so the
	// endpoint and the holder can never name different ports. There is no
	// operator-asserted advertise override — the session (or loopback) is
	// the only address path.
	port, _ := LocalDevPort(r.WorkDir)
	if port <= 0 {
		return
	}
	host, err := netbind.AdvertiseHostMode("")
	if err != nil || host == "" {
		host = "127.0.0.1"
	}
	u := fmt.Sprintf("http://%s:%d", host, port)
	// Publish only when something is LISTENING on the port we would announce.
	// With a constant there is always a number; publishing on that alone makes
	// every not-yet-booted server read as broken (ticket 21). No grace timer.
	if !LocalPortListening(port) {
		r.diagnoseWrongBind(log, port)
		if r.lastDev != "" {
			_ = r.Control.ClearEndpoint(ctx, r.PIN, controlplane.EndpointDev, r.Generation)
			_ = r.Control.ClearEndpoint(ctx, r.PIN, controlplane.EndpointDemo, r.Generation)
			r.lastDev = ""
		}
		return
	}
	r.saidWrongBind = false
	r.lastDev = u
	// The dev ENDPOINT is the session address when this member is publishing
	// one; the DEMO endpoint deliberately stays the machine address, because it
	// means "the URL an outsider gets" and that is plan 17's business.
	devURL := u
	if r.sessionDevURL != "" {
		devURL = r.sessionDevURL
	}
	r.Control.PutEndpointBestEffort(ctx, r.PIN, controlplane.EndpointDev, controlplane.EndpointPut{
		URL: devURL, Direct: r.sessionDevDirect, Host: host, Port: port, Generation: r.Generation, Source: "runtime",
	})
	r.Control.PutEndpointBestEffort(ctx, r.PIN, controlplane.EndpointDemo, controlplane.EndpointPut{
		URL: u, Generation: r.Generation, Source: "runtime",
	})
}

// diagnoseWrongBind names a silent mismatch: the child is alive and its own
// logs claim a different listen port. A cold start (alive, silent, not yet
// listening) must not warn — that is "coming up", not broken.
func (r *Reconciler) diagnoseWrongBind(log *logx.Logger, expect int) {
	if r.saidWrongBind || r.Dev == nil || !r.Dev.Running() || r.Dev.Logs == nil {
		return
	}
	other := listenPortClaimedInLogs(r.Dev.Logs.String(), expect)
	if other <= 0 {
		return
	}
	r.saidWrongBind = true
	log.Warnf("dev server appears to be listening on port %d, but the session splices into %d — "+
		"fix the framework config (Vite: server.port); the dev server must bind port %d", other, expect, expect)
}

// listenPortClaimedInLogs finds a localhost/127.0.0.1 listen URL in framework
// startup lines whose port is not expect. Empty when the logs have not claimed
// a port yet (cold start).
func listenPortClaimedInLogs(logs string, expect int) int {
	for _, line := range strings.Split(logs, "\n") {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "localhost:") && !strings.Contains(lower, "127.0.0.1:") &&
			!strings.Contains(lower, "0.0.0.0:") && !strings.Contains(lower, "listening on") {
			continue
		}
		for _, tok := range []string{"localhost:", "127.0.0.1:", "0.0.0.0:"} {
			i := strings.Index(lower, tok)
			if i < 0 {
				continue
			}
			rest := lower[i+len(tok):]
			n := 0
			for _, c := range rest {
				if c < '0' || c > '9' {
					break
				}
				n = n*10 + int(c-'0')
				if n > 65535 {
					n = 0
					break
				}
			}
			if n > 0 && n != expect {
				return n
			}
		}
	}
	return 0
}

// DevPort is where the supervised dev process binds (abuse-surface ticket 21).
// Alias of devserver.DefaultPort so callers outside that package name one constant.
const DevPort = devserver.DefaultPort

// LocalDevPort is the port the supervised dev process is listening on *on this
// machine*, which is what the session-network dev holder splices into (plan 41).
// Always the constant DevPort (ticket 09) — nothing published onto the docker
// host, and no environment override. A committed PORT= in .slopball/runtime.env
// never selects the splice target (ticket 21). workDir is kept so call sites
// stay stable.
func LocalDevPort(workDir string) (int, string) {
	_ = workDir
	return devserver.ResolveLocalPort()
}

// LocalPortListening reports whether something accepts TCP on 127.0.0.1:port.
// Used to gate endpoint publication — not a grace timer.
func LocalPortListening(port int) bool {
	if port <= 0 {
		return false
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// Reconcile applies migration / env / reseed hooks for files that changed
// between oldHEAD and newHEAD. Restarts Dev when live state changed.
func Reconcile(ctx context.Context, opt Options, oldHEAD, newHEAD string) (*Result, error) {
	res := &Result{}
	if opt.WorkDir == "" || oldHEAD == "" || newHEAD == "" || oldHEAD == newHEAD {
		return res, nil
	}
	gitRun := opt.GitRun
	if gitRun == nil {
		gitRun = sbGit.Output
	}
	changed, err := changedFiles(ctx, opt.WorkDir, oldHEAD, newHEAD, gitRun)
	if err != nil {
		return res, err
	}

	needRestart := false

	// Migration strategy: slopball only *triggers* migration; ordering and
	// idempotency are delegated to the stack's own migration tool (`prisma
	// migrate deploy`, `manage.py migrate`, a project `migrate` script, …),
	// which is designed to apply pending migrations in order and skip
	// already-applied ones. slopball's job is "a migration file changed on
	// main → run the tool once." A failure is surfaced to the dev-server log
	// (below) so the error-watcher (plan 07) can self-heal it, rather than
	// silently leaving the live DB out of sync with the schema.
	if migrationChanged(changed) {
		cmd := opt.MigrateCmd
		if len(cmd) == 0 {
			cmd = detectMigrate(opt.WorkDir)
		}
		if len(cmd) > 0 {
			if err := runCmd(ctx, opt.WorkDir, cmd); err != nil {
				logFailure(opt.Dev, "migrate", err)
				return res, fmt.Errorf("migrate: %w", err)
			}
			res.Migrated = true
			needRestart = true
		}
	}

	if contains(changed, SharedEnvPath) {
		changedEnv, err := materializeEnv(opt.WorkDir)
		if err != nil {
			return res, err
		}
		if changedEnv {
			res.EnvApplied = true
			needRestart = true
		}
	}

	if reseedRequested(ctx, opt.WorkDir, oldHEAD, newHEAD, gitRun) {
		cmd := opt.ReseedCmd
		if len(cmd) == 0 {
			cmd = detectReseed(opt.WorkDir)
		}
		if len(cmd) > 0 {
			if err := runCmd(ctx, opt.WorkDir, cmd); err != nil {
				logFailure(opt.Dev, "reseed", err)
				return res, fmt.Errorf("reseed: %w", err)
			}
			res.Reseeded = true
			needRestart = true
		}
	}

	if needRestart && opt.Dev != nil {
		if err := opt.Dev.Reload(ctx); err != nil {
			return res, err
		}
		res.Restarted = true
	}
	return res, nil
}

func changedFiles(ctx context.Context, dir, oldH, newH string, gitRun func(context.Context, string, ...string) (string, error)) ([]string, error) {
	out, err := gitRun(ctx, dir, "diff", "--name-only", oldH, newH)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

func migrationChanged(paths []string) bool {
	for _, p := range paths {
		p = filepath.ToSlash(p)
		for _, prefix := range migrationPrefixes {
			if strings.HasPrefix(p, prefix) {
				return true
			}
		}
		if strings.HasSuffix(p, ".sql") && strings.Contains(p, "migrat") {
			return true
		}
	}
	return false
}

func contains(paths []string, want string) bool {
	want = filepath.ToSlash(want)
	for _, p := range paths {
		if filepath.ToSlash(p) == want {
			return true
		}
	}
	return false
}

func reseedRequested(ctx context.Context, dir, oldH, newH string, gitRun func(context.Context, string, ...string) (string, error)) bool {
	out, err := gitRun(ctx, dir, "log", "--format=%B", oldH+".."+newH)
	if err != nil {
		return false
	}
	return strings.Contains(out, ReseedSignal)
}

// materializeEnv writes the tracked shared runtime config to the host's .env.
// Returns whether .env actually changed, so an identical shared file doesn't
// trigger a needless demo restart. Note: SharedEnvPath is the source of truth
// for demo-relevant config; secrets that must not be shared don't belong in it.
func materializeEnv(workDir string) (bool, error) {
	src := filepath.Join(workDir, SharedEnvPath)
	body, err := os.ReadFile(src)
	if err != nil {
		return false, err
	}
	dst := filepath.Join(workDir, ".env")
	if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, body) {
		return false, nil
	}
	return true, os.WriteFile(dst, body, 0o644)
}

// logFailure routes a runtime-hook failure into the dev-server log stream so the
// error-watcher role (plan 07) can pick it up — self-healing over silent loss.
func logFailure(dev *devserver.Supervisor, what string, err error) {
	if dev != nil && dev.Logs != nil {
		fmt.Fprintf(dev.Logs, "ERROR: slopball runtime %s failed: %v\n", what, err)
	}
}

func detectMigrate(workDir string) []string {
	if exists(workDir, ".slopball/migrate") {
		return []string{filepath.Join(workDir, ".slopball/migrate")}
	}
	if exists(workDir, "package.json") {
		b, _ := os.ReadFile(filepath.Join(workDir, "package.json"))
		s := string(b)
		if strings.Contains(s, `"migrate"`) {
			return []string{"npm", "run", "migrate"}
		}
		if exists(workDir, "prisma") {
			return []string{"npx", "prisma", "migrate", "deploy"}
		}
	}
	if exists(workDir, "manage.py") {
		return []string{"python", "manage.py", "migrate"}
	}
	return nil
}

func detectReseed(workDir string) []string {
	if exists(workDir, ".slopball/reseed") {
		return []string{filepath.Join(workDir, ".slopball/reseed")}
	}
	if exists(workDir, "package.json") {
		b, _ := os.ReadFile(filepath.Join(workDir, "package.json"))
		if strings.Contains(string(b), `"seed"`) {
			return []string{"npm", "run", "seed"}
		}
	}
	return nil
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func runCmd(ctx context.Context, dir string, cmd []string) error {
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %w\n%s", cmd, err, out)
	}
	return nil
}
