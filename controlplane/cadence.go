package controlplane

import "time"

// Cadences for the member conversation (plan 43). Each lives next to the
// timeout it must beat, with the ratio stated — every bug in this area came
// from a cadence and its timeout being five files apart.

const (
	// MemberCycle is the one remaining poll: presence + renews + published
	// state. Through a proxy a held stream proves the proxy is alive, not the
	// member, so liveness stays something the member asserts.
	MemberCycle = 5 * time.Second

	// DefaultLeaseTTL is 30s — 6 renewals per TTL at MemberCycle.
	// (Defined on types.go as DefaultLeaseTTL = 30.)

	// MemberOnlineAfter is how recently last_seen_at must be for Online=true.
	// 4× MemberCycle, the same margin as today's 2s/10s.
	MemberOnlineAfter = 20 * time.Second

	// SnapshotEvery is how many cycles between WantSnapshot requests — the
	// lost-frame backstop. With no live stream this collapses to 1.
	SnapshotEvery = 12

	// SSEPingInterval is how often the server writes ": ping" down an idle
	// stream. It is the only traffic a quiet session generates, and it is what
	// a member measures its stream's liveness against.
	SSEPingInterval = 15 * time.Second

	// StreamSilentAfter is how long without a single byte before a member
	// judges its own stream dead — 2.3× SSEPingInterval, so one dropped ping
	// never flaps a healthy stream. This has to be measured from bytes
	// actually received rather than from whether the socket is still open: a
	// carrier handover or a buffering proxy leaves the connection up and goes
	// quiet, which is the exact case the degradation rule exists for.
	StreamSilentAfter = 35 * time.Second

	// MirrorFloor: client mirror fetch on stream frame, else this often.
	// Collapses to MemberCycle with no live stream.
	MirrorFloor = 60 * time.Second

	// RemoteConductorFloor: remote host.Refresh on sync.pushed, else this.
	// §8.1: the stream may only make a merge happen sooner, never be what
	// makes it happen — with the control plane dark, merging continues at 10s.
	RemoteConductorFloor = 10 * time.Second

	// LogsFloor: /logs stream reconnect / poll floor. Collapses to MemberCycle
	// with no live stream.
	LogsFloor = 60 * time.Second

	// IdleTTL is how long after the last real work a session lives. Heartbeats,
	// cycles and lease renewals deliberately do not extend it (plan 43 / ADR 0003).
	// Neither does a knock (plan 44 ticket 07) — only accepting one does.
	IdleTTL = 3 * time.Hour

	// DoorRefuseDelay is how long the control plane waits before answering a
	// redeem that will never hold (closed, full, or no such session).
	//
	// Deliberate cost, not a race workaround: a valid PIN yields a held
	// connection and an invalid one an error, so this imposes cost per guess
	// and hides nothing about which PINs exist. Do not delete it citing the
	// house rule about timers — that rule is about racing readiness, and this
	// is the opposite.
	DoorRefuseDelay = 2 * time.Second

	// Redeem flood limits (plan 44 ticket 08). Sized for a team of ten on one
	// wifi, not for an attacker — the per-source bucket stays generous on
	// purpose (same reasoning as sessionnet.allowRegister).
	RedeemPendingPerSession = 64
	// RedeemConcurrentPerSource sits between RosterLimit and the pending cap,
	// and both bounds are load-bearing.
	//
	// Above RosterLimit because a hackathon puts the whole team behind one
	// venue address, so the honest worst case for a single source is every
	// machine a session could still admit, arriving at once. At 8 against a
	// roster of 10 the ninth teammate was refused with "too many concurrent
	// join requests held from this address" — the limiter causing the failure
	// it exists to prevent.
	//
	// Below RedeemPendingPerSession so the two caps stay distinguishable: at or
	// above it, one address saturating itself also saturates the session, and
	// a refused neighbour can no longer tell "that address is doing too much"
	// from "this session's queue is full".
	//
	// Pending is 64 (abuse-surface ticket 14): at concurrent/source 16, pending
	// 32 meant two addresses shut a door; 64 restores four. Test the ratio
	// (pending÷concurrent ≥ 4), not the constants in isolation.
	RedeemConcurrentPerSource = 16
	RedeemAttemptsPerMinute   = 60
	// Burst sits ABOVE the other two so each cap can still be the one that
	// speaks. Set at or below them, the bucket empties first and every refusal
	// blames the rate — hiding whether the session's queue or the address's
	// concurrency was the real limit, which is exactly what plan 46 has to
	// report on.
	RedeemAttemptBurst = 40

	// Session SSE stream ceilings (abuse-surface ticket 14). A member
	// legitimately holds one stream (plan 43); 3 is generous and mostly bounds
	// a buggy client leaking reconnects. Per-pin is roster × per-member so a
	// full session of leaky clients still has a bound.
	StreamPerMemberMax = 3
	StreamPerPinMax    = RosterLimit * StreamPerMemberMax // 30

	// HeldConnectionGlobalMax covers redeem holds and session SSE streams
	// together — the same goroutine/row cost from the machine's point of view.
	// Default when SLOPBALL_BUDGET_CEILING is unset (= BudgetDefaultHeldConnections).
	// When the ceiling is set: ceiling × HeldConnectionsPerBoxSlot (budget.go).
	HeldConnectionGlobalMax = BudgetDefaultHeldConnections

	// Knock text bounds (abuse-surface ticket 09). name / machine / requestId
	// come from an unauthenticated stranger and are rendered in every member's
	// users tab — the only unauthenticated write that reaches a human's screen.
	// 64 matches POSIX HOST_NAME_MAX and the invite bound (#9); refuse, never
	// truncate, so an admit decision is reading what was actually sent.
	KnockNameMax      = 64
	KnockMachineMax   = 64
	KnockRequestIDMax = 64

	// Session-state write bounds (abuse-surface ticket 19). Enforced in the
	// *Tx functions both doors call — PUT handlers and MemberSync — so a bound
	// never allocates per field and never lives only on the quieter door.
	EndpointURLMax   = 512
	MaxBranchesAhead = 32
	MaxBranchNameLen = 255
	MemberNameMax    = 64 // invite name; same as KnockNameMax
	MemberMachineMax = 64
	RoleActivityMax  = 200
	RoleModelMax     = 200

	// Create metering (abuse-surface ticket 11). Sized for a venue, not an
	// attacker: ten teammates arriving, a laptop rebooting and a re-host after
	// a crash is under 15 in the first hour. Do not tighten without re-deriving
	// from that scenario. Resume and takeover are not metered — they need a
	// Bearer already.
	CreateAttemptsPerHour = 30
	CreateAttemptBurst    = 30

	// Distinct unknown PINs on redeem (abuse-surface ticket 12). Per-PIN was
	// always the wrong key: a scanner never guesses the same PIN twice, while
	// an honest mistype repeats one. Twenty distinct misses in five minutes is
	// well above a person hunting for a typo and well below a scan; the window
	// matches the old (dead) join_attempts lockout so operators keep one clock.
	RedeemUnknownPINDistinctMax = 20
	RedeemUnknownPINWindow      = 5 * time.Minute

	// Box request bounds (abuse-surface ticket 13). BoxConcurrentGlobal is the
	// first consumer of the budget ceiling (ticket 24): when
	// SLOPBALL_BUDGET_CEILING is set it equals that number; when unset it is
	// BudgetDefaultBoxConcurrent.
	BoxPerSourcePerDay    = 10
	BoxPerSessionLifetime = 3
	BoxConcurrentGlobal   = BudgetDefaultBoxConcurrent
	// BoxHoldStaleAfter must beat the 20-minute provision timeout so a live
	// boot is not swept mid-flight, and still release a crashed control
	// plane's abandoned slot.
	BoxHoldStaleAfter = 25 * time.Minute

	// Member-published event append rate (abuse-surface ticket 20). Per member,
	// not per address: the feed is attributable and a venue wifi sharing one
	// NAT must not let one laptop starve the others. Sized for a busy session
	// (task-boundary syncs + merger applies), not a flood — a normal topology
	// writes a handful per minute.
	EventsPerMemberPerMinute = 60
	EventsPerMemberBurst     = 60

	// Event payload string caps (ticket 20). Intent is the human-readable
	// half of the work feed; 512 matches the endpoint URL bound.
	EventIntentMax = 512
	EventSHAMax    = 64
)

// Floor returns the healthy-stream floor, or MemberCycle when the stream is
// not live — the one degradation rule.
func Floor(healthy time.Duration, streamLive bool) time.Duration {
	if streamLive {
		return healthy
	}
	return MemberCycle
}
