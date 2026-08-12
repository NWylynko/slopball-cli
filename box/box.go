// Package box provisions and drives a slopball cloud dev box: a Docker container
// on a remote (or local) machine that hosts a session's canonical, runs the
// conductor fleet, and serves the dev server. The originating machine — a
// teammate's laptop that is already in the session — reaches the box over a
// Runner (SSH in the real cross-machine case, or local when you are already on
// the box).
//
// Two provisioning lifecycles (plan 23):
//   - Default / production: docker pull a CI-published, version-tagged image
//     (ghcr.io/nwylynko/slopball-box:<cli-version>) and run it. No binary ship,
//     no on-box docker build.
//   - --build-local (dev / air-gapped): ship the linux binary + Dockerfile, build
//     the image on the box, then run — the plan-14 interim path, kept as an
//     escape hatch so iterating on slopball itself needs no CI round-trip and a
//     registry is never load-bearing for the mesh.
//
// This is the CLI making concrete what MASTERPLAN §9.2 calls pre-baking and §11
// calls the cloud-box tier; the box then behaves like any other host on the
// overlay.
package box

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/logx"
	"github.com/nwylynko/slopball-cli/telemetry"
)

//go:embed Dockerfile
var dockerfile string

var log = logx.New("box")

// buildDir is where the build context (Dockerfile + binary) is staged on the box
// for the --build-local path.
const buildDir = "/tmp/slopball-box"

// DefaultRegistry is the GHCR repository that CI publishes multi-arch box images to.
const DefaultRegistry = "ghcr.io/nwylynko/slopball-box"

// defaultWaitTimeout bounds how long Provision waits for the booted container to
// register itself as the session's host on the control plane.
const defaultWaitTimeout = 30 * time.Second

// Pull policies for Options.PullPolicy.
const (
	PullAlways  = "always"  // docker pull every provision (default)
	PullMissing = "missing" // pull only when the image is not already on the box
)

// Runner executes commands and stages files on the box's machine. SSHRunner is
// the real transport; LocalRunner is for provisioning the machine you are on
// (and for tests). Keeping it an interface is what lets the same provisioning
// logic serve both without a live SSH dependency.
type Runner interface {
	// Run executes a shell command on the target and returns combined stdout.
	Run(ctx context.Context, command string) (string, error)
	// Stream runs a long-lived command with live stdin/stdout/stderr (attach).
	Stream(ctx context.Context, command string, in io.Reader, out, errW io.Writer) error
	// Put writes data to remotePath on the target (parent dirs created).
	Put(ctx context.Context, data []byte, remotePath string) error
	// PutMode is Put with an explicit file mode — used for credentials so the
	// file is never briefly world-readable between create and chmod.
	PutMode(ctx context.Context, data []byte, remotePath string, mode os.FileMode) error
	// Target names the machine, for logs.
	Target() string
}

// Options configure a box provision.
type Options struct {
	// Image is the local docker tag used by --build-local (default "slopball-box").
	Image string
	// ImageRef is the fully-qualified image to pull+run on the default path
	// (e.g. ghcr.io/nwylynko/slopball-box:1.2.3). Ignored when BuildLocal.
	ImageRef string
	// BuildLocal selects the ship-binary + docker build path. Requires Binary.
	BuildLocal bool
	// PullPolicy controls when the pull path fetches: PullAlways (default) or
	// PullMissing (skip pull when `docker image inspect` succeeds).
	PullPolicy string
	// Binary is the slopball linux binary to ship; required when BuildLocal.
	Binary  []byte
	PIN     string
	SeedURL string // git URL to seed canonical from; "" = blank canonical
	Network string // docker --network (default "slopball" — named bridge shared with local services)
	// Labels are extra docker --label pairs (harness Sweep / make test-clean).
	Labels map[string]string
	// AdvertiseIP is the box address containers publish into the control plane.
	// Discovered from the box when empty; a container in its own netns cannot
	// work it out for itself. No host ports are published — this is for the
	// direct session-network address and DEMO endpoint, not a -p mapping.
	AdvertiseIP string
	ServeOnly   bool   // box serves canonical + tracks main, no on-box merger
	Volume      string // host path mounted at $HOME/.slopball for persistence; "" = ephemeral
	Dev         string // dev-server command the box supervises (logs served at /logs); "" = none
	// Install is the dependency-install command the box runs in canonical/work
	// before starting Dev (plan 26). "" with Dev set falls through to DetectInstall
	// inside the container.
	Install string
	// Brief is the one-line "what are we building?" answer (plan 28). It lands
	// on the box's main as .slopball/brief.md, where the setup role scaffolds
	// from it and every joined agent's contract quotes it.
	Brief string
	// MemberID / MemberSecret are the invited box member's identity (plan 44
	// ticket 05). Handed into the container via a docker --env-file (never as
	// -e argv); the secret is never logged.
	MemberID     string
	MemberSecret string
	// envFilePath is the unpredictable /tmp path Provision writes the member
	// env file to, resolved once so dockerRun names the same file.
	envFilePath string
	// Telemetry is what this box is TOLD about recording (plan 46 ticket 12).
	// A container has no human to ask, so the machine that provisions it
	// answers on its behalf: a MANAGED box always records (our container on our
	// docker host, and the one machine in a session no laptop's console fully
	// sees), while a BYO box inherits the setting of the laptop that ran
	// `box add`, because it is somebody else's hardware. Empty means say
	// nothing, and the container falls back to its own default: off.
	Telemetry string
	// Takeover tells the container to claim the PIN even though the session is
	// hosted elsewhere (plan 25). Without it a fresh box is generation 0, the
	// control plane reads that as a returning host and demotes it to a client —
	// so it never serves canonical and never starts Dev.
	Takeover  bool
	ExtraArgs []string // extra args appended to the slopball host command
	Container string   // container name (default "slopball-<pin>")

	Control    *controlplane.Client // polls endpoints; nil → NewClient(ControlURL)
	ControlURL string               // public control-plane URL used by the driving CLI
	// ContainerControlURL is an optional Docker-internal route to that same
	// control plane. Local compose uses http://slopball-control-plane:7777 while
	// laptops reach the published service through ControlURL.
	ContainerControlURL string
	// WaitTimeout bounds the wait for the box to announce itself as the new
	// host. Zero → defaultWaitTimeout.
	WaitTimeout time.Duration

	// CLIVersion is the driving CLI's Version, compared to the container's
	// `slopball --version` after boot (plan 23 drift guard).
	CLIVersion string
	// RequireVersionMatch turns a version mismatch into a hard error; default
	// is a loud warning so a stale --build-local image is visible.
	RequireVersionMatch bool
}

// devContainerPort is the port the dev server binds *inside* its own network
// namespace. Fixed on purpose: with one netns per session nothing collides, so a
// repo's committed PORT=3000 is the truth. Nothing is published onto the docker
// host — git and dev ride the session network.
const devContainerPort = 3000

// Box is a provisioned, running dev box.
type Box struct {
	Target      string
	Container   string
	Image       string
	JoinPIN     string
	GitURL      string // canonical git URL (Tailscale/LAN) clients clone/sync against
	Generation  int    // session generation the box announced at (the cutover's writer generation)
	ControlURL  string // control-plane URL clients resolve the PIN through
	Rendezvous  string // deprecated alias for ControlURL
	RawJoinInfo string // the host's full startup blurb, for display
}

// DefaultImageRef returns the published box image for a CLI version.
// Empty / *-dev / *-dirty versions fall back to :latest — a dirty local CLI has
// no matching published tag (plan 23 §1).
func DefaultImageRef(version string) string {
	tag := "latest"
	if !unpublishedVersion(version) {
		tag = strings.TrimSpace(version)
	}
	return DefaultRegistry + ":" + tag
}

// unpublishedVersion reports a CLI version CI never published an image for: an
// unset/`0.0.0-dev` build, or a `git describe --dirty` working copy.
func unpublishedVersion(version string) bool {
	v := strings.TrimSpace(version)
	return v == "" || strings.HasSuffix(v, "-dev") || strings.HasSuffix(v, "-dirty")
}

func (o *Options) defaults() {
	if o.BuildLocal {
		if o.Image == "" {
			o.Image = "slopball-box"
		}
	} else {
		if o.ImageRef == "" {
			if o.Image != "" {
				// --image without --build-local: treat as the pull ref.
				o.ImageRef = o.Image
			} else {
				o.ImageRef = DefaultImageRef(o.CLIVersion)
			}
		}
		o.PullPolicy = strings.ToLower(strings.TrimSpace(o.PullPolicy))
		if o.PullPolicy == "" {
			o.PullPolicy = PullAlways
		}
	}
	if o.Container == "" {
		o.Container = "slopball-" + o.PIN
	}
	if o.Network == "" {
		// One network namespace per session. Sharing the box's was what let two
		// sessions fight over port 3000 — and let the loser advertise a port it
		// had not managed to bind. A named bridge preserves those namespaces and
		// gives local infrastructure (notably the control plane) stable DNS.
		o.Network = "slopball"
	}
	if o.WaitTimeout <= 0 {
		o.WaitTimeout = defaultWaitTimeout
	}
}

// runImage is the docker image token used by docker run.
func (o Options) runImage() string {
	if o.BuildLocal {
		return o.Image
	}
	return o.ImageRef
}

// pullingLatestFallback reports the case where we are pulling `:latest` only
// because this CLI has no published tag. The container's version then differs
// by construction, so it is not drift — see checkVersion.
func (o Options) pullingLatestFallback() bool {
	return !o.BuildLocal && unpublishedVersion(o.CLIVersion) && o.ImageRef == DefaultImageRef(o.CLIVersion)
}

// Provision boots a box container. Default path pulls ImageRef; BuildLocal ships
// a binary and builds on the box. Idempotent on the container name: an existing
// container with the same name is replaced.
func Provision(ctx context.Context, r Runner, opt Options) (*Box, error) {
	opt.defaults()
	// Everything that can be known without touching the box is checked first, so
	// a bad invocation costs no ssh round-trip and leaves no half-provisioned box.
	if opt.PIN == "" {
		return nil, fmt.Errorf("box: PIN required")
	}
	if err := opt.validate(); err != nil {
		return nil, err
	}
	if err := preflight(ctx, r, opt.BuildLocal); err != nil {
		return nil, err
	}
	if err := ensureNetwork(ctx, r, opt.Network); err != nil {
		return nil, err
	}

	cp, err := opt.controlClient()
	if err != nil {
		return nil, err
	}

	if opt.BuildLocal {
		if err := buildLocal(ctx, r, opt); err != nil {
			return nil, err
		}
	} else {
		if err := pullImage(ctx, r, opt); err != nil {
			return nil, err
		}
	}

	// What the session looked like before this box existed. Provisioning
	// succeeds only when the box moves it — anything still at this generation
	// and URL belongs to somebody else (plan 25 defect 2).
	base := readBaseline(ctx, cp, opt.PIN)

	// A container in its own network namespace advertises what we tell it to.
	if opt.AdvertiseIP == "" {
		ip, err := AdvertiseIP(ctx, r)
		if err != nil {
			return nil, err
		}
		opt.AdvertiseIP = ip
	}

	// Egress policy after we know we will boot — not before a failed pull.
	if err := ensurePrivateEgressBlocked(ctx, r, opt.Network); err != nil {
		return nil, err
	}

	// Replace any prior container of the same name, then boot. Nothing is
	// published onto the docker host — git and dev reach peers only through
	// the session network — so there is no port-collision retry to do.
	_, _ = r.Run(ctx, fmt.Sprintf("docker rm -f %s", sh(opt.Container)))
	if opt.MemberSecret != "" || opt.MemberID != "" {
		// Secrets do not go in argv (abuse-surface ticket 07). Write a 0600
		// env file on the box and point docker at it; root reading it is fine.
		// PutMode so the file is never briefly world-readable between create
		// and a later chmod. Resolve the path ONCE — it is random per call.
		path := memberEnvFilePath(opt)
		if path == "" {
			return nil, fmt.Errorf("could not name an unpredictable env file for the box membership")
		}
		opt.envFilePath = path
		if err := r.PutMode(ctx, memberEnvFile(opt), path, 0o600); err != nil {
			return nil, fmt.Errorf("write member env file: %w", err)
		}
		defer func() { _, _ = r.Run(context.WithoutCancel(ctx), "rm "+sh(path)) }()
	}
	log.Infof("starting box container %s on %s", opt.Container, r.Target())
	if out, err := r.Run(ctx, dockerRun(opt)); err != nil {
		return nil, fmt.Errorf("docker run: %w\n%s", err, out)
	}
	log.Infof("session %s box ready on %s (no host ports published)", opt.PIN, opt.AdvertiseIP)

	b := &Box{Target: r.Target(), Container: opt.Container, Image: opt.runImage(), JoinPIN: opt.PIN}
	if err := waitForEndpoints(ctx, r, opt, cp, base, b); err != nil {
		return b, fmt.Errorf("box booted but never became the session host: %w", err)
	}
	if err := checkVersion(ctx, r, opt); err != nil {
		return b, err
	}
	log.Infof("box live: git=%s control=%s", b.GitURL, b.ControlURL)
	return b, nil
}

// validate checks the option combinations that need no access to the box.
func (o Options) validate() error {
	if o.BuildLocal {
		if len(o.Binary) == 0 {
			return fmt.Errorf("box: --build-local requires a binary (pass --binary <path>)")
		}
		return nil
	}
	switch o.PullPolicy {
	case PullAlways, PullMissing:
	default:
		return fmt.Errorf("box: unknown pull policy %q (want %s|%s)", o.PullPolicy, PullAlways, PullMissing)
	}
	if o.RequireVersionMatch && o.pullingLatestFallback() {
		return fmt.Errorf("box: --require-version-match cannot be satisfied — this CLI reports version %q, which has no published image, so it falls back to %s. "+
			"Use a released build, or pin the image with --image <ref>, or --build-local --binary <path>", o.CLIVersion, o.ImageRef)
	}
	return nil
}

func buildLocal(ctx context.Context, r Runner, opt Options) error {
	log.Infof("shipping build context to %s (%s)", r.Target(), humanBytes(len(opt.Binary)))
	if err := r.Put(ctx, opt.Binary, buildDir+"/slopball"); err != nil {
		return fmt.Errorf("ship binary: %w", err)
	}
	if err := r.Put(ctx, []byte(dockerfile), buildDir+"/Dockerfile"); err != nil {
		return fmt.Errorf("ship Dockerfile: %w", err)
	}
	log.Infof("building image %s on %s (first build pulls base + node, can take a minute)", opt.Image, r.Target())
	if out, err := r.Run(ctx, fmt.Sprintf("cd %s && docker build -f Dockerfile -t %s .", sh(buildDir), sh(opt.Image))); err != nil {
		return fmt.Errorf("docker build: %w\n%s", err, out)
	}
	return nil
}

// imagePresent reports whether the box's docker already holds ref locally.
func imagePresent(ctx context.Context, r Runner, ref string) bool {
	_, err := r.Run(ctx, fmt.Sprintf("docker image inspect %s", sh(ref)))
	return err == nil
}

func pullImage(ctx context.Context, r Runner, opt Options) error {
	ref := opt.ImageRef
	if opt.PullPolicy == PullMissing {
		if imagePresent(ctx, r, ref) {
			log.Infof("image %s already present on %s — skipping pull", ref, r.Target())
			return nil
		}
	}
	log.Infof("pulling image %s onto %s", ref, r.Target())
	out, err := r.Run(ctx, fmt.Sprintf("docker pull %s", sh(ref)))
	if err == nil {
		return nil
	}
	// A registry is not load-bearing: the image can be built straight onto the
	// box with `make box-image`, and until CI publishes a tag that is the only
	// way it exists at all. So an unreachable/denied ref that the box already
	// holds is the normal registry-less path, not a failure — warn and boot it.
	// Staleness stays visible via checkVersion, which reads the *running*
	// container's `slopball --version`.
	if imagePresent(ctx, r, ref) {
		log.Warnf("could not pull %s (%v) — booting the copy already on %s; if it is stale, rebuild it there with `make box-image`", ref, err, r.Target())
		return nil
	}
	return fmt.Errorf("docker pull %s: %w\n%s\n(hint: build the image on the box with `make box-image`; or `docker login` there for a private registry; or use --build-local --binary …)", ref, err, out)
}

// checkVersion compares the container's slopball --version to the driving CLI —
// the guard for the original footgun, a box quietly running an older slopball
// than the CLI driving it (that is how a box ended up not knowing `--dev`).
func checkVersion(ctx context.Context, r Runner, opt Options) error {
	if opt.CLIVersion == "" {
		return nil
	}
	out, err := Exec(ctx, r, opt.Container, "--version")
	if err != nil {
		// A container too old to answer --version is itself a staleness signal.
		if opt.RequireVersionMatch {
			return fmt.Errorf("box: --require-version-match: could not read the container's `slopball --version` (image too old?): %w", err)
		}
		log.Warnf("could not read container slopball --version: %v", err)
		return nil
	}
	got := parseVersionOutput(out)
	if versionsMatch(got, opt.CLIVersion) {
		return nil
	}
	if opt.pullingLatestFallback() {
		// We asked for :latest precisely because this CLI has no published tag,
		// so a different version is expected — warning here would be crying wolf
		// on every dev run. (--require-version-match is refused up front.)
		log.Infof("box runs slopball %s (from %s); this CLI is an unreleased build (%s) — versions are not comparable", got, opt.ImageRef, opt.CLIVersion)
		return nil
	}
	msg := fmt.Sprintf("box: container runs slopball %s but this CLI is %s — rebuild/pull to match", got, opt.CLIVersion)
	if opt.RequireVersionMatch {
		return fmt.Errorf("%s (container %s is up on %s; `slopball box rm` to drop it)", msg, opt.Container, r.Target())
	}
	log.Warnf("%s", msg)
	return nil
}

func parseVersionOutput(out string) string {
	// cobra default: "slopball version <X>" — take the last field of the first line.
	line := strings.TrimSpace(strings.Split(out, "\n")[0])
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return strings.TrimSpace(out)
	}
	return fields[len(fields)-1]
}

// versionsMatch compares after stripping the cosmetic differences between how a
// tag and a build stamp spell the same release (`v1.2.3` vs `1.2.3`, a `-dirty`
// working copy of the same commit). Everything else is a real difference —
// notably 1.2.3 must not be satisfied by a box running 1.2.30.
func versionsMatch(got, want string) bool {
	return normalizeVersion(got) == normalizeVersion(want)
}

func normalizeVersion(v string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(v), "v"), "-dirty")
}

// dockerRun builds the `docker run` command that boots the host inside the box.
func dockerRun(opt Options) string {
	var b strings.Builder
	b.WriteString("docker run -d")
	// slopball is PID 1 in this container, which makes it what the kernel
	// re-parents orphans to — and Go reaps only children it Wait()s on, never an
	// adopted one. git orphans a process per fetch (upload-pack outliving its
	// parent, or the reverse), so the host accumulated one zombie per 2s tick
	// until it hit the cgroup pids ceiling and could not fork at all. --init runs
	// tini as PID 1 to reap them; slopball has no business being an init system.
	b.WriteString(" --init")
	fmt.Fprintf(&b, " --name %s", sh(opt.Container))
	fmt.Fprintf(&b, " --network %s", sh(opt.Network))
	for k, v := range opt.Labels {
		fmt.Fprintf(&b, " --label %s", sh(k+"="+v))
	}
	appendContainment(&b, opt)
	// No -p. The plain git listener is loopback-only and the raw dev server
	// must not sit on 0.0.0.0; peers reach both through the session network.
	// What to publish into the control plane — and the only bind input. With
	// this set, ListenAdvertise binds 0.0.0.0 and publishes the address; a
	// separate bind-mode env is gone (ticket 06).
	if opt.AdvertiseIP != "" {
		fmt.Fprintf(&b, " -e SLOPBALL_ADVERTISE=%s", sh(opt.AdvertiseIP))
	}
	// Tell the dev server which port to bind rather than letting it pick and
	// then silently fall back to another one.
	fmt.Fprintf(&b, " -e PORT=%d", devContainerPort)
	// Whether this box records. Not a secret and not operator configuration —
	// it is the provisioner telling the container what its owner decided.
	if opt.Telemetry != "" {
		fmt.Fprintf(&b, " -e %s=%s", telemetry.EnvMode, sh(opt.Telemetry))
	}
	if u := opt.containerControlURL(); u != "" {
		fmt.Fprintf(&b, " -e SLOPBALL_CONTROL=%s", sh(u))
	}
	if opt.envFilePath != "" {
		// Invited identity (plan 44) — via --env-file, never -e KEY=VALUE.
		// -e lands in argv (ps, shell history, docker inspect recreate).
		// Provision resolved this path when it wrote the file; dockerRun never
		// mints one, or it would name a file nobody wrote.
		fmt.Fprintf(&b, " --env-file %s", sh(opt.envFilePath))
	}
	b.WriteString(" " + sh(opt.runImage()))

	// slopball host args (ENTRYPOINT is `slopball`). A PIN is only legal on the
	// internal boot verb — plain `slopball` creates and tells you the PIN.
	fmt.Fprintf(&b, " _host --pin %s", sh(opt.PIN))
	if opt.SeedURL != "" {
		fmt.Fprintf(&b, " --seed-url %s", sh(opt.SeedURL))
	}
	if opt.ServeOnly {
		b.WriteString(" --serve-only")
	}
	if opt.Dev != "" {
		fmt.Fprintf(&b, " --dev %s", sh(opt.Dev))
	}
	if opt.Install != "" {
		fmt.Fprintf(&b, " --install %s", sh(opt.Install))
	}
	if opt.Brief != "" {
		fmt.Fprintf(&b, " --brief %s", sh(opt.Brief))
	}
	if opt.Takeover {
		b.WriteString(" --takeover")
	}
	for _, a := range opt.ExtraArgs {
		b.WriteString(" " + sh(a))
	}
	return b.String()
}

// memberEnvFilePath MINTS a path for the invited membership. Provision calls it
// once and stores the answer on opt.envFilePath, which is what dockerRun names —
// two calls never agree, and that is the point.
//
// The name carries 128 bits of randomness and NOT the PIN. `Runner.PutMode`
// writes with `cat > <path>`, which follows a symlink, and the PIN is public by
// design — a predictable name on a shared box lets any local user pre-create it
// pointing at a file the ssh user owns and have provisioning truncate and write
// that instead. Each call returns a fresh path, so Provision resolves it once
// and hands the same string to dockerRun.
func memberEnvFilePath(opt Options) string {
	if opt.MemberSecret == "" && opt.MemberID == "" {
		return ""
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A path we cannot make unpredictable is not one to fall back from:
		// the caller surfaces this instead of writing a guessable name.
		return ""
	}
	return "/tmp/slopball-env-" + hex.EncodeToString(b[:]) + ".env"
}

// memberEnvFile is the docker --env-file body for the invited box member.
func memberEnvFile(opt Options) []byte {
	var b strings.Builder
	if opt.MemberID != "" {
		fmt.Fprintf(&b, "SLOPBALL_MEMBER_ID=%s\n", opt.MemberID)
	}
	if opt.MemberSecret != "" {
		fmt.Fprintf(&b, "SLOPBALL_MEMBER_SECRET=%s\n", opt.MemberSecret)
	}
	return []byte(b.String())
}

// Exec runs a slopball verb inside the box and returns its output (one-shot
// commands like sync/monitor).
func Exec(ctx context.Context, r Runner, container string, args ...string) (string, error) {
	return r.Run(ctx, execCmd(container, false, args))
}

// ExecAttached runs a slopball verb inside the box with live I/O — the default
// for `box run`, so npm/dev-server logs stream back to the laptop until Ctrl-C.
func ExecAttached(ctx context.Context, r Runner, container string, in io.Reader, out, errW io.Writer, args ...string) error {
	return r.Stream(ctx, execCmd(container, false, args), in, out, errW)
}

// ExecDetached starts a long-running slopball verb inside the box with
// `docker exec -d` and returns once it is launched (no log stream).
func ExecDetached(ctx context.Context, r Runner, container string, args ...string) error {
	_, err := r.Run(ctx, execCmd(container, true, args))
	return err
}

func execCmd(container string, detach bool, args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = sh(a)
	}
	flag := ""
	if detach {
		flag = "-d "
	}
	return fmt.Sprintf("docker exec %s%s slopball %s", flag, sh(container), strings.Join(quoted, " "))
}

// Logs returns the box container's logs.
func Logs(ctx context.Context, r Runner, container string) (string, error) {
	return r.Run(ctx, fmt.Sprintf("docker logs %s", sh(container)))
}

// Remove force-removes the box container.
func Remove(ctx context.Context, r Runner, container string) error {
	_, err := r.Run(ctx, fmt.Sprintf("docker rm -f %s", sh(container)))
	return err
}

func preflight(ctx context.Context, r Runner, buildLocal bool) error {
	if _, err := r.Run(ctx, "docker version --format '{{.Server.Version}}'"); err != nil {
		return fmt.Errorf("docker not usable on %s (is it installed and running?): %w", r.Target(), err)
	}
	arch, err := r.Run(ctx, "uname -m")
	if err != nil {
		return fmt.Errorf("probe arch on %s: %w", r.Target(), err)
	}
	a := strings.TrimSpace(arch)
	if buildLocal {
		// The shipped binary is linux/amd64 only.
		if a != "x86_64" && a != "amd64" {
			return fmt.Errorf("box arch is %q; --build-local ships a linux/amd64 binary — use the pull path (default) on arm64, or cross-build an arm64 binary", a)
		}
		return nil
	}
	// The pull path is multi-arch, but only for the arches CI publishes: let
	// docker pick the manifest, and fail readably on anything else rather than
	// leaving the user with "no matching manifest" from docker pull.
	switch a {
	case "x86_64", "amd64", "aarch64", "arm64":
		return nil
	default:
		return fmt.Errorf("box arch is %q; the published box image is multi-arch linux/amd64+arm64 only — build your own image for this arch and pass --image <ref>", a)
	}
}

func ensureNetwork(ctx context.Context, r Runner, network string) error {
	switch network {
	case "", "bridge", "host", "none":
		return nil
	}
	inspect := fmt.Sprintf("docker network inspect %s", sh(network))
	if _, err := r.Run(ctx, inspect); err == nil {
		return nil
	}
	out, err := r.Run(ctx, fmt.Sprintf("docker network create %s", sh(network)))
	if err == nil {
		return nil
	}
	// Two simultaneous provisions may both miss the first inspect and race to
	// create the network. If it exists now, the loser also succeeded.
	if _, inspectErr := r.Run(ctx, inspect); inspectErr == nil {
		return nil
	}
	return fmt.Errorf("create docker network %s on %s: %w\n%s", network, r.Target(), err, out)
}

// baseline is the session's addressing state before the box booted.
type baseline struct {
	Generation int
	GitURL     string
}

// readBaseline snapshots the session's generation + git endpoint. A PIN nobody
// has claimed yet reads as {0, ""}, which every real announcement beats.
func readBaseline(ctx context.Context, cp *controlplane.Client, pin string) baseline {
	sess, err := cp.Session(ctx, pin)
	if err != nil {
		return baseline{}
	}
	// raw endpoint ok: a baseline to compare later announcements against.
	return baseline{Generation: sess.Generation, GitURL: strings.TrimSpace(sess.Endpoints[controlplane.EndpointGit].URL)}
}

// waitForEndpoints polls the control plane until the box has taken the host role
// and announced *its own* canonical: a generation past the baseline and a git URL
// that is not the one already parked there. Accepting whatever is parked is how a
// provision that never hosted reported the operator's laptop as the box.
//
// The generation is the authority; `docker logs` is scraped only for a faster,
// better-worded failure (plan 24 keeps inferred endpoints non-load-bearing).
func waitForEndpoints(ctx context.Context, r Runner, opt Options, cp *controlplane.Client, base baseline, b *Box) error {
	deadline := time.Now().Add(opt.WaitTimeout)
	lastGen, lastGit := base.Generation, base.GitURL
	for {
		logsOut, _ := Logs(ctx, r, opt.Container)
		if strings.Contains(strings.ToLower(logsOut), "error:") {
			return fmt.Errorf("container reported an error:\n%s", logsOut)
		}
		if strings.Contains(logsOut, "joining as a client") {
			return fmt.Errorf("the container joined %s as a client instead of hosting it — "+
				"it never served canonical, so no dev server ran there. `box add` claims the host role with --takeover; "+
				"if you passed --no-cutover, that is expected.\ncontainer logs:\n%s", opt.PIN, logsOut)
		}
		sess, err := cp.Session(ctx, opt.PIN)
		if err == nil {
			lastGen = sess.Generation
			// raw endpoint ok: compared against the baseline above, and handed
			// on as the box's own published address.
			lastGit = strings.TrimSpace(sess.Endpoints[controlplane.EndpointGit].URL)
			if announcedByTheBox(opt, base, lastGen, lastGit) {
				b.GitURL = lastGit
				b.Generation = lastGen
				b.ControlURL = cp.Base
				b.Rendezvous = cp.Base
				b.RawJoinInfo = logsOut
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s: session %s is at generation %d with git endpoint %q "+
				"(was generation %d / %q before the box booted) — the box never announced its own canonical.\ncontainer logs:\n%s",
				opt.WaitTimeout, opt.PIN, lastGen, orNone(lastGit), base.Generation, orNone(base.GitURL), logsOut)
		}
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

// announcedByTheBox decides whether the session's git endpoint is the container
// we just booted rather than whatever was already parked there.
//
// The git server binds an ephemeral port, so a fresh container's URL never
// matches the previous one — a changed URL is the signal that *something new*
// announced, and only the box can have. On the takeover path the generation must
// also have advanced, which is the authoritative half (plan 24: scraped docker
// logs must not be load-bearing). A box resuming its own canonical from a
// --volume claims at the same generation, so requiring a bump there would
// reject a legitimate re-provision.
//
// Deliberately not compared: host_machine against the ssh target. With
// --network host the container keeps its own UTS namespace, so os.Hostname()
// inside it is the container id, not the box's hostname.
func announcedByTheBox(opt Options, base baseline, gen int, git string) bool {
	if git == "" || git == base.GitURL || gen < base.Generation {
		return false
	}
	if opt.Takeover {
		return gen > base.Generation
	}
	return true
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// controlClient resolves the control plane this provision talks to.
func (o Options) controlClient() (*controlplane.Client, error) {
	if o.Control != nil {
		return o.Control, nil
	}
	if u := o.controlURL(); u != "" {
		return controlplane.NewClient(u), nil
	}
	return nil, fmt.Errorf("box: Control client or SLOPBALL_CONTROL required")
}

func (o Options) controlURL() string {
	if u := strings.TrimSpace(o.ControlURL); u != "" {
		return strings.TrimRight(u, "/")
	}
	return strings.TrimRight(strings.TrimSpace(os.Getenv("SLOPBALL_CONTROL")), "/")
}

func (o Options) containerControlURL() string {
	if u := strings.TrimSpace(o.ContainerControlURL); u != "" {
		return strings.TrimRight(u, "/")
	}
	return o.controlURL()
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// sh single-quotes an argument for safe interpolation into a /bin/sh command
// line (the Runner sends one shell string to the target).
func sh(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
