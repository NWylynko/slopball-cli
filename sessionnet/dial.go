package sessionnet

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/slopball-cli/logx"
)

// Dialer is the client half of the session network. It dials the relay, never
// the peer — so it works from inside client-isolated wifi, and the address it
// uses does not change when a lease migrates to a different machine. That last
// property is the one a mesh could not have given us: the client-visible
// endpoint is the relay, and the holder moves behind it.
type Dialer struct {
	Relay string
	PIN   string
	Key   Key
	// Direct, when set, is tried before the relay for a service — a LAN or
	// same-machine session should not pay for a round trip through a relay.
	// The handshake is identical on both paths, so this is an optimisation and
	// never a weaker one.
	//
	// It pays whenever teammates share a LAN or tether to one person's phone,
	// and it costs the thing dual paths always cost: a route that fires only
	// sometimes is how "works for me" bugs happen. That is paid down with
	// observability rather than a flag — every Dial says which path it took and
	// LastPath keeps the answer for the monitor.
	Direct func(service string) string

	// DialContext overrides how the underlying sockets are opened. Tests use it
	// to sever a link the way a carrier handover does; production leaves it nil.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)

	// Ticket returns a current Ed25519 relay ticket for this PIN and the
	// service being dialled. Required when the relay verifies tickets.
	Ticket func(service string) (string, error)
}

var lastPath sync.Map // "pin/service" → "direct <addr>" | "relay <addr>"

// LastPath reports how this process most recently reached a session service,
// for `slopball monitor` and slopdebug. An unobservable dual path is the
// failure mode that wastes an hour on stage; one that announces itself is
// diagnosable in seconds.
func LastPath(pin, service string) string {
	v, ok := lastPath.Load(routeKey(pin, service))
	if !ok {
		return ""
	}
	return v.(string)
}

func recordPath(pin, service, how, addr string) {
	lastPath.Store(routeKey(pin, service), how+" "+addr)
	logx.New("sessionnet").Infof("%s: %s connected via %s %s", pin, service, how, addr)
}

// Dial opens an authenticated, encrypted connection to whoever currently holds
// service's lease.
func (d *Dialer) Dial(ctx context.Context, service string) (net.Conn, error) {
	if d.Key.Zero() {
		return nil, fmt.Errorf("sessionnet: no session key for %s — resolve the PIN through the control plane first", d.PIN)
	}
	if d.Direct != nil {
		if addr := d.Direct(service); addr != "" {
			// The direct path is an OPTIMISATION and the relay is the mechanism,
			// so the bet on it is bounded. It used to take the caller's whole
			// context, which meant a published address that answers nothing cost
			// the Go transport's 30-second connect budget on EVERY connection —
			// git opens one per operation, so a clone-and-push spent two minutes
			// waiting on a route that was never going to work.
			dctx, cancel := context.WithTimeout(ctx, directBudget)
			c, err := d.dialDirect(dctx, service, addr)
			cancel()
			if err == nil {
				return c, nil
			} else if errors.Is(err, errHandshake) {
				// A reachable peer that cannot complete the handshake is not a
				// routing problem, and retrying via the relay would only hide it.
				return nil, err
			}
			logx.New("sessionnet").Debugf("%s: %s direct %s did not answer (%v) — using the relay", d.PIN, service, addr, err)
		}
	}
	if d.Relay == "" {
		return nil, fmt.Errorf("sessionnet: no relay address for session %s", d.PIN)
	}
	c, err := dialRelay(ctx, d.Relay, d.PIN, service, d.DialContext)
	if err != nil {
		return nil, err
	}
	ticket := ""
	if d.Ticket != nil {
		ticket, err = d.Ticket(service)
		if err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("sessionnet: relay ticket for %s: %w", service, err)
		}
	}
	line := fmt.Sprintf("%s connect %s %s", relayProto, d.PIN, service)
	if ticket != "" {
		line += " " + ticket
	}
	if _, err := fmt.Fprintf(c, "%s\n", line); err != nil {
		_ = c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
	replyLine, err := br.ReadString('\n')
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("sessionnet: relay did not answer: %w", err)
	}
	_ = c.SetReadDeadline(time.Time{})
	if strings.TrimSpace(replyLine) != "ok" {
		reply := strings.TrimSpace(replyLine)
		_ = c.Close()
		switch reply {
		case "rate-limited":
			return nil, fmt.Errorf("sessionnet: relay rate-limited the %s connect", service)
		case "full":
			return nil, fmt.Errorf("sessionnet: relay is at capacity for session %s", d.PIN)
		case "unauthorized":
			return nil, fmt.Errorf("sessionnet: relay refused the %s connect — ticket missing or invalid", service)
		default:
			return nil, fmt.Errorf("sessionnet: %s has no live %s holder — nobody is serving it right now", d.PIN, service)
		}
	}
	if br.Buffered() > 0 {
		_ = c.Close()
		return nil, errors.New("sessionnet: relay sent unexpected data before the handshake")
	}
	sec, err := handshake(c, d.Key, true)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	recordPath(d.PIN, service, "relay", d.Relay)
	return sec, nil
}

// directBudget is the whole cost of trying the shorter route before falling
// back to the one that always works. The direct path exists for peers on the
// same LAN, the same hotspot or the same Tailscale mesh, where a connection and
// a handshake are single-digit milliseconds — so this is a hundredfold headroom
// and still bounded. A stale endpoint is ordinary: a laptop that changed
// networks, a box that was reprovisioned, a DHCP lease that moved.
// Regression: TestADeadDirectAddressDoesNotStallTheRelayFallback.
const directBudget = 5 * time.Second

// dialDirect reaches the holder on a machine address, skipping the relay. It
// runs the IDENTICAL handshake — the direct path is a shorter route, never a
// weaker one, which is what keeps "holding the session key is the
// authorization" true on both paths.
//
// ctx bounds the ATTEMPT, not the connection it returns: every stage below is
// tied to it, including the handshake, whose own window is far longer than a
// direct peer is ever worth waiting for.
func (d *Dialer) dialDirect(ctx context.Context, service, addr string) (net.Conn, error) {
	c, err := dialRelay(ctx, addr, d.PIN, service, d.DialContext)
	if err != nil {
		return nil, err
	}
	// Everything after the upgrade reads and writes on the conn itself, so the
	// budget is enforced by closing it — which is also what unblocks a peer that
	// completed the upgrade and then went silent.
	release := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer release()
	if _, err := fmt.Fprintf(c, "%s direct %s %s\n", relayProto, d.PIN, service); err != nil {
		_ = c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(directBudget))
	line, err := br.ReadString('\n')
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("sessionnet: direct holder %s did not answer: %w", addr, err)
	}
	_ = c.SetReadDeadline(time.Time{})
	if strings.TrimSpace(line) != "ok" {
		_ = c.Close()
		return nil, fmt.Errorf("sessionnet: direct holder %s refused %s", addr, service)
	}
	sec, err := handshake(&prefixedConn{Conn: c, buf: br}, d.Key, true)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	// Stop the watchdog BEFORE the caller's cancel lands, or a successful direct
	// dial would be closed the moment Dial tidies its budget away.
	release()
	recordPath(d.PIN, service, "direct", addr)
	return sec, nil
}

// prefixedConn hands the handshake anything the greeting reader buffered past
// the line it consumed, so a peer that wrote its first record promptly does not
// lose it.
type prefixedConn struct {
	net.Conn
	buf *bufio.Reader
}

func (c *prefixedConn) Read(p []byte) (int, error) { return c.buf.Read(p) }

// ForwarderConfig is a client-side loopback forwarder: one local TCP port that
// carries a session service.
type ForwarderConfig struct {
	Relay   string
	PIN     string
	Service string
	Key     Key
	Direct  func(service string) string
	// Addr is the local bind address; "" means 127.0.0.1:0.
	Addr string
	// LocalHost is the host:port to hand humans and tools, when it should differ
	// from the primary listener's own address — the dev forwarder's
	// <pin>.slopball.localhost:<port> name. Empty means the listener address.
	LocalHost string
	// AlsoAddr are additional local bind addresses spliced onto the same
	// service. The dev forwarder binds both loopbacks with it: a name like
	// <pin>.slopball.localhost resolves ::1 FIRST on this machine, so a v4-only
	// listener survives only on the client's retry after a refused connection.
	AlsoAddr []string
	// DialContext overrides how the underlying sockets are opened (tests).
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// Ticket is forwarded to the Dialer for each splice.
	Ticket func(service string) (string, error)
}

// Forwarder is what makes the session network need no TUN device and no root.
// slopball owns git's `origin` URL and the browser's demo URL, so a loopback
// listener that splices into the session network is a complete substitute for
// an IP-level VPN — for exactly the connections slopball actually makes.
type Forwarder struct {
	ln     net.Listener
	extra  []net.Listener
	cfg    ForwarderConfig
	cancel context.CancelFunc
	once   sync.Once
}

// Forward starts the local listener.
func Forward(ctx context.Context, cfg ForwarderConfig) (*Forwarder, error) {
	addr := cfg.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	f := &Forwarder{ln: ln, cfg: cfg, cancel: cancel}
	for _, a := range cfg.AlsoAddr {
		eln, err := net.Listen("tcp", a)
		if err != nil {
			// A machine with IPv6 switched off is not a broken machine, and the
			// primary listener is what the URL is built from — so an extra bind
			// that fails is reported and skipped rather than fatal.
			logx.New("sessionnet").Debugf("%s: %s forwarder could not also bind %s: %v", cfg.PIN, cfg.Service, a, err)
			continue
		}
		f.extra = append(f.extra, eln)
	}
	go f.acceptOn(ctx, f.ln)
	for _, eln := range f.extra {
		go f.acceptOn(ctx, eln)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		for _, eln := range f.extra {
			_ = eln.Close()
		}
	}()
	return f, nil
}

// URL is the http:// address local tools point at.
func (f *Forwarder) URL() string { return "http://" + f.ln.Addr().String() }

// Addr is the local host:port actually bound.
func (f *Forwarder) Addr() string { return f.ln.Addr().String() }

// LocalHost is the host:port to put in front of a human, which is the name when
// there is one and the bound address otherwise.
func (f *Forwarder) LocalHost() string {
	if f.cfg.LocalHost != "" {
		return f.cfg.LocalHost
	}
	return f.Addr()
}

// Close stops forwarding.
func (f *Forwarder) Close() error {
	f.once.Do(f.cancel)
	return nil
}

func (f *Forwarder) acceptOn(ctx context.Context, ln net.Listener) {
	d := &Dialer{
		Relay: f.cfg.Relay, PIN: f.cfg.PIN, Key: f.cfg.Key,
		Direct: f.cfg.Direct, DialContext: f.cfg.DialContext,
		Ticket: f.cfg.Ticket,
	}
	for {
		local, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer local.Close()
			remote, err := d.Dial(ctx, f.cfg.Service)
			if err != nil {
				logx.New("sessionnet").Warnf("%s: %s forwarder: %v", f.cfg.PIN, f.cfg.Service, err)
				return
			}
			defer remote.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(remote, local); done <- struct{}{} }()
			go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()
			<-done
		}()
	}
}

// RewriteHostPort replaces the host:port of rawURL, keeping path and scheme —
// how a canonical git URL becomes a forwarder-local one.
func RewriteHostPort(rawURL, hostPort string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	u.Host = hostPort
	return u.String(), nil
}
