package sessionnet

import (
	"context"
	"fmt"
	"hash/fnv"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// Scheme is how a session address is written down. `slop://<pin>/<service>/<path>`
// says *which session service* to reach and says nothing about which machine is
// holding it — which is the whole point: the git lease can move to another
// member mid-session and every published URL stays correct.
//
// It is also what the control plane stores as the git endpoint, so the endpoint
// record stops being an IP:port that was true when it was written.
const Scheme = "slop"

// FormatURL builds a session address. path is the trailing resource, e.g.
// "canonical.git" for the session git server.
func FormatURL(pin, service, path string) string {
	u := Scheme + "://" + pin + "/" + service
	if p := strings.TrimPrefix(path, "/"); p != "" {
		u += "/" + p
	}
	return u
}

// IsSessionURL reports whether raw is a session address rather than a machine
// address. Both exist during the transition: a session with no relay still
// publishes a plain http:// endpoint.
func IsSessionURL(raw string) bool {
	return strings.HasPrefix(raw, Scheme+"://")
}

// ParseURL splits a session address into pin, service and trailing path.
func ParseURL(raw string) (pin, service, path string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", err
	}
	if u.Scheme != Scheme {
		return "", "", "", fmt.Errorf("sessionnet: %q is not a %s:// address", raw, Scheme)
	}
	pin = u.Host
	parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 2)
	if pin == "" || parts[0] == "" {
		return "", "", "", fmt.Errorf("sessionnet: %q names no session service", raw)
	}
	service = parts[0]
	if len(parts) == 2 {
		path = parts[1]
	}
	return pin, service, path, nil
}

var (
	fwdMu    sync.Mutex
	fwdCache = map[string]*Forwarder{}
)

// LocalURL turns a session address into an ordinary http:// URL local tools can
// use, standing up (and reusing) one loopback forwarder per session service in
// this process. git, curl and a browser all get a normal URL and never learn
// that the bytes went through a relay.
//
// The forwarder is process-lived on purpose: the address it hands out is
// written into git's `origin`, and a URL that stopped working when some caller
// happened to return would be worse than no forwarder at all.
// direct, when non-nil, resolves a service's directly-published machine address
// so a reachable holder is dialed without the relay hop.
func LocalURL(ctx context.Context, raw, relay string, key Key, direct func(service string) string, ticket func(service string) (string, error)) (string, error) {
	pin, service, path, err := ParseURL(raw)
	if err != nil {
		return "", err
	}
	if key.Zero() {
		return "", fmt.Errorf("sessionnet: %s needs the session key — resolve the PIN through the control plane", raw)
	}
	cacheKey := pin + "/" + service
	fwdMu.Lock()
	f, ok := fwdCache[cacheKey]
	fwdMu.Unlock()
	if !ok {
		// context.WithoutCancel: the forwarder outlives whatever request first
		// asked for it, for the reason above.
		nf, err := forwardFor(context.WithoutCancel(ctx), pin, service, relay, key, direct, ticket)
		if err != nil {
			return "", err
		}
		fwdMu.Lock()
		if existing, raced := fwdCache[cacheKey]; raced {
			fwdMu.Unlock()
			_ = nf.Close()
			f = existing
		} else {
			fwdCache[cacheKey] = nf
			fwdMu.Unlock()
			f = nf
		}
	}
	out := "http://" + f.LocalHost()
	if path != "" {
		out += "/" + path
	}
	return out, nil
}

// localDomain is a name a human can read, on a domain that costs nothing:
// *.localhost is reserved by RFC 6761 and resolves to loopback with no DNS
// lookup, no /etc/hosts line and no root. A bare `slopball` host would need a
// sudo'd hosts entry — the single-binary-no-root promise traded for one dot.
const localDomain = "slopball.localhost"

// DevLocalHost is the hostname a member opens the session's dev server on.
// Per-PIN so two concurrent sessions stay distinct.
func DevLocalHost(pin string) string { return pin + "." + localDomain }

// devPortAttempts bounds the search for a free stable port.
const devPortAttempts = 8

// devLocalPort derives this member's local forwarder port from the PIN. Derived
// rather than allocated so a human-facing URL survives a join-daemon restart.
func devLocalPort(pin string, attempt int) int {
	h := fnv.New32a()
	_, _ = fmt.Fprintf(h, "devlocal:%s:%d", pin, attempt)
	return 20000 + int(h.Sum32()%40000)
}

// forwardFor stands up the loopback forwarder for one session service.
//
// git keeps an ephemeral 127.0.0.1 port: nothing reads it but git's `origin`.
// dev is the one a human reads and clicks, so it gets a stable derived port,
// both loopback stacks, and a name.
func forwardFor(ctx context.Context, pin, service, relay string, key Key, direct func(string) string, ticket func(string) (string, error)) (*Forwarder, error) {
	cfg := ForwarderConfig{Relay: relay, PIN: pin, Service: service, Key: key, Direct: direct, Ticket: ticket}
	if service != "dev" {
		return Forward(ctx, cfg)
	}
	var err error
	for attempt := 0; attempt < devPortAttempts; attempt++ {
		port := devLocalPort(pin, attempt)
		cfg.Addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		cfg.AlsoAddr = []string{net.JoinHostPort("::1", strconv.Itoa(port))}
		cfg.LocalHost = net.JoinHostPort(DevLocalHost(pin), strconv.Itoa(port))
		var f *Forwarder
		if f, err = Forward(ctx, cfg); err == nil {
			return f, nil
		}
	}
	return nil, fmt.Errorf("sessionnet: no free local port for %s's dev server: %w", pin, err)
}

// CloseForwarders drops every forwarder this process holds. Leaving a session
// is the only caller — the addresses stop being valid at that moment anyway.
func CloseForwarders() {
	fwdMu.Lock()
	all := fwdCache
	fwdCache = map[string]*Forwarder{}
	fwdMu.Unlock()
	for _, f := range all {
		_ = f.Close()
	}
}
