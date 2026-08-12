package devserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"

	"github.com/nwylynko/slopball-cli/logx"
	"github.com/nwylynko/slopball-cli/sessionnet"
)

// HolderOptions publishes the dev server onto the session network (plan 41).
//
// git joined the session network and dev never did, which is the whole bug this
// exists to fix: a member who joined from another network synced fine and then
// could not open the page, because `runtime.announceDev` published a machine
// address nothing outside that network could route.
type HolderOptions struct {
	Relay string
	PIN   string
	Key   sessionnet.Key
	// LocalPort is the port the supervised dev process is actually listening on
	// *here*. On a box that is not the published port — the container binds its
	// own netns's port and docker forwards a different one — so it is resolved
	// explicitly and never guessed.
	LocalPort int
	// DirectListener / DirectAdvertise give a routable holder the same
	// no-relay-hop shortcut the git server has.
	DirectListener  net.Listener
	DirectAdvertise string
	// Ticket returns a current relay ticket for the dev service (ticket 17).
	Ticket func() (string, error)
}

// Holder is a live dev-server publication. Its lifetime rides the dev LEASE,
// not the process: a holder still registered for a service this member no
// longer runs is the same lie as a held lease for a service it does not serve.
type Holder struct {
	ln     net.Listener
	url    string
	direct string
	cancel context.CancelFunc
	once   sync.Once

	mu    sync.Mutex
	conns map[net.Conn]struct{} // live splices, torn down when publication ends
}

// StartHolder publishes the dev server and splices session-network connections
// into it.
//
// It is a byte-level TCP splice rather than `srv.Serve(sln)` the way the git
// server does it, and the difference is not stylistic: slopball *is* the git
// server, whereas the dev server is somebody else's child process. Splicing is
// also what makes websocket upgrades — HMR — ride it for free, since nothing in
// the path parses HTTP at all.
func StartHolder(ctx context.Context, opt HolderOptions) (*Holder, error) {
	if opt.Relay == "" {
		return nil, errors.New("devserver: no relay address")
	}
	if opt.LocalPort <= 0 {
		return nil, errors.New("devserver: no local dev port to publish")
	}
	ctx, cancel := context.WithCancel(ctx)
	ln, err := sessionnet.Serve(ctx, sessionnet.HolderConfig{
		Relay: opt.Relay, PIN: opt.PIN, Service: "dev", Key: opt.Key,
		DirectListener: opt.DirectListener, DirectAdvertise: opt.DirectAdvertise,
		Ticket: opt.Ticket,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("publish the dev server onto the session network: %w", err)
	}
	h := &Holder{
		ln: ln, cancel: cancel, conns: map[net.Conn]struct{}{},
		url:    sessionnet.FormatURL(opt.PIN, "dev", ""),
		direct: sessionnet.DirectAddr(ln),
	}
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(opt.LocalPort))
	go h.accept(ctx, target)
	logx.New("dev").Infof("dev server published on the session network: %s → %s", h.url, target)
	return h, nil
}

// URL is the session address to publish as the dev endpoint. It names the
// session's dev SERVICE, so it stays true when the dev lease migrates.
func (h *Holder) URL() string { return h.url }

// Direct is the machine address peers may dial to skip the relay hop, or "".
func (h *Holder) Direct() string { return h.direct }

// Close stops publishing.
func (h *Holder) Close() error {
	if h == nil {
		return nil
	}
	h.once.Do(func() {
		h.cancel()
		_ = h.ln.Close()
		// Tear the live splices down too. A member that has lost the dev lease
		// must stop serving, not keep answering whoever already had a
		// keep-alive connection open — that is the same lie as a stale
		// registration, just harder to see.
		h.mu.Lock()
		for c := range h.conns {
			_ = c.Close()
		}
		h.conns = map[net.Conn]struct{}{}
		h.mu.Unlock()
	})
	return nil
}

func (h *Holder) accept(ctx context.Context, target string) {
	log := logx.New("dev")
	for {
		c, err := h.ln.Accept()
		if err != nil {
			return
		}
		h.mu.Lock()
		h.conns[c] = struct{}{}
		h.mu.Unlock()
		go func() {
			defer func() {
				h.mu.Lock()
				delete(h.conns, c)
				h.mu.Unlock()
				c.Close()
			}()
			var d net.Dialer
			up, err := d.DialContext(ctx, "tcp", target)
			if err != nil {
				log.Warnf("dev server at %s did not answer a session-network request: %v", target, err)
				return
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
			<-done
		}()
	}
}
