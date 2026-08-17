// The wire types live HERE, in the client half, and the store imports them
// rather than the other way round. That inversion is the whole of plan 39: the
// CLI a teammate installs needs Session, Endpoint and friends, and before this
// it reached them through an alias into `store` — which imports pgx, so every
// laptop linked a postgres driver to read a JSON struct.
package controlplane

import (
	"errors"
	"time"
)

// Endpoint kinds announced into the control plane.
const (
	EndpointGit    = "git"
	EndpointLogs   = "logs"
	EndpointDev    = "dev"
	EndpointDemo   = "demo"
	EndpointPublic = "public"
	EndpointMirror = "mirror"
)

// Member roles.
const (
	RoleHost   = "host"
	RoleClient = "client"
	RoleBox    = "box"
)

// Session statuses (advisory).
const (
	StatusLive     = "live"
	StatusDegraded = "degraded"
	StatusEnded    = "ended"
)

// Session access state (plan 44 ticket 07). Open/closed are set; full is derived.
const (
	AccessOpen   = "open"
	AccessClosed = "closed"
	AccessFull   = "full"
)

// RosterLimit is the lifetime member cap (counting the box). Left members keep
// their slot; there is no evict.
const RosterLimit = 10

// MemberHeader carries the caller's member id alongside `Authorization: Bearer
// <secret>`. It is what makes authentication one indexed row fetch and one hash
// verification instead of a scan of the roster — and, on a miss, a scan of every
// other session's members to tell 403 from 401.
//
// It rides its own header rather than being folded into the Bearer value so the
// Authorization header stays the conventional shape. The id is not a secret: it
// already appears in request paths and in the session document, so a proxy log
// keeping it costs nothing. The secret never goes anywhere but this Bearer.
const MemberHeader = "X-Slopball-Member"

// Endpoint is one announced address for a session.
type Endpoint struct {
	URL       string     `json:"url"`
	Host      string     `json:"host,omitempty"`
	Port      int        `json:"port,omitempty"`
	Source    string     `json:"source,omitempty"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
	// Direct is the machine address of whoever currently holds this service,
	// for clients that can reach it (plan 38 step 4). URL stays the answer —
	// it names the session role and survives the lease migrating — and this is
	// a shortcut a client tries first and silently abandons when it does not
	// answer. Empty when the holder publishes no direct address, which is the
	// normal case on separate mobile hotspots.
	Direct string `json:"direct,omitempty"`
}

// MemberCapability is the subset of detect.Profile published for ranking.
type MemberCapability struct {
	Runtime string `json:"runtime,omitempty"`
	Version string `json:"version,omitempty"`
	PkgMgr  string `json:"pkgMgr,omitempty"`
	Score   int    `json:"score,omitempty"`
}

// Member is a presence record.
type Member struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Role          string `json:"role"`
	Branch        string `json:"branch,omitempty"`
	Machine       string `json:"machine,omitempty"`
	HeadSHA       string `json:"headSha,omitempty"`
	MainMirrorSHA string `json:"mainMirrorSha,omitempty"`
	// MainMirrorHeight is the commit count on this member's mirror of main —
	// how *far along* it is, which is what makes "the freshest replica wins the
	// git lease" an ordering rather than a guess (plan 30).
	MainMirrorHeight int               `json:"mainMirrorHeight,omitempty"`
	Harness          string            `json:"harness,omitempty"`
	Online           bool              `json:"online"`
	LastSeenAt       time.Time         `json:"lastSeenAt"`
	JoinedAt         time.Time         `json:"joinedAt,omitempty"`
	Capability       *MemberCapability `json:"capability,omitempty"`
	// State is pending | admitted | left (plan 44). Blank on older rows means
	// admitted — every member that existed before admission was real.
	State string `json:"state,omitempty"`
	// Version is the build this member's slopball is running, as it presented
	// itself in VersionHeader on its last knock or member cycle (plan 48). Blank
	// means a client that predates the header, or one that has not talked to this
	// control plane since it started asking — never "current". It is what makes
	// the drain check answerable: active sessions are what a deploy can strand,
	// and this column is the only place the control plane learns who is in one.
	Version string `json:"version,omitempty"`
	// Secret is the raw member secret, returned exactly once at mint time and
	// never stored or re-read. Callers must persist it; the server only keeps a hash.
	Secret string `json:"secret,omitempty"`
}

// Member states (plan 44). Pending is a knock; admitted is in; left keeps the
// membership (and the secret) so coming back costs nobody a decision.
const (
	MemberPending  = "pending"
	MemberAdmitted = "admitted"
	MemberLeft     = "left"
)

// Role states (plan 36 §2). Anything else reads as unknown.
const (
	RoleIdle    = "idle"
	RoleWorking = "working"
)

// RoleStaleAfter is how long a role's published state stays believable. It is
// generous against the conductor's 2s publish cadence for the same reason
// DefaultLeaseTTL is: a network blip must not blank the dashboard. What it
// really guards is the elector who shuts their laptop mid-merge — without it
// every other member's dashboard reads `merger ● working` for the rest of the
// session.
const RoleStaleAfter = 15 * time.Second

// RoleAgent is one fleet role's chosen intelligence, plus what it is doing
// right now. A blank model means the CLI's own default.
//
// State/Activity/Since/UpdatedAt are the dashboard's half (plan 36 §2). They
// are *sampled*, and that is accepted: a clean mechanical merge starts and
// finishes inside one publish tick, so it usually reads idle while fast work
// lands. The dot is a slow-work indicator — it lights up for the AI conflict
// resolves and the error-watcher fixes, which are the ones worth watching — and
// the event feed is the durable record of everything else.
type RoleAgent struct {
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`
	// Mechanical is true when the session chose a harness for this role but this
	// machine runs it mechanically because the CLI is not installed here.
	Mechanical bool `json:"mechanical,omitempty"`
	// State is idle | working. Blank means nothing has published yet.
	State string `json:"state,omitempty"`
	// Activity is one line of what it is doing, shown beside the dot.
	Activity string `json:"activity,omitempty"`
	// Since is when this state was entered — "working for 45s".
	Since time.Time `json:"since,omitzero"`
	// UpdatedAt is when the elector last published, which is what staleness is
	// measured against. Since cannot do that job: a genuinely long AI resolve
	// would age out of a state it is still legitimately in.
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
}

// Stale reports whether this role's state is too old to believe, the way member
// Online / LastSeenAt already works.
func (r RoleAgent) Stale(now time.Time) bool {
	return r.UpdatedAt.IsZero() || now.Sub(r.UpdatedAt) > RoleStaleAfter
}

// Working reports whether the role is doing slow work *and* said so recently.
func (r RoleAgent) Working(now time.Time) bool {
	return r.State == RoleWorking && !r.Stale(now)
}

// ConductorRecord is the discovery-facing election (never credentials).
//
// Roles carries the session's per-role fleet composition (plan 29), keyed
// "merger" / "error-watcher" / "setup". It lives here rather than on the
// asking laptop so a machine that elects itself conductor mid-session — or a
// migrated host — reproduces the agents the session actually chose. Elector /
// Harness / Model stay as the elector's primary, for the monitor's one-line
// view; the field is additive, so an older record reads back unchanged.
type ConductorRecord struct {
	Elector   string               `json:"elector,omitempty"`
	Harness   string               `json:"harness,omitempty"`
	Model     string               `json:"model,omitempty"`
	RunnerID  string               `json:"runnerId,omitempty"`
	Roles     map[string]RoleAgent `json:"roles,omitempty"`
	Active    bool                 `json:"active"`
	ElectedAt *time.Time           `json:"electedAt,omitempty"`
}

// Convergence is an advisory mirror of git convergence state.
type Convergence struct {
	MainSHA       string   `json:"mainSha,omitempty"`
	BranchesAhead []string `json:"branchesAhead,omitempty"`
	// BranchesAheadOmitted is how many names the publisher cut past
	// MaxBranchesAhead — so a console can tell truncation from a real list of 32.
	BranchesAheadOmitted int        `json:"branchesAheadOmitted,omitempty"`
	LastMergeAt          *time.Time `json:"lastMergeAt,omitempty"`
	LastMergeBranch      string     `json:"lastMergeBranch,omitempty"`
	Watcher              string     `json:"watcher,omitempty"`
	LastError            *string    `json:"lastError"`
	UpdatedAt            *time.Time `json:"updatedAt,omitempty"`
}

// Box states (plan 37). A BYO box is written straight to BoxReady — it was
// already running before anyone told the control plane about it.
const (
	BoxRequested    = "requested"
	BoxProvisioning = "provisioning"
	BoxReady        = "ready"
	BoxFailed       = "failed"
)

// Box providers.
const (
	BoxProviderBYO    = "byo"    // `slopball box add <ssh-target>` — someone else's machine
	BoxProviderDocker = "docker" // the control plane's own machine
	// BoxProviderCloudflare is a Cloudflare Container, spawned by the box
	// worker because Container instances can only be created from Worker code.
	BoxProviderCloudflare = "cloudflare"
)

// BoxFacts is a session's box: what it is, who made it, and — since plan 37 —
// how far along it is. One record covers both tiers, which is what keeps the
// monitor, the console and slopdebug from having to know which one they are
// looking at.
type BoxFacts struct {
	Target    string `json:"target,omitempty"`
	Container string `json:"container,omitempty"`
	Image     string `json:"image,omitempty"`
	Version   string `json:"version,omitempty"`
	// State is requested | provisioning | ready | failed.
	State    string `json:"state,omitempty"`
	Provider string `json:"provider,omitempty"`
	GitURL   string `json:"gitUrl,omitempty"`
	DevURL   string `json:"devUrl,omitempty"`
	LogsURL  string `json:"logsUrl,omitempty"`
	// Error is the provisioner's own message on a failed provision, including
	// the container log tail. Losing it would turn a diagnosable failure into
	// "it timed out" on a machine the human cannot see.
	Error       string     `json:"error,omitempty"`
	RequestedAt *time.Time `json:"requestedAt,omitempty"`
	ReadyAt     *time.Time `json:"readyAt,omitempty"`
	UpdatedAt   *time.Time `json:"updatedAt,omitempty"`
}

// Pending reports whether a box request is still in flight.
func (b BoxFacts) Pending() bool {
	return b.State == BoxRequested || b.State == BoxProvisioning
}

// BoxBootFailure is what a managed box POSTs to `…/box/boot-failed` when it
// cannot come up at all: the one sentence saying why, sent under its own box
// membership, before the process stops.
//
// It exists because a box that dies on boot used to be indistinguishable from
// one still starting. Session 2lmymb's replacement box exited five seconds in
// and the record said `provisioning` with an empty error for the provisioner's
// whole eight-minute deadline, at which point the session was told the generic
// "the box never claimed" — the reason having been written only to a container
// stdout nobody in this repo can read.
//
// Deliberately NOT a member.left: leaving is what a healthy box does when the
// platform stops it, and the control plane answers that by starting the box
// again. A boot failure that reported itself as a leave would restart the
// container into the same failure until the auto-restart cap ran out.
type BoxBootFailure struct {
	Reason string `json:"reason"`
}

// BoxRequest is what a client asks the control plane to provision. It carries
// only what the box cannot work out for itself — no ssh target, no pull policy,
// no binary. Those are the control plane's business now.
type BoxRequest struct {
	PIN     string `json:"pin,omitempty"`
	Brief   string `json:"brief,omitempty"`
	SeedURL string `json:"seedUrl,omitempty"`
	Image   string `json:"image,omitempty"`
	// Replace re-provisions a session that already has a box, instead of
	// returning the existing record.
	Replace bool `json:"replace,omitempty"`
	// MemberID and MemberSecret are set by the control plane when it mints the
	// box member before booting (plan 44 ticket 05). Never accepted from the
	// wire — the operator already holds every session key, and this is that
	// same trust boundary written down rather than discovered. Both are needed:
	// the box authenticates with the pair, not the secret alone.
	MemberID     string `json:"-"`
	MemberSecret string `json:"-"`
}

// StackInfo is the session's detected default stack.
type StackInfo struct {
	Runtime string `json:"runtime,omitempty"`
	Version string `json:"version,omitempty"`
	PkgMgr  string `json:"pkgMgr,omitempty"`
}

// The three leased services (plan 30). They have three different portability
// stories — code moves in seconds, runtime does not, and a harness login moves
// nowhere at all (§4.3, §10) — which is exactly why they are leased separately
// rather than bundled into "the host".
const (
	ServiceGit       = "git"       // canonical + the session git server
	ServiceDev       = "dev"       // the supervised dev server
	ServiceConductor = "conductor" // the merger / error-watcher / setup fleet
)

// Services is every leased service, in display order.
var Services = []string{ServiceGit, ServiceDev, ServiceConductor}

// Lease is one service's current placement. A lease is only claimable when it
// has expired or been handed over — never taken from a live owner, which is
// what preserves plan 25's guarantee that a laptop cannot steal hosting from a
// working box.
type Lease struct {
	Service    string    `json:"service"`
	Owner      string    `json:"owner,omitempty"` // member id
	OwnerName  string    `json:"ownerName,omitempty"`
	Machine    string    `json:"machine,omitempty"`
	Generation int       `json:"generation"`
	ExpiresAt  time.Time `json:"expiresAt"`
	UpdatedAt  time.Time `json:"updatedAt,omitempty"`
	// StartFailure is the last member that held this lease and could not start
	// the service. It rides the LEASE rather than convergence because all three
	// services fail this way — dev has flapped on "this project declares no dev
	// command" since plan 30 — and because the lease is the one row that is
	// already per-service and already read by everything that renders placement.
	StartFailure *StartFailure `json:"startFailure,omitempty"`
}

// StartFailure is one visible, durable fact: a member took a service's lease,
// could not start the service, and handed the lease back. Session 2lmymb spent
// eleven minutes with the conductor lease flapping every five seconds and this
// fact existing nowhere but one laptop's stdout, so the machine and the reason
// travel together and are read straight out of the session document.
type StartFailure struct {
	MemberID string    `json:"memberId,omitempty"`
	Name     string    `json:"name,omitempty"`
	Machine  string    `json:"machine,omitempty"`
	Reason   string    `json:"reason"`
	At       time.Time `json:"at,omitzero"`
}

// Line is the one sentence every surface shows — console, monitor, log.
func (f StartFailure) Line(service string) string {
	who := f.Name
	if who == "" {
		who = f.MemberID
	}
	if f.Machine != "" {
		who += "@" + f.Machine
	}
	return service + ": " + who + " can't start it — " + f.Reason
}

// Live reports whether the lease is still held. An unowned or expired lease is
// claimable; a live one is not.
func (l Lease) Live(now time.Time) bool {
	return l.Owner != "" && now.Before(l.ExpiresAt)
}

// LeaseRequest claims, renews or hands over one service.
type LeaseRequest struct {
	Service    string `json:"service"`
	MemberID   string `json:"memberId"`
	Name       string `json:"name,omitempty"`
	Machine    string `json:"machine,omitempty"`
	TTLSeconds int    `json:"ttlSeconds,omitempty"`
	// To hands the lease to a specific member (graceful transfer).
	To string `json:"to,omitempty"`
	// StartFailReason is why the member could not start the service it just
	// took, sent with the `start-failed` release. Only that action reads it.
	StartFailReason string `json:"startFailReason,omitempty"`
	// Force takes a LIVE lease from its owner. The escape hatch for a wedged
	// owner and nothing else — automatic placement never sets it.
	Force bool `json:"force,omitempty"`
}

// DefaultLeaseTTL is generous against the 2s heartbeat on purpose: a network
// blip must not move a service. This is §4.2's false-positive guard, relocated
// from "ask a human" to "the arbiter says the lease is still live".
const DefaultLeaseTTL = 30

// Session is the full snapshot returned by GET /v1/sessions/{pin}.
type Session struct {
	PIN string `json:"pin"`
	// UID is the session's surrogate id, minted once at create (plan 46
	// ticket 01). PINs are six characters and get reused, so a telemetry query
	// keyed on one mixes unrelated sessions; the uid is what makes a session
	// identifiable forever. The PIN remains the primary key and expired
	// sessions are still swept — the control plane is live state, and the
	// historical record lives in the telemetry database.
	UID         string              `json:"uid,omitempty"`
	Generation  int                 `json:"generation"`
	Status      string              `json:"status"`
	OverlayAddr string              `json:"overlayAddr,omitempty"`
	HostMachine string              `json:"hostMachine,omitempty"`
	Endpoints   map[string]Endpoint `json:"endpoints"`
	Members     []Member            `json:"members"`
	Conductor   *ConductorRecord    `json:"conductor,omitempty"`
	Convergence *Convergence        `json:"convergence,omitempty"`
	Box         *BoxFacts           `json:"box,omitempty"`
	Leases      map[string]Lease    `json:"leases,omitempty"`
	Stack       *StackInfo          `json:"stack,omitempty"`
	Harness     string              `json:"harness,omitempty"`
	SeedURL     string              `json:"seedUrl,omitempty"`
	// SessionKey is the session network's shared secret (plan 09), minted with
	// the session and handed to whoever resolves the PIN. It is what turns
	// §16 #7's "the PIN is the whole auth model" into an access control rather
	// than a lookup — before it, reaching a git port was authorization.
	//
	// Yes, this means the control plane hands a secret to anyone who knows a
	// 6-character PIN. That is §5.6's stated model: the guard is join-attempt
	// throttling and a session lifetime measured in tens of minutes, not PIN
	// entropy.
	SessionKey string `json:"sessionKey,omitempty"`
	// RelayAddr is where the session network's relay listens. It is served
	// from the control plane's own configuration rather than stored per
	// session, because members CACHE it: once a client holds it, a
	// control-plane outage costs new joins and lease changes, never merging.
	RelayAddr string `json:"relayAddr,omitempty"`
	// TelemetryURL is where an opted-in member POSTs its own telemetry (plan 46
	// ticket 11). Served the same way and for the same reason as RelayAddr: it
	// names a deployment, so a laptop is TOLD rather than configured, and it
	// arrives in the same breath as the relay address and the tickets a member
	// already fetches. Empty means this deployment advertises no ingest, which
	// an opted-in client reports once and then records nothing.
	TelemetryURL string `json:"telemetryUrl,omitempty"`
	// Access is open | closed | full (plan 44 ticket 07). Full is derived from
	// the lifetime roster and is never written by a client.
	Access    string    `json:"access,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Event kinds the session publishes for itself, rather than the server deriving
// them from a state write (plan 36 §2). Work *arriving* and work *landing* are
// both worth seeing, and the gap between them is the interesting number.
//
// The allow-list is the point: the control plane is the addressing spine and is
// deliberately never on the merge hot path, so it must not become a log bus.
// Structured facts travel; text stays where it is produced.
const (
	EventSyncPushed   = "sync.pushed"
	EventMergeApplied = "merge.applied"
)

// EventPlacementFailed is server-derived, like role.working: the store appends
// it when a member reports that it took a service's lease and could not start
// the service. It is deliberately NOT in PublishableEvents — a member does not
// post it, it reports the failure and the store decides what the session's feed
// says about it.
const EventPlacementFailed = "placement.failed"

// The managed box's lifecycle on the session feed. All three are
// server-derived — a member posting "the session's box failed" could declare
// somebody else's serving box dead — and together they are the story session
// 2lmymb could not tell: the control plane asked for a box (`restart: true`
// when it is starting one that WAS running again), the provisioner could not
// produce it (`reason`, the same words that land in `boxes.error`), or the box
// itself came up and could not boot (`reason`, reported by the box on its way
// out via `POST …/box/boot-failed`).
const (
	EventBoxRequested  = "box.requested"
	EventBoxFailed     = "box.failed"
	EventBoxBootFailed = "box.boot-failed"
)

// PublishableEvents is every kind a session member may append directly.
var PublishableEvents = map[string]bool{
	EventSyncPushed:   true,
	EventMergeApplied: true,
}

// EventPost is POST /v1/sessions/{pin}/events.
type EventPost struct {
	Kind    string         `json:"kind"`
	Payload map[string]any `json:"payload,omitempty"`
}

// Event is one broadcast item, server-derived or member-published.
type Event struct {
	Seq       int64          `json:"seq"`
	Kind      string         `json:"kind"`
	Payload   map[string]any `json:"payload,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
}

// ClaimRequest is POST /v1/sessions.
type ClaimRequest struct {
	PIN         string     `json:"pin"`
	OverlayAddr string     `json:"overlayAddr,omitempty"`
	HostMachine string     `json:"hostMachine,omitempty"`
	Stack       *StackInfo `json:"stack,omitempty"`
	Harness     string     `json:"harness,omitempty"`
	SeedURL     string     `json:"seedUrl,omitempty"`
	// LocalGeneration is what this machine last knew. 0 = never hosted.
	LocalGeneration int `json:"localGeneration,omitempty"`
	// Takeover asks to become the host even though this machine's generation is
	// behind — the one authorized promotion (plan 25). A fresh cloud box has
	// generation 0 by construction, so without this it is indistinguishable from
	// an old laptop rebooting and gets demoted to a client. Set only by
	// `slopball box add` (via the hidden `--takeover`), never by a plain host
	// start, so the plan-24 rule "a departed host never reclaims the role"
	// stays the default.
	Takeover bool `json:"takeover,omitempty"`
	// MemberName / MemberRole describe the creator minted with a fresh claim
	// (plan 44). A --serve-only box is role=box name=box; a laptop is host/$USER.
	// Blank falls back to HostMachine / host.
	MemberName string `json:"memberName,omitempty"`
	MemberRole string `json:"memberRole,omitempty"`
}

// ClaimResponse is returned by POST /v1/sessions.
type ClaimResponse struct {
	Session  Session `json:"session"`
	Created  bool    `json:"created"`
	Resumed  bool    `json:"resumed"`
	JoinOnly bool    `json:"joinOnly"` // generation ahead — caller must join as client
	Takeover bool    `json:"takeover"` // promoted over a live host at a new generation
	Message  string  `json:"message,omitempty"`
	// MemberID and Secret are set when this claim creates the session (plan 44):
	// the creator is minted as an admitted member in the same round trip. Secret
	// is returned exactly once and never appears on a later read.
	MemberID string `json:"memberId,omitempty"`
	Secret   string `json:"secret,omitempty"`
	// RelayTickets are minted with the creator secret so the host can register
	// on the session network before the first member cycle (ticket 17).
	RelayTickets map[string]string `json:"relayTickets,omitempty"`
}

// CutoverRequest is POST /v1/sessions/{pin}/cutover.
type CutoverRequest struct {
	NewGitURL   string `json:"newGitUrl"`
	From        string `json:"from,omitempty"`
	Drain       bool   `json:"drain,omitempty"`
	Generation  int    `json:"generation"` // writer's current generation
	LogsURL     string `json:"logsUrl,omitempty"`
	HostMachine string `json:"hostMachine,omitempty"`
}

// MemberJoinRequest is POST /v1/sessions/{pin}/members.
type MemberJoinRequest struct {
	Name       string            `json:"name"`
	Role       string            `json:"role"`
	Branch     string            `json:"branch,omitempty"`
	Machine    string            `json:"machine,omitempty"`
	Harness    string            `json:"harness,omitempty"`
	Capability *MemberCapability `json:"capability,omitempty"`
}

// RedeemRequest is POST /v1/sessions/{pin}/redeem — the unauthenticated knock
// (plan 44 ticket 06). RequestID is client-minted so a dropped connection
// re-attaches to the same queue entry.
type RedeemRequest struct {
	Name      string `json:"name"`
	Machine   string `json:"machine,omitempty"`
	RequestID string `json:"requestId"`
	// Source is the caller's address, set by the server before Knock — not wire.
	Source string `json:"-"`
	// Version is the knocker's build, read off VersionHeader by the server before
	// Knock — not wire, for the same reason Source is not: a body field is
	// something a client chooses, and this is something the server observes.
	Version string `json:"-"`
}

// Admission decisions (plan 44).
const (
	DecisionAccept  = "accept"
	DecisionDecline = "decline"
)

// MemberDecision is PUT /v1/sessions/{pin}/members/{id}/decision.
type MemberDecision struct {
	Decision string `json:"decision"` // accept | decline
}

// AccessPut is PUT /v1/sessions/{pin}/access (plan 44 ticket 07).
type AccessPut struct {
	Access string `json:"access"` // open | closed
}

// RedeemResult is the final SSE data frame on an accepted (or declined) knock.
type RedeemResult struct {
	Decision     string            `json:"decision"` // accepted | declined
	MemberID     string            `json:"memberId,omitempty"`
	Secret       string            `json:"secret,omitempty"`
	Message      string            `json:"message,omitempty"`
	Session      Session           `json:"session,omitempty"`
	RequestID    string            `json:"requestId,omitempty"`
	RelayTickets map[string]string `json:"relayTickets,omitempty"`
}

// RedeemPending is the first SSE frame on a held knock (plan 44 ticket 10).
// Acceptors are name@machine for everyone who can decide — admitted or left.
type RedeemPending struct {
	MemberID  string   `json:"memberId"`
	RequestID string   `json:"requestId"`
	Acceptors []string `json:"acceptors,omitempty"`
}

// MemberUpdate is PUT /v1/sessions/{pin}/members/{id}.
type MemberUpdate struct {
	Branch           string            `json:"branch,omitempty"`
	HeadSHA          string            `json:"headSha,omitempty"`
	MainMirrorSHA    string            `json:"mainMirrorSha,omitempty"`
	MainMirrorHeight int               `json:"mainMirrorHeight,omitempty"`
	Harness          string            `json:"harness,omitempty"`
	Capability       *MemberCapability `json:"capability,omitempty"`
	Role             string            `json:"role,omitempty"`
	// Version is the caller's build, read off VersionHeader by the server on
	// every member cycle — not wire. The cycle is where a laptop that upgrades
	// mid-session corrects the row the drain check reads (plan 48).
	Version string `json:"-"`
}

// MemberSync is POST /v1/sessions/{pin}/members/{id}/sync — the one uplink
// per member cycle (plan 43). Hold renews leases this member already owns;
// claims stay their own request. WantSnapshot asks for the session document
// back (every 12th cycle, or every cycle with no live stream).
type MemberSync struct {
	Update       MemberUpdate           `json:"update"`
	Generation   int                    `json:"generation"`
	Hold         []string               `json:"hold,omitempty"`
	Endpoints    map[string]EndpointPut `json:"endpoints,omitempty"`
	Convergence  *Convergence           `json:"convergence,omitempty"`
	Conductor    *ConductorRecord       `json:"conductor,omitempty"`
	WantSnapshot bool                   `json:"wantSnapshot,omitempty"`
}

// MemberSyncResult is the sync response. Conflict means the endpoint half was
// skipped because Generation was stale — the member upsert and lease renews
// still committed. Lost lists held services this member no longer owns.
// RelayTickets are fresh Ed25519 proofs for the session network (ticket 17),
// renewed each cycle so a control-plane outage does not strand a member whose
// previous ticket is still within TicketTTL.
type MemberSyncResult struct {
	Generation   int               `json:"generation"`
	Conflict     bool              `json:"conflict,omitempty"`
	Lost         []string          `json:"lost,omitempty"`
	Session      *Session          `json:"session,omitempty"`
	RelayTickets map[string]string `json:"relayTickets,omitempty"` // service → ticket
	// Refused lists endpoint kinds (and "conductor") this member tried to
	// publish while someone else held the matching lease (ticket 18). Data,
	// never an error — rolling the cycle back would take the member offline.
	Refused []string `json:"refused,omitempty"`
}

// EndpointPut is PUT /v1/sessions/{pin}/endpoints/{kind}.
type EndpointPut struct {
	URL        string `json:"url"`
	Host       string `json:"host,omitempty"`
	Port       int    `json:"port,omitempty"`
	Source     string `json:"source,omitempty"`
	Direct     string `json:"direct,omitempty"`
	Generation int    `json:"generation"` // required for addressing writes
}

// GenerationConflict is the 409 body when a writer is stale.
type GenerationConflict struct {
	Error             string `json:"error"`
	CurrentGeneration int    `json:"currentGeneration"`
	DemoteToClient    bool   `json:"demoteToClient"`
}

// Health is GET /healthz. Version used to ride here as a build fingerprint; it
// moved to the admin surface (abuse-surface ticket 23) so scanners stop learning
// which build's bugs are present. WaitHealthy only reads OK.
type Health struct {
	OK      bool   `json:"ok"`
	Version string `json:"version,omitempty"` // unused on the wire; kept for decode compatibility
	DB      string `json:"db"`
}

// ErrLeaseHeld surfaces a lost placement race: the service is held by a live
// owner. Callers stand down and report who holds it. It lives with the wire
// types because clients match on it (`slopball take`), and matching on it must
// not drag a database driver in.
var ErrLeaseHeld = errors.New("lease held")
