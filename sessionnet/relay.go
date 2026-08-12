package sessionnet

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nwylynko/slopball-cli/clientaddr"
	"github.com/nwylynko/slopball-cli/logx"
	"github.com/nwylynko/slopball-cli/relayticket"
	"github.com/nwylynko/slopball-cli/telemetry"
)

// The relay speaks one line of ASCII and then gets out of the way. Three verbs,
// all initiated by the peers — the relay never dials anybody, which is what
// lets it sit behind an address both sides can reach while neither can reach
// the other.
//
//	register <pin> <service>            holder's control connection, held open
//	accept   <pin> <service> <stream>   holder's data connection for one client
//	connect  <pin> <service>            client asking to be spliced
//
// It is deliberately ignorant of session keys. A rogue that registers a service
// it has no key for can deny service, but it cannot read or forge traffic — the
// handshake at each end sees to that. Keeping the relay out of the credential
// business is what lets slopball run one for many sessions.
//
// That ignorance is load-bearing and easy to erode. Validating a key would mean
// asking the control plane, and then a control-plane outage would break every
// NEW connection through the relay — the precise entanglement §8.1 forbids, and
// the reason a session must keep merging while the control plane is down.
// Rate limiting (below) is deliberately the only defence that needs no
// knowledge of anything.
// Proto is the greeting's first token, and it is EXPORTED because there is
// exactly one wire protocol and more than one implementation of it: relay.go,
// cloudflare/session/src/relay-do.js, and any test that has to speak the
// protocol without going through this package's holder (internal/relaylive
// does, to keep a splice genuinely idle). A second copy of the literal is how
// two implementations drift.
const Proto = "slopball-relay/1"

const relayProto = Proto

// Registration throttling defaults. Registration is unauthenticated, so a
// relay on a public hostname can be squatted: `register <pin> <service>` for a
// PIN you do not hold denies service to a session. Confidentiality and
// integrity are untouched (the handshake still fails for a rogue) — only
// availability is exposed, and this does not stop a targeted squat by someone
// who already knows a PIN.
//
// ~~"A PIN has to leak first, and sessions are short-lived"~~ was the rationale
// written here under §5.6; plan 44 overturned it (a PIN leaking by being seen
// is the exposure admission exists for), so do not read that sentence as
// current reasoning. Plan 45 / abuse-surface ticket 15: a registration may not
// displace a holder that is currently ping-alive. Availability defence with no
// credentials — the relay stays out of the session-key business (§8.1).
const (
	defaultRegisterBurst = 30
	defaultRegisterRate  = 2 * time.Second
	// A source that has legitimately held (pin, service) before may
	// re-register for free within this window. On mobile links reconnect is
	// the steady state, and a throttle that blocks a phone recovering from a
	// handover would be worse than no throttle at all.
	knownHolderWindow = 30 * time.Minute

	// Connect is the verb an attacker repeats (ticket 16). More generous than
	// register: git opens a connection per operation. Known-source free pass
	// (register or a prior successful connect) rides underneath.
	defaultConnectBurst = 60
	defaultConnectRate  = 100 * time.Millisecond // 10/s refill

	// Ceilings sized for one small relay box (~1 Gbps NIC, shared with other
	// sessions). ~80 concurrent sessions × ≤3 services ≈ 240 tunnels → 256;
	// splices leave headroom for clones-in-flight; 8 MiB/s/splice keeps one
	// peer from saturating the box alone.
	defaultMaxSplicesPerPin     = 32
	defaultMaxLiveTunnels       = 256
	defaultMaxConcurrentSplices = 1024
	defaultSpliceBytesPerSec    = 8 << 20 // 8 MiB/s
	defaultHealthBurst          = 100
	defaultHealthRate           = 10 * time.Millisecond // 100/s
)

// Holder liveness. A link that flaps does not close its sockets — a carrier
// handover leaves a connection that reads as open and never delivers anything.
// The WebSocket ping is the connection's OWN liveness signal, which is why
// eviction is driven by it rather than by a guessed interval.
const (
	holderPingEvery = 5 * time.Second
	holderPingGrace = 5 * time.Second
	// holderAliveProbe is how long register waits for a ping answer before
	// treating the incumbent as gone. Shorter than the watcher's grace: a squat
	// probe must not block for the full eviction window, and a dead link fails
	// the probe so a survivor can take over without waiting on TCP.
	holderAliveProbe = time.Second
)

// Relay splices holders and clients. It is a SEPARATE SERVICE from the control
// plane, and that separation is load-bearing: a running session holds its
// tunnel here and keeps merging across a control-plane outage (MASTERPLAN §8.1).
type Relay struct {
	// OnBytes, when set, observes every byte the relay forwards. It exists for
	// the test that proves those bytes are ciphertext; production leaves it nil.
	OnBytes func([]byte)

	// Telemetry records who registered, who connected, and how much every
	// splice moved before it ended (plan 46 ticket 09). The relay gains NO
	// database and no postgres driver for this — it POSTs, which keeps the
	// package split honest, keeps this the smaller image, and keeps the relay
	// trivially restartable. Nil records nothing.
	Telemetry *telemetry.Emitter

	// RegisterBurst / RegisterRate configure the per-source token bucket on
	// `register`. Zero means the defaults above.
	RegisterBurst int
	RegisterRate  time.Duration

	// ConnectBurst / ConnectRate configure the per-source token bucket on
	// `connect` (ticket 16). Zero means the defaults. A source in known
	// (legitimate holder reconnect, or a prior successful connect) rides free.
	ConnectBurst int
	ConnectRate  time.Duration

	// MaxSplicesPerPin caps concurrent waiting+active splices for one PIN.
	// Zero means the default. The amplifier connect opens is one goroutine and
	// one waiting channel per attempt for up to 15s.
	MaxSplicesPerPin int

	// MaxLiveTunnels is the global ceiling on registered (pin, service) holders.
	// Zero means the default.
	MaxLiveTunnels int

	// MaxConcurrentSplices is the global ceiling on waiting+active splices.
	// Zero means the default.
	MaxConcurrentSplices int

	// SpliceBytesPerSec paces each splice. Zero means the default. A splice
	// that would exceed it is closed rather than slowing every peer.
	SpliceBytesPerSec int

	// ProxyHops is how many reverse-proxy hops sit in front (SLOPBALL_PROXY_HOPS /
	// ADR 0005). Resolved at the WebSocket upgrade and carried on the connection
	// so sourceOf is not the TCP peer of the proxy.
	ProxyHops int

	// TicketPublic, when set, requires an Ed25519 relay ticket on register and
	// connect (ticket 17). The relay holds ONLY this public key — nothing that
	// could mint. Nil keeps today's PIN-only path for local unit tests that
	// have not been wired; production sets it via $SLOPBALL_RELAY_TICKET_PUBLIC.
	TicketPublic ed25519.PublicKey

	mu        sync.Mutex
	ln        net.Listener
	srv       *http.Server
	holders   map[string]*holderReg // "pin/service"
	waiting   map[string]chan bufferedConn
	buckets   map[string]*tokenBucket // register: source IP
	connectB  map[string]*tokenBucket // connect: source IP
	healthB   *tokenBucket            // global /healthz gate
	known     map[string]time.Time    // "source|pin/service" → last legitimate register/connect
	streamID  uint64
	splices   int            // global waiting+active
	pinSplice map[string]int // pin → waiting+active
	closed    bool
}

type holderReg struct {
	ctrl   net.Conn
	source string
	mu     sync.Mutex
}

// NewRelay constructs an unstarted relay.
func NewRelay() *Relay {
	return &Relay{
		holders:   map[string]*holderReg{},
		waiting:   map[string]chan bufferedConn{},
		buckets:   map[string]*tokenBucket{},
		connectB:  map[string]*tokenBucket{},
		known:     map[string]time.Time{},
		pinSplice: map[string]int{},
		ProxyHops: clientaddr.HopsFromEnv(),
	}
}

// Start binds and begins accepting. addr is host:port; ":0" picks a port.
//
// The listener is an HTTP server that upgrades to WebSocket at
// /relay/v1/<pin>/<service> — the dial names the session so an edge can route
// it before forwarding the upgrade — and
// only that — see transport.go for why there is no raw-TCP mode to fall back
// to. There is nothing to stay compatible with (no relay is deployed anywhere),
// and one transport means one implementation of keepalive and dead-peer
// eviction rather than two that disagree.
func (r *Relay) Start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: relayHTTPHandler(r, r.handle)}
	r.mu.Lock()
	r.ln = ln
	r.srv = srv
	r.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
	log := logx.New("relay")
	log.Infof("session relay listening on %s (ws %s/<pin>/<service>, health %s)", ln.Addr(), RelayPath, HealthPath)
	log.Infof("%s", clientaddr.Describe(r.ProxyHops))
	if len(r.TicketPublic) != 0 {
		log.Infof("relay tickets required (Ed25519 public key configured)")
	}
	return nil
}

// Addr is the relay's bind address, empty until started. It stays a bare
// host:port: it is what an operator points a control plane at, and
// NormalizeRelayURL turns it into a URL at dial time.
func (r *Relay) Addr() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ln == nil {
		return ""
	}
	return r.ln.Addr().String()
}

// URL is the address to advertise to members, written as they will dial it.
func (r *Relay) URL() string {
	addr := r.Addr()
	if addr == "" {
		return ""
	}
	return "ws://" + addr + RelayPath
}

// Close stops the relay and drops every registration.
func (r *Relay) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	srv := r.srv
	holders := r.holders
	r.holders = map[string]*holderReg{}
	r.mu.Unlock()
	for _, h := range holders {
		_ = h.ctrl.Close()
	}
	if srv != nil {
		return srv.Close()
	}
	return nil
}

func (r *Relay) handle(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		_ = c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	f := strings.Fields(strings.TrimSpace(line))
	if len(f) < 3 || f[0] != relayProto {
		_ = c.Close()
		return
	}
	switch f[1] {
	case "register":
		ticket, ok := takeTicketArg(f)
		if !ok {
			_ = c.Close()
			return
		}
		r.register(c, f[2], f[3], ticket)
	case "connect":
		ticket, ok := takeTicketArg(f)
		if !ok {
			_ = c.Close()
			return
		}
		r.connect(c, br, f[2], f[3], ticket)
	case "accept":
		if len(f) != 5 {
			_ = c.Close()
			return
		}
		r.acceptData(c, br, f[4])
	default:
		_ = c.Close()
	}
}

// takeTicketArg parses register/connect: proto verb pin service [ticket].
// Ticket is a single Fields token (v1.payload.sig has dots, not spaces).
func takeTicketArg(f []string) (ticket string, ok bool) {
	switch len(f) {
	case 4:
		return "", true
	case 5:
		return f[4], true
	default:
		return "", false
	}
}

// requireTicket enforces ticket 17 when TicketPublic is set. Returns false
// after writing unauthorized and closing. Ping-alive still applies after this.
//
// It returns the VERIFIED claims, which is the only thing telemetry may name a
// member or a session by: the pin and service on the wire are what the peer
// asserted, and a relay that recorded those would be recording a claim rather
// than a fact. With no TicketPublic configured (unit tests, a dev relay) there
// are no verified claims at all, so member and session uid stay empty rather
// than being filled in from the wire.
func (r *Relay) requireTicket(c net.Conn, pin, service, ticket string) (relayticket.Claims, bool) {
	if len(r.TicketPublic) == 0 {
		return relayticket.Claims{}, true
	}
	if ticket == "" {
		logx.New("relay").Warnf("%s: refused %s — ticket required", pin, service)
		r.recordDoor(eventRelayRegister, c, relayticket.Claims{PIN: pin, Service: service}, "unauthorized", time.Time{})
		_, _ = io.WriteString(c, "unauthorized\n")
		_ = c.Close()
		return relayticket.Claims{}, false
	}
	claims, err := relayticket.Verify(r.TicketPublic, ticket, time.Now())
	if err != nil || claims.PIN != pin || claims.Service != service {
		logx.New("relay").Warnf("%s: refused %s — bad ticket (%v)", pin, service, err)
		r.recordDoor(eventRelayRegister, c, relayticket.Claims{PIN: pin, Service: service}, "unauthorized", time.Time{})
		_, _ = io.WriteString(c, "unauthorized\n")
		_ = c.Close()
		return relayticket.Claims{}, false
	}
	return claims, true
}

func routeKey(pin, service string) string { return pin + "/" + service }

func sourceOf(c net.Conn) string {
	if rc, ok := c.(*relayConn); ok && rc.source != "" {
		return rc.source
	}
	a := c.RemoteAddr()
	if a == nil {
		return "unknown"
	}
	return clientaddr.PeerHost(a.String())
}

// allowRegister is the whole of step 6's defence, and it needs no knowledge of
// sessions, keys or the control plane — which is exactly why it was chosen over
// a registration token that would drag the relay into the credential business.
//
// A source that has legitimately registered this (pin, service) before rides
// free: that is a holder reconnecting after a drop, and it is the case the
// throttle must never catch. The free-pass stamp itself lives in rememberKnown,
// called only after register actually accepts the connection — stamping here
// used to let a successful displace of a live holder earn the pass (ticket 15).
func (r *Relay) allowRegister(source, key string) bool {
	burst := r.RegisterBurst
	if burst <= 0 {
		burst = defaultRegisterBurst
	}
	rate := r.RegisterRate
	if rate <= 0 {
		rate = defaultRegisterRate
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if seen, ok := r.known[source+"|"+key]; ok && now.Sub(seen) < knownHolderWindow {
		r.known[source+"|"+key] = now
		return true
	}
	b := r.buckets[source]
	if b == nil {
		b = &tokenBucket{tokens: float64(burst), last: now}
		r.buckets[source] = b
	}
	return b.take(now, float64(burst), rate)
}

// rememberKnown records that source legitimately registered key, so a
// reconnect within knownHolderWindow skips the register and connect buckets.
// Only call after register accepts — stamping every successful connect would
// let the first burst buy unlimited connects for 30 minutes.
func (r *Relay) rememberKnown(source, key string) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.known[source+"|"+key] = now
	r.forgetStaleKnownLocked(now)
}

// allowConnect is connect's half of ticket 16: per-source bucket with the same
// known-source free pass register uses. A phone recovering from a drop that
// already spoke this (pin, service) must not sit behind the flood.
func (r *Relay) allowConnect(source, key string) bool {
	burst := r.ConnectBurst
	if burst <= 0 {
		burst = defaultConnectBurst
	}
	rate := r.ConnectRate
	if rate <= 0 {
		rate = defaultConnectRate
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if seen, ok := r.known[source+"|"+key]; ok && now.Sub(seen) < knownHolderWindow {
		r.known[source+"|"+key] = now
		return true
	}
	b := r.connectB[source]
	if b == nil {
		b = &tokenBucket{tokens: float64(burst), last: now}
		r.connectB[source] = b
	}
	return b.take(now, float64(burst), rate)
}

func (r *Relay) maxSplicesPerPin() int {
	if r.MaxSplicesPerPin > 0 {
		return r.MaxSplicesPerPin
	}
	return defaultMaxSplicesPerPin
}

func (r *Relay) maxLiveTunnels() int {
	if r.MaxLiveTunnels > 0 {
		return r.MaxLiveTunnels
	}
	return defaultMaxLiveTunnels
}

func (r *Relay) maxConcurrentSplices() int {
	if r.MaxConcurrentSplices > 0 {
		return r.MaxConcurrentSplices
	}
	return defaultMaxConcurrentSplices
}

func (r *Relay) spliceBytesPerSec() int {
	if r.SpliceBytesPerSec > 0 {
		return r.SpliceBytesPerSec
	}
	return defaultSpliceBytesPerSec
}

// takeSpliceSlot reserves one waiting/active splice. Refuses without touching
// established splices when a ceiling is hit.
func (r *Relay) takeSpliceSlot(pin string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.splices >= r.maxConcurrentSplices() {
		return false
	}
	if r.pinSplice[pin] >= r.maxSplicesPerPin() {
		return false
	}
	r.splices++
	r.pinSplice[pin]++
	return true
}

func (r *Relay) releaseSpliceSlot(pin string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.splices > 0 {
		r.splices--
	}
	if n := r.pinSplice[pin]; n <= 1 {
		delete(r.pinSplice, pin)
	} else {
		r.pinSplice[pin] = n - 1
	}
}

// allowHealth is a cheap global gate so /healthz cannot be a liveness amplifier
// (#34). Not keyed on source — scanners rotate addresses.
func (r *Relay) allowHealth() bool {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.healthB == nil {
		r.healthB = &tokenBucket{tokens: float64(defaultHealthBurst), last: now}
	}
	return r.healthB.take(now, float64(defaultHealthBurst), defaultHealthRate)
}

// holderAlive is the register-path probe: true while the incumbent answers a
// WebSocket ping. No pinger means we cannot tell, so treat as alive — opening
// a squat when we are unsure is worse than asking the newcomer to retry.
func holderAlive(reg *holderReg) bool {
	if reg == nil || reg.ctrl == nil {
		return false
	}
	p, ok := reg.ctrl.(pinger)
	if !ok {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), holderAliveProbe)
	defer cancel()
	return p.Ping(ctx) == nil
}

// forgetStaleKnownLocked keeps the free-pass map from growing without bound on
// a relay serving many short sessions.
func (r *Relay) forgetStaleKnownLocked(now time.Time) {
	if len(r.known) < 4096 {
		return
	}
	for k, seen := range r.known {
		if now.Sub(seen) >= knownHolderWindow {
			delete(r.known, k)
		}
	}
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func (b *tokenBucket) take(now time.Time, burst float64, rate time.Duration) bool {
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() / rate.Seconds()
		if b.tokens > burst {
			b.tokens = burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// register takes a service's tunnel when free or when the incumbent is dead.
// A later registration used to REPLACE a live holder on purpose (lease move),
// but that also let anyone who had seen a PIN squat the git tunnel. Ticket 15:
// refuse while the incumbent answers ping; a legitimate migration registers
// only after the previous holder Stop'd (or the watcher evicted a corpse). A
// holder's own reconnect closes its old control connection first, so the slot
// is empty before registerOnce runs — never refused as "busy".
//
// The alive probe runs before the rate-limit bucket so a refused squat does not
// spend a token — and rememberKnown runs only after accept, so a displace of a
// live holder (which no longer happens) cannot earn the reconnect free pass.
func (r *Relay) register(c net.Conn, pin, service, ticket string) {
	started := time.Now()
	claims, ok := r.requireTicket(c, pin, service, ticket)
	if !ok {
		return
	}
	claims.PIN, claims.Service = pin, service
	key := routeKey(pin, service)
	source := sourceOf(c)

	// Probe outside the map lock: a ping can take up to holderAliveProbe.
	r.mu.Lock()
	old := r.holders[key]
	closed := r.closed
	r.mu.Unlock()
	if closed {
		_ = c.Close()
		return
	}
	if old != nil && holderAlive(old) {
		logx.New("relay").Warnf("%s: refused %s registration from %s — holder still alive", pin, service, source)
		r.recordDoor(eventRelayRegister, c, claims, "busy", started)
		_, _ = io.WriteString(c, "busy\n")
		_ = c.Close()
		return
	}

	if !r.allowRegister(source, key) {
		logx.New("relay").Warnf("%s: refused %s registration from %s — rate limited", pin, service, source)
		r.recordDoor(eventRelayRegister, c, claims, "rate-limited", started)
		_, _ = io.WriteString(c, "rate-limited\n")
		_ = c.Close()
		return
	}
	reg := &holderReg{ctrl: c, source: source}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = c.Close()
		return
	}
	if cur := r.holders[key]; cur != nil && cur != old {
		// Another register won the race. Re-probe without holding the lock.
		r.mu.Unlock()
		if holderAlive(cur) {
			logx.New("relay").Warnf("%s: refused %s registration from %s — holder still alive", pin, service, source)
			r.recordDoor(eventRelayRegister, c, claims, "busy", started)
			_, _ = io.WriteString(c, "busy\n")
			_ = c.Close()
			return
		}
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			_ = c.Close()
			return
		}
		if r.holders[key] != cur && r.holders[key] != nil {
			r.mu.Unlock()
			logx.New("relay").Warnf("%s: refused %s registration from %s — another holder claimed the slot", pin, service, source)
			r.recordDoor(eventRelayRegister, c, claims, "busy", started)
			_, _ = io.WriteString(c, "busy\n")
			_ = c.Close()
			return
		}
		old = r.holders[key]
	}
	if old == nil && len(r.holders) >= r.maxLiveTunnels() {
		r.mu.Unlock()
		logx.New("relay").Warnf("%s: refused %s registration from %s — live tunnel ceiling", pin, service, source)
		r.recordDoor(eventRelayRegister, c, claims, "full", started)
		_, _ = io.WriteString(c, "full\n")
		_ = c.Close()
		return
	}
	r.holders[key] = reg
	r.mu.Unlock()
	if old != nil {
		_ = old.ctrl.Close()
	}
	r.rememberKnown(source, key)
	if _, err := io.WriteString(c, "ok\n"); err != nil {
		_ = c.Close()
		return
	}
	r.recordDoor(eventRelayRegister, c, claims, "ok", started)
	logx.New("relay").Infof("%s: %s tunnel registered from %s", pin, service, source)

	stop := r.watchHolder(key, reg)

	// Hold it open. The holder never writes on the control connection, so the
	// read is purely how we learn it went away — and on a link that flapped
	// rather than closed, that read never returns, which is what watchHolder is
	// for.
	buf := make([]byte, 1)
	for {
		if _, err := c.Read(buf); err != nil {
			break
		}
	}
	close(stop)
	r.mu.Lock()
	if r.holders[key] == reg {
		delete(r.holders, key)
	}
	r.mu.Unlock()
	_ = c.Close()
	logx.New("relay").Infof("%s: %s tunnel closed", pin, service)
}

// watchHolder pings the control connection and evicts a holder that stops
// answering. This is not a timer papering over a race: the ping/pong IS the
// connection's liveness signal, and without it a vanished phone leaves clients
// spliced onto a corpse until TCP eventually gives up — minutes on a mobile
// network, and the session looks dead the whole time.
func (r *Relay) watchHolder(key string, reg *holderReg) chan struct{} {
	stop := make(chan struct{})
	p, ok := reg.ctrl.(pinger)
	if !ok {
		return stop
	}
	go func() {
		t := time.NewTicker(holderPingEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			ctx, cancel := context.WithTimeout(context.Background(), holderPingGrace)
			err := p.Ping(ctx)
			cancel()
			if err == nil {
				continue
			}
			select {
			case <-stop:
				return
			default:
			}
			logx.New("relay").Warnf("%s: holder stopped answering (%v) — evicting so a reconnect can take over", key, err)
			// Closing the control connection unblocks register's read, which
			// does the deregistration on one path for every cause.
			_ = reg.ctrl.Close()
			return
		}
	}()
	return stop
}

func (r *Relay) connect(client net.Conn, br *bufio.Reader, pin, service, ticket string) {
	started := time.Now()
	claims, ok := r.requireTicket(client, pin, service, ticket)
	if !ok {
		return
	}
	claims.PIN, claims.Service = pin, service
	key := routeKey(pin, service)
	source := sourceOf(client)
	if !r.allowConnect(source, key) {
		logx.New("relay").Warnf("%s: refused %s connect from %s — rate limited", pin, service, source)
		r.recordDoor(eventRelayConnect, client, claims, "rate-limited", started)
		_, _ = io.WriteString(client, "rate-limited\n")
		_ = client.Close()
		return
	}
	if !r.takeSpliceSlot(pin) {
		logx.New("relay").Warnf("%s: refused %s connect from %s — splice ceiling", pin, service, source)
		r.recordDoor(eventRelayConnect, client, claims, "full", started)
		_, _ = io.WriteString(client, "full\n")
		_ = client.Close()
		return
	}
	defer r.releaseSpliceSlot(pin)

	r.mu.Lock()
	reg := r.holders[key]
	if reg == nil {
		r.mu.Unlock()
		r.recordDoor(eventRelayConnect, client, claims, "no-holder", started)
		_, _ = io.WriteString(client, "no-holder\n")
		_ = client.Close()
		return
	}
	r.streamID++
	stream := fmt.Sprintf("%s-%d", pin, r.streamID)
	ch := make(chan bufferedConn, 1)
	r.waiting[stream] = ch
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		delete(r.waiting, stream)
		r.mu.Unlock()
	}()

	reg.mu.Lock()
	_, err := io.WriteString(reg.ctrl, fmt.Sprintf("accept %s\n", stream))
	reg.mu.Unlock()
	if err != nil {
		r.recordDoor(eventRelayConnect, client, claims, "no-holder", started)
		_, _ = io.WriteString(client, "no-holder\n")
		_ = client.Close()
		return
	}

	select {
	case up := <-ch:
		if _, err := io.WriteString(client, "ok\n"); err != nil {
			_ = up.conn.Close()
			_ = client.Close()
			return
		}
		r.recordDoor(eventRelayConnect, client, claims, "ok", started)
		// Free pass for connect is allowRegister's known map only (a source that
		// has held this key). Stamping every successful connect would let the
		// first burst buy unlimited connects for 30 minutes — defeating the
		// bucket for every NAT that got one ok through.
		//
		// A splice is not a request that returns, so its record is written when
		// it ENDS, carrying the bytes it moved and how long it lived. That
		// volume-and-lifetime pair is the only view there is of a session's
		// traffic: the relay carries ciphertext.
		spliceStart := time.Now()
		moved := r.splice(client, br, up.conn, up.buf)
		r.recordSplice(client, claims, moved, spliceStart)
	case <-time.After(15 * time.Second):
		r.recordDoor(eventRelayConnect, client, claims, "no-holder", started)
		_, _ = io.WriteString(client, "no-holder\n")
		_ = client.Close()
	}
}

// bufferedConn carries a connection together with the reader that has already
// consumed its greeting line. Handing the bare conn to the splice would drop
// anything the reader buffered past that line — the first records of the
// handshake, if the peer wrote them promptly.
type bufferedConn struct {
	conn net.Conn
	buf  *bufio.Reader
}

func (r *Relay) acceptData(c net.Conn, br *bufio.Reader, stream string) {
	r.mu.Lock()
	ch := r.waiting[stream]
	r.mu.Unlock()
	if ch == nil {
		_ = c.Close()
		return
	}
	select {
	case ch <- bufferedConn{conn: c, buf: br}:
	default:
		_ = c.Close()
	}
}

// splice returns how many bytes crossed it, both directions summed. The count
// is all there is to record: the payload is end-to-end encrypted, so the relay
// forwards ciphertext it cannot read.
func (r *Relay) splice(a net.Conn, aBuf *bufio.Reader, b net.Conn, bBuf *bufio.Reader) int64 {
	defer a.Close()
	defer b.Close()
	bps := r.spliceBytesPerSec()
	var moved atomic.Int64
	done := make(chan struct{}, 2)
	go func() {
		n, _ := io.Copy(b, r.pace(r.observe(aBuf), bps))
		moved.Add(n)
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(a, r.pace(r.observe(bBuf), bps))
		moved.Add(n)
		done <- struct{}{}
	}()
	<-done
	return moved.Load()
}

func (r *Relay) observe(src io.Reader) io.Reader {
	if r.OnBytes == nil {
		return src
	}
	return &observedReader{src: src, fn: r.OnBytes}
}

// pace bounds one direction of a splice to bytesPerSec. One peer that would
// otherwise saturate the NIC is slowed rather than allowed to starve every
// other live splice on the box.
func (r *Relay) pace(src io.Reader, bytesPerSec int) io.Reader {
	if bytesPerSec <= 0 {
		return src
	}
	return &pacedReader{src: src, bps: bytesPerSec, last: time.Now(), tokens: float64(bytesPerSec)}
}

type pacedReader struct {
	src    io.Reader
	bps    int
	tokens float64
	last   time.Time
}

func (p *pacedReader) Read(b []byte) (int, error) {
	now := time.Now()
	if elapsed := now.Sub(p.last); elapsed > 0 {
		p.tokens += elapsed.Seconds() * float64(p.bps)
		if p.tokens > float64(p.bps) {
			p.tokens = float64(p.bps)
		}
		p.last = now
	}
	if p.tokens < 1 {
		// Wait for one byte's worth rather than returning 0 (Copy treats 0,nil as busy-spin).
		wait := time.Duration((1 - p.tokens) / float64(p.bps) * float64(time.Second))
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		time.Sleep(wait)
		return p.Read(b)
	}
	max := int(p.tokens)
	if max > len(b) {
		max = len(b)
	}
	if max > p.bps/10 && p.bps/10 > 0 {
		max = p.bps / 10 // ~100ms chunks
	}
	n, err := p.src.Read(b[:max])
	if n > 0 {
		p.tokens -= float64(n)
	}
	return n, err
}

type observedReader struct {
	src io.Reader
	fn  func([]byte)
}

func (o *observedReader) Read(p []byte) (int, error) {
	n, err := o.src.Read(p)
	if n > 0 {
		b := make([]byte, n)
		copy(b, p[:n])
		o.fn(b)
	}
	return n, err
}

// --- telemetry (plan 46 ticket 09) -------------------------------------------
//
// The relay reports what it is CARRYING: who registered, who connected, and how
// much every splice moved before it ended. It carries ciphertext, so volume and
// lifetime are all there is to record — and all that is needed to answer "is
// anyone abusing this".
//
// Every field that names a session or a person comes off the verified ticket
// claims. The pin and service on the wire are what the peer asserted; recording
// those would be recording a claim rather than a fact.
const (
	eventRelayRegister    = "relay.register"
	eventRelayConnect     = "relay.connect"
	eventRelaySpliceClose = "relay.splice.close"
)

// recordDoor records one register or connect and its outcome. The refusals are
// the interesting half: "is anyone abusing this" is answered by what was turned
// away, not by what got through.
func (r *Relay) recordDoor(name string, c net.Conn, claims relayticket.Claims, outcome string, started time.Time) {
	if r.Telemetry == nil {
		return
	}
	var ms int64
	if !started.IsZero() {
		ms = time.Since(started).Milliseconds()
	}
	r.Telemetry.Emit(name, telemetry.Event{
		Source: sourceOf(c), PIN: claims.PIN, SessionUID: claims.SessionUID,
		Member: claims.MemberID, DurationMS: ms,
		// The session-network service (git/dev/exec) is not the emitting
		// SERVICE column, which is always "relay" — so it rides data.
		Data: map[string]any{"service": claims.Service, "outcome": outcome},
	})
}

// recordSplice records a finished splice: bytes and duration, and nothing else.
// There is no body and there can never be one.
func (r *Relay) recordSplice(c net.Conn, claims relayticket.Claims, moved int64, started time.Time) {
	if r.Telemetry == nil {
		return
	}
	r.Telemetry.Emit(eventRelaySpliceClose, telemetry.Event{
		Source: sourceOf(c), PIN: claims.PIN, SessionUID: claims.SessionUID,
		Member: claims.MemberID,
		Bytes:  moved, DurationMS: time.Since(started).Milliseconds(),
		Data: map[string]any{"service": claims.Service},
	})
}
