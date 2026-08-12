package sessionnet

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/nwylynko/slopball-cli/clientaddr"
)

// Everything on the session network is carried over WebSocket, and that is a
// transport decision made for OUR ingress rather than anyone's egress.
//
// The relay has to be hosted somewhere both a holder and a client can reach.
// The public front door slopball actually has is an outbound-only HTTP tunnel,
// which cannot carry raw TCP on a bespoke port — so a raw-TCP relay is code
// that works and cannot be deployed. WebSocket makes the relay hostable behind
// any HTTP ingress, which is the whole gap plan 38 closes. Surviving a network
// that only permits 443 is a real second benefit, not the headline.
//
// It is also the standard answer rather than a workaround: `cloudflared access
// tcp` carries TCP over a tunnel by wrapping the stream in a WebSocket. We do
// natively what it bolts on, which saves a second binary on every laptop.
//
// No crypto lives here. The payload is already end-to-end encrypted by the
// X25519/PSK handshake running THROUGH the splice (conn.go); this layer is
// framing and transport, and the relay must keep seeing ciphertext only.
const (
	// RelayPath is the HTTP path the relay upgrades on. An ingress routes a
	// hostname at the relay; this is the only route below it that matters.
	RelayPath = "/relay/v1"
	// HealthPath is what makes `restart: unless-stopped` mean something.
	HealthPath = "/healthz"
)

// NormalizeRelayURL turns whatever the control plane advertised into the
// WebSocket base URL. A bare host:port is accepted and normalized to
// ws://host:port/relay/v1 — loopback and test convenience only. Production
// needs wss://: the payload is E2E encrypted either way, but the
// `register <pin> <service>` greeting — and now the dial URL itself — rides the
// transport in the clear, so plain ws:// on a path strangers share leaks
// session PINs. That is a refusal rather than a warning: a warning shipped a
// PIN leak in silence every time somebody pasted the wrong scheme into an env
// var.
func NormalizeRelayURL(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", fmt.Errorf("sessionnet: no relay address")
	}
	if !strings.Contains(addr, "://") {
		// Bare host:port. Validate it as one so a typo fails here rather than
		// as an inscrutable dial error much later.
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return "", fmt.Errorf("sessionnet: relay address %q is neither a URL nor host:port: %w", addr, err)
		}
		addr = "ws://" + addr + RelayPath
	}
	u, err := url.Parse(addr)
	if err != nil {
		return "", fmt.Errorf("sessionnet: relay address %q: %w", addr, err)
	}
	switch u.Scheme {
	case "ws", "wss":
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("sessionnet: relay address %q must be ws:// or wss:// (or a bare host:port)", addr)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = RelayPath
	}
	if u.Scheme == "ws" && !isPrivateHost(u.Hostname()) {
		return "", fmt.Errorf(
			"sessionnet: relay %s is plain ws:// — the session PIN rides the transport in the clear on every register and connect. Use wss://%s%s",
			u.String(), u.Host, u.Path)
	}
	return u.String(), nil
}

// RelayDialURL is the address a peer actually opens: the relay base plus
// <pin>/<service>. The session has to be named BEFORE the upgrade, because the
// Durable Object port routes on the URL and cannot read the greeting to pick an
// id. The ticket deliberately stays in the greeting — only routing needs
// pre-upgrade facts, and URLs get logged.
func RelayDialURL(addr, pin, service string) (string, error) {
	base, err := NormalizeRelayURL(addr)
	if err != nil {
		return "", err
	}
	if pin == "" || service == "" {
		return "", fmt.Errorf("sessionnet: a relay dial must name a pin and a service")
	}
	return strings.TrimSuffix(base, "/") + "/" + url.PathEscape(pin) + "/" + url.PathEscape(service), nil
}

// isPrivateHost decides whether plain ws:// is tolerable. Loopback is the local
// suite and `make control-up`; a literal private address is the docker bridge
// the e2e harness reaches its relay container on and the LAN tier the reach
// story already names. Anything else — including any hostname, which we will
// not resolve at dial time — is treated as the public internet and refused.
func isPrivateHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// dialRelay is the ONE place that knows how a session-network connection is
// made. Every caller — client, holder control connection, holder data
// connection, direct dial — asks for a net.Conn and gets one, which is what
// let the transport swap happen without any of them learning WebSocket. It
// mirrors controlplane.Client.Dialable, and for the same reason.
func dialRelay(ctx context.Context, addr, pin, service string, dial dialFunc) (net.Conn, error) {
	target, err := RelayDialURL(addr, pin, service)
	if err != nil {
		return nil, err
	}
	opts := &websocket.DialOptions{
		HTTPClient:      &http.Client{Transport: relayTransport(dial)},
		CompressionMode: websocket.CompressionDisabled,
	}
	c, _, err := websocket.Dial(ctx, target, opts)
	if err != nil {
		return nil, fmt.Errorf("sessionnet: dial relay %s: %w", target, err)
	}
	// WithoutCancel: the ctx passed in often bounds one request, while the
	// connection it returns is spliced for the life of a git fetch. Deadlines
	// still work — relayConn implements them itself — so nothing loses its bound.
	return newRelayConn(websocket.NetConn(context.WithoutCancel(ctx), c, websocket.MessageBinary), c, ""), nil
}

// dialFunc lets a test sever the underlying sockets the way a carrier handover
// does. nil means an ordinary net.Dialer.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func relayTransport(dial dialFunc) http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if dial != nil {
		t.DialContext = dial
	}
	return t
}

// relayConn is a net.Conn that can also be pinged. The ping is how a vanished
// peer is detected on a link that flapped rather than closed — on a phone that
// is the steady state, not an edge case, and a TCP connection to a radio that
// went away can sit "open" for minutes.
//
// source is the quota subject resolved at the WebSocket upgrade (ADR 0005);
// sourceOf reads it rather than the TCP peer of a reverse proxy.
//
// ⚠️ It also OWNS THE READ DEADLINE, and that is the whole of a shipped defect.
//
// coder/websocket's NetConn documents that "when a deadline is hit and there is
// an active read or write goroutine, the connection will be closed" — there is
// no way to abort one message mid-stream and resynchronise, so it destroys the
// socket instead. net/http's SERVER assumes the opposite of every net.Conn it
// is handed: it parks a background read on the connection while a handler runs
// (server.go startBackgroundRead) and cancels it the instant the response has
// been flushed —
//
//	w.conn.bufw.Flush()
//	w.conn.r.abortPendingRead()   →   rwc.SetReadDeadline(aLongTimeAgo)
//
// — resetting the deadline to zero one line later and carrying on. Handing
// that call straight to the WebSocket killed the socket the response had just
// been written to, so a managed box's git operations failed with
// `curl 52 Empty reply from server` on perhaps one request in eight, while the
// relay ledger showed a healthy splice and nothing logged an error anywhere.
// The rate is a goroutine race — whether the background read had reached Read
// yet — which is why it read as "the network is flaky".
//
// So the read side is pumped into readq by one goroutine that never has a
// deadline, and Read implements ORDINARY net.Conn semantics on top: an expired
// deadline returns os.ErrDeadlineExceeded and leaves the connection usable. At
// most one chunk is ever read ahead (readq is unbuffered), so this buffers
// nothing and changes no flow control. Write deadlines still go to the
// WebSocket, where closing on expiry is the wanted behaviour and nothing in
// net/http sets one we did not ask for.
//
// Regressions: TestAnHTTPServerOnTheSessionNetworkAnswersEveryRequest,
// TestAReadDeadlineDoesNotDestroyASessionConnection.
type relayConn struct {
	net.Conn // the WebSocket net.Conn: writes, Close, addresses. Never read directly.
	ws       *websocket.Conn
	source   string

	readq   chan []byte   // one chunk at a time, from pump
	pumped  chan struct{} // closed when pump has stopped
	pumpMu  sync.Mutex    // guards pumpErr against the close of pumped
	pumpErr error
	closed  chan struct{}
	once    sync.Once

	rest []byte // tail of the chunk the last Read did not finish

	dlMu    sync.Mutex
	readDL  time.Time
	dlReset chan struct{} // closed and replaced when readDL changes, to wake a blocked Read
}

func newRelayConn(inner net.Conn, ws *websocket.Conn, source string) *relayConn {
	c := &relayConn{
		Conn: inner, ws: ws, source: source,
		readq:   make(chan []byte),
		pumped:  make(chan struct{}),
		closed:  make(chan struct{}),
		dlReset: make(chan struct{}),
	}
	go c.pump()
	return c
}

// pump is the only reader of the WebSocket, and it never carries a deadline.
// Reading continuously is also what keeps coder/websocket's own ping/pong
// handling running, which is what Ping below depends on.
func (c *relayConn) pump() {
	defer close(c.pumped)
	buf := make([]byte, 32*1024)
	for {
		n, err := c.Conn.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			select {
			case c.readq <- chunk:
			case <-c.closed:
				return
			}
		}
		if err != nil {
			c.pumpMu.Lock()
			c.pumpErr = err
			c.pumpMu.Unlock()
			return
		}
	}
}

func (c *relayConn) Read(p []byte) (int, error) {
	for {
		if len(c.rest) > 0 {
			n := copy(p, c.rest)
			c.rest = c.rest[n:]
			return n, nil
		}
		c.dlMu.Lock()
		dl, reset := c.readDL, c.dlReset
		c.dlMu.Unlock()
		var expired <-chan time.Time
		var timer *time.Timer
		if !dl.IsZero() {
			d := time.Until(dl)
			if d <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(d)
			expired = timer.C
		}
		stop := func() {
			if timer != nil {
				timer.Stop()
			}
		}
		select {
		case chunk := <-c.readq:
			stop()
			c.rest = chunk
		case <-c.pumped:
			stop()
			// Whatever pump managed to hand over before it stopped is still
			// owed to the caller; the error comes after it, as with any conn.
			select {
			case chunk := <-c.readq:
				c.rest = chunk
				continue
			default:
			}
			c.pumpMu.Lock()
			err := c.pumpErr
			c.pumpMu.Unlock()
			if err == nil {
				err = io.EOF
			}
			return 0, err
		case <-expired:
			return 0, os.ErrDeadlineExceeded
		case <-reset:
			// The deadline moved under a blocked read — which is exactly what
			// net/http does — so recompute it rather than sit on the old one.
			stop()
		}
	}
}

// SetReadDeadline is ours; SetWriteDeadline still belongs to the WebSocket.
// SetDeadline is deliberately NOT delegated to the embedded conn, because that
// would put the read half back where the defect was.
func (c *relayConn) SetReadDeadline(t time.Time) error {
	c.dlMu.Lock()
	c.readDL = t
	close(c.dlReset)
	c.dlReset = make(chan struct{})
	c.dlMu.Unlock()
	return nil
}

func (c *relayConn) SetDeadline(t time.Time) error {
	_ = c.SetReadDeadline(t)
	return c.Conn.SetWriteDeadline(t)
}

func (c *relayConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func (c *relayConn) Ping(ctx context.Context) error { return c.ws.Ping(ctx) }

// ClientSource is the resolved client address carried from the upgrade.
func (c *relayConn) ClientSource() string { return c.source }

// pinger is what the relay asserts to find the liveness signal. Keeping it an
// interface means nothing outside this file has to know the transport.
type pinger interface {
	Ping(context.Context) error
}

// acceptRelayConn upgrades an inbound HTTP request into the same net.Conn every
// other side of this package deals in. hops is SLOPBALL_PROXY_HOPS for the
// public relay; a holder's direct listener passes 0 (the peer is the client).
func acceptRelayConn(w http.ResponseWriter, r *http.Request, hops int) (net.Conn, error) {
	// Ticket 16 / #35: refuse a browser Origin, accept its absence. Our Go
	// client sends none — forging one authenticates nothing and would couple
	// versions. Browsers always send one, which is the whole value of the check.
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		http.Error(w, "browser origins are refused", http.StatusForbidden)
		return nil, fmt.Errorf("sessionnet: browser origin refused")
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Still skip the library's origin allow-list: we already refused any
		// Origin above, and an empty Origin must pass.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, err
	}
	src := clientaddr.FromRequest(r, hops)
	return newRelayConn(websocket.NetConn(context.Background(), c, websocket.MessageBinary), c, src), nil
}

// relayHTTPHandler is the HTTP surface both the relay and a holder's direct
// listener present: one upgrade route and a health check.
func relayHTTPHandler(rel *Relay, onConn func(net.Conn)) http.Handler {
	hops := 0
	if rel != nil {
		hops = rel.ProxyHops
	}
	mux := http.NewServeMux()
	// The session is named in the path so an edge can route the upgrade before
	// it forwards it. There is exactly one wire protocol, so a bare /relay/v1
	// dial is refused rather than kept working alongside it.
	mux.HandleFunc(RelayPath+"/{pin}/{service}", func(w http.ResponseWriter, r *http.Request) {
		c, err := acceptRelayConn(w, r, hops)
		if err != nil {
			return
		}
		go onConn(c)
	})
	mux.HandleFunc(RelayPath, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "a relay dial must name the session: "+RelayPath+"/<pin>/<service>\n", http.StatusNotFound)
	})
	mux.HandleFunc(HealthPath, func(w http.ResponseWriter, r *http.Request) {
		if rel != nil && !rel.allowHealth() {
			http.Error(w, "rate-limited\n", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}
