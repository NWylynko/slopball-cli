package sessionnet

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/slopball-cli/logx"
)

// HolderConfig describes the service a lease holder publishes onto the session
// network. It is the server half, and it makes only outbound connections.
type HolderConfig struct {
	Relay   string // relay dial address
	PIN     string
	Service string // git | dev | logs
	Key     Key

	// DirectListener, when set, is a SECOND way in: an ordinary listener on a
	// machine address, speaking the identical greeting and the identical
	// handshake. It exists so a teammate who can actually reach this machine —
	// same LAN, same hotspot, same box — does not pay for a round trip through
	// a relay. Clients learn the address from the session record and fall back
	// to the relay when it does not answer.
	//
	// The caller owns the bind (see internal/netbind), which keeps this package
	// free of any opinion about which interface a session should be on.
	DirectListener net.Listener
	// DirectAdvertise is the host:port peers should dial for DirectListener.
	// Empty means the listener's own address, which is right on a LAN and wrong
	// behind docker's port publishing.
	DirectAdvertise string

	// DialContext overrides how outbound sockets are opened. Tests use it to
	// sever a link the way a carrier handover does; production leaves it nil.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)

	// Ticket returns a current Ed25519 relay ticket for PIN/Service. Required
	// when the relay has TicketPublic configured; refreshed on reconnect so a
	// control-plane outage does not strand a holder whose ticket is still valid.
	Ticket func() (string, error)
}

// DirectAddr reports the machine address a holder listener publishes for direct
// dials, or "" when it has none. It is what the host writes into the session's
// endpoint record.
func DirectAddr(ln net.Listener) string {
	d, ok := ln.(interface{ DirectAddr() string })
	if !ok {
		return ""
	}
	return d.DirectAddr()
}

// ErrHolderBusy is the relay's "busy": another holder of this service is
// registered and still answers ping. It is the one refusal a caller may
// legitimately wait out — a takeover has demoted the incumbent and it stands
// down on its next member cycle — where every other refusal is a
// configuration to fix.
var ErrHolderBusy = errors.New("busy — the previous holder is still registered")

// Serve publishes Service on the session network and returns an ordinary
// net.Listener whose accepted connections are already decrypted and
// authenticated. Hand it to http.Serve, the git server, anything.
//
// The registration is maintained for the life of ctx: a relay restart, a dropped
// tunnel or a hostile network reconnects underneath without the caller noticing,
// which is what makes this survivable on hackathon wifi (§3.4).
func Serve(ctx context.Context, cfg HolderConfig) (net.Listener, error) {
	if cfg.Relay == "" {
		return nil, errors.New("sessionnet: no relay address")
	}
	if cfg.PIN == "" || cfg.Service == "" {
		return nil, errors.New("sessionnet: pin and service required")
	}
	if cfg.Key.Zero() {
		return nil, fmt.Errorf("sessionnet: serving %s needs the session key", cfg.Service)
	}
	ctx, cancel := context.WithCancel(ctx)
	l := &holderListener{cfg: cfg, conns: make(chan net.Conn), closed: make(chan struct{}), cancel: cancel}

	// The first registration is synchronous so a relay that is not there is an
	// error the caller sees, not a silent retry loop behind a listener that
	// never accepts anything.
	ctrl, err := l.registerOnce(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	go l.run(ctx, ctrl)
	if cfg.DirectListener != nil {
		l.serveDirect(ctx)
	}
	return l, nil
}

type holderListener struct {
	cfg    HolderConfig
	conns  chan net.Conn
	closed chan struct{}
	cancel context.CancelFunc
	once   sync.Once
	// br is the current control connection's reader — the relay's accept
	// notifications arrive on it. Only registerOnce and run touch it, and they
	// strictly alternate, so it needs no lock.
	br *bufio.Reader

	mu   sync.Mutex
	ctrl net.Conn // current control connection, closed by Close to unblock run
}

func (l *holderListener) registerOnce(ctx context.Context) (net.Conn, error) {
	c, err := dialRelay(ctx, l.cfg.Relay, l.cfg.PIN, l.cfg.Service, l.cfg.DialContext)
	if err != nil {
		return nil, err
	}
	ticket := ""
	if l.cfg.Ticket != nil {
		ticket, err = l.cfg.Ticket()
		if err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("sessionnet: relay ticket for %s: %w", l.cfg.Service, err)
		}
	}
	line := fmt.Sprintf("%s register %s %s", relayProto, l.cfg.PIN, l.cfg.Service)
	if ticket != "" {
		line += " " + ticket
	}
	if _, err := fmt.Fprintf(c, "%s\n", line); err != nil {
		_ = c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	reply, err := br.ReadString('\n')
	if err != nil || strings.TrimSpace(reply) != "ok" {
		_ = c.Close()
		got := strings.TrimSpace(reply)
		if got == "" {
			got = fmt.Sprint(err)
		}
		if got == "busy" {
			return nil, fmt.Errorf("sessionnet: relay refused registration of %s: %w", l.cfg.Service, ErrHolderBusy)
		}
		return nil, fmt.Errorf("sessionnet: relay refused registration of %s: %s", l.cfg.Service, got)
	}
	_ = c.SetReadDeadline(time.Time{})
	l.br = br
	l.mu.Lock()
	l.ctrl = c
	l.mu.Unlock()
	return c, nil
}

func (l *holderListener) run(ctx context.Context, ctrl net.Conn) {
	br := l.br
	for {
		if err := l.pump(ctx, ctrl, br); err != nil && ctx.Err() == nil {
			logx.New("sessionnet").Debugf("%s: %s tunnel dropped (%v) — reconnecting", l.cfg.PIN, l.cfg.Service, err)
		}
		_ = ctrl.Close()
		if ctx.Err() != nil {
			return
		}
		// Reconnect with a bounded backoff. This is a network retry, not a
		// race being papered over: the relay may legitimately be restarting.
		var err error
		for delay := 200 * time.Millisecond; ; {
			ctrl, err = l.registerOnce(ctx)
			if err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			if delay < 5*time.Second {
				delay *= 2
			}
		}
		br = l.br
		logx.New("sessionnet").Infof("%s: %s tunnel re-registered", l.cfg.PIN, l.cfg.Service)
	}
}

// watchCtrl is the holder's half of step 5's liveness, and it is needed for the
// same reason the relay's half is: on a link that went quiet rather than
// closing, the read below never returns an error, so without a ping the holder
// sits happily on a tunnel that has already been evicted and never reconnects.
// Both ends ping the same connection because both ends have to act.
func (l *holderListener) watchCtrl(ctx context.Context, ctrl net.Conn) chan struct{} {
	stop := make(chan struct{})
	p, ok := ctrl.(pinger)
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
			case <-ctx.Done():
				return
			case <-t.C:
			}
			pctx, cancel := context.WithTimeout(ctx, holderPingGrace)
			err := p.Ping(pctx)
			cancel()
			if err == nil {
				continue
			}
			select {
			case <-stop:
				return
			default:
			}
			logx.New("sessionnet").Warnf("%s: %s tunnel went quiet (%v) — dropping it to re-register", l.cfg.PIN, l.cfg.Service, err)
			// Closing unblocks pump, which routes into the one reconnect path.
			_ = ctrl.Close()
			return
		}
	}()
	return stop
}

// pump reads accept notifications until the control connection dies.
func (l *holderListener) pump(ctx context.Context, ctrl net.Conn, br *bufio.Reader) error {
	stop := l.watchCtrl(ctx, ctrl)
	defer close(stop)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) != 2 || f[0] != "accept" {
			continue
		}
		go l.openData(ctx, f[1])
	}
}

// openData answers one accept notification with a fresh outbound connection,
// completes the handshake as responder, and hands the plaintext conn to Accept.
func (l *holderListener) openData(ctx context.Context, stream string) {
	c, err := dialRelay(ctx, l.cfg.Relay, l.cfg.PIN, l.cfg.Service, l.cfg.DialContext)
	if err != nil {
		return
	}
	if _, err := fmt.Fprintf(c, "%s accept %s %s %s\n", relayProto, l.cfg.PIN, l.cfg.Service, stream); err != nil {
		_ = c.Close()
		return
	}
	sec, err := handshake(c, l.cfg.Key, false)
	if err != nil {
		logx.New("sessionnet").Warnf("%s: refused a %s connection: %v", l.cfg.PIN, l.cfg.Service, err)
		_ = c.Close()
		return
	}
	select {
	case l.conns <- sec:
	case <-ctx.Done():
		_ = sec.Close()
	case <-l.closed:
		_ = sec.Close()
	}
}

func (l *holderListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

// Close unblocks Accept immediately AND kills the control connection. Both
// halves matter: an http.Server shutting down waits for Serve to return, and
// Serve only returns when Accept errors — so a Close that merely cancelled the
// context would hang the caller's shutdown while the reconnect loop sat in a
// blocking read.
func (l *holderListener) Close() error {
	l.once.Do(func() {
		l.cancel()
		close(l.closed)
		l.mu.Lock()
		ctrl := l.ctrl
		l.mu.Unlock()
		if ctrl != nil {
			_ = ctrl.Close()
		}
	})
	return nil
}

func (l *holderListener) Addr() net.Addr { return sessionAddr{pin: l.cfg.PIN, service: l.cfg.Service} }

// DirectAddr is the machine address peers may dial to skip the relay.
func (l *holderListener) DirectAddr() string {
	if l.cfg.DirectListener == nil {
		return ""
	}
	if l.cfg.DirectAdvertise != "" {
		return l.cfg.DirectAdvertise
	}
	return l.cfg.DirectListener.Addr().String()
}

// serveDirect answers dials that reached this machine without the relay. It is
// the same transport (WebSocket, so one ingress story), the same greeting and
// the same handshake — the only difference is which socket the bytes arrived
// on, which is exactly what "an optimisation, not the mechanism" has to mean.
func (l *holderListener) serveDirect(ctx context.Context) {
	srv := &http.Server{Handler: relayHTTPHandler(nil, func(c net.Conn) { l.handleDirect(ctx, c) })}
	go func() { _ = srv.Serve(l.cfg.DirectListener) }()
	go func() {
		select {
		case <-ctx.Done():
		case <-l.closed:
		}
		_ = srv.Close()
	}()
	logx.New("sessionnet").Infof("%s: %s also reachable directly at %s", l.cfg.PIN, l.cfg.Service, l.DirectAddr())
}

func (l *holderListener) handleDirect(ctx context.Context, c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		_ = c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	f := strings.Fields(strings.TrimSpace(line))
	if len(f) != 4 || f[0] != relayProto || f[1] != "direct" {
		_ = c.Close()
		return
	}
	// A greeting naming a session or service this holder does not serve gets
	// the SAME answer as a peer with the wrong key: `ok`, a handshake, a
	// refusal. Closing here instead used to be a pre-handshake oracle — this
	// listener has no ticket gate and no limiter, and it binds a LAN address
	// whenever the control plane is remote, so a stranger on the venue wifi
	// could confirm "this machine holds PIN X" at LAN speed. The decoy key is
	// fresh per connection and the handshake fails at key confirmation exactly
	// as a wrong session key does; nothing about the answer depends on the pin.
	key, held := l.cfg.Key, f[2] == l.cfg.PIN && f[3] == l.cfg.Service
	if !held {
		key = NewKey()
	}
	if _, err := io.WriteString(c, "ok\n"); err != nil {
		_ = c.Close()
		return
	}
	sec, err := handshake(&prefixedConn{Conn: c, buf: br}, key, false)
	if err != nil || !held {
		if !held {
			logx.New("sessionnet").Warnf("%s: refused a direct connection naming %s/%s, which this holder does not serve", l.cfg.PIN, f[2], f[3])
		} else {
			logx.New("sessionnet").Warnf("%s: refused a direct %s connection: %v", l.cfg.PIN, l.cfg.Service, err)
		}
		if sec != nil {
			_ = sec.Close()
		}
		_ = c.Close()
		return
	}
	select {
	case l.conns <- sec:
	case <-ctx.Done():
		_ = sec.Close()
	case <-l.closed:
		_ = sec.Close()
	}
}

type sessionAddr struct{ pin, service string }

func (a sessionAddr) Network() string { return "slopball" }
func (a sessionAddr) String() string  { return a.pin + "/" + a.service }
