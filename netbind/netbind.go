// Package netbind decides what interface the *direct* session-network listener
// binds, and what host it advertises. The plain unauthenticated git HTTP
// listener is always loopback (see gitserver) — Bind / BindForControl never
// open that door. This package is the seam that lets a session publish a
// handshake-gated machine address from "loopback, this machine only" to
// "reachable by other devices on the same LAN or Tailscale mesh" with no
// external infrastructure and no operator knob.
//
// Two modes only, and BindForControl is the sole producer:
//
//	"" (loopback)  127.0.0.1 only — and no direct is published
//	"auto"         first routable address: Tailscale, else LAN
//
// "auto" is what BindForControl picks when the control plane is on another
// machine: peers who will dial the direct address are provably elsewhere, so
// loopback would strand every one of them (and a loopback direct is never
// published anyway — see hoststart.directIsPublishable). Same-LAN and same-
// Tailscale reachability is what auto detection returns, not something anyone
// configures.
package netbind

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// AdvertiseNone is what a provisioner sets SLOPBALL_ADVERTISE to when this
// machine has NO address a peer could dial — not a loopback one, none at all.
//
// It exists for a Cloudflare Container, which is reachable only outbound through
// the session Durable Object. Auto-detection there finds a perfectly real 10.x
// interface inside Cloudflare's network that is reachable by nobody, and the
// container cannot tell the difference — the same reason the docker box is
// HANDED the address it advertises instead of guessing one. Unset keeps meaning
// "work it out", which is every laptop.
const AdvertiseNone = "none"

// DirectSuppressed reports that the provisioner said there is no dialable
// address here, so no direct listener should be opened or published.
func DirectSuppressed() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("SLOPBALL_ADVERTISE")), AdvertiseNone)
}

// Advertise returns SLOPBALL_ADVERTISE: the address a listener should publish
// regardless of what it binds. Set for a container in its own network
// namespace, where the only address the process can see is a bridge IP that
// resolves for nobody, and traffic arrives via docker's port forwarding.
//
// AdvertiseNone is not an address, so it reads as absent here; the callers that
// have to act on it ask DirectSuppressed.
func Advertise() string {
	adv := strings.TrimSpace(os.Getenv("SLOPBALL_ADVERTISE"))
	if strings.EqualFold(adv, AdvertiseNone) {
		return ""
	}
	return adv
}

// ListenAdvertise is ListenMode with the bind address and the advertised
// address allowed to differ, and with an optional fixed port.
//
// With SLOPBALL_ADVERTISE set it binds 0.0.0.0 — docker forwards to the
// wildcard, so anything narrower is unreachable — and advertises the address it
// was handed. port pins the listener (needed when the box publishes it 1:1 and
// the container has to bind the number it will advertise); 0 stays ephemeral.
// Without the override this is exactly ListenMode.
func ListenAdvertise(mode string, port int) (net.Listener, string, error) {
	adv := Advertise()
	if adv == "" {
		if port == 0 {
			return ListenMode(mode)
		}
		ln, host, err := ListenMode(mode)
		if err != nil || ln == nil {
			return ln, host, err
		}
		// A fixed port with no override still has to honour the bind mode, so
		// rebind on the same interface at the requested port.
		bindIP := ln.Addr().(*net.TCPAddr).IP.String()
		_ = ln.Close()
		fixed, err := net.Listen("tcp", net.JoinHostPort(bindIP, strconv.Itoa(port)))
		return fixed, host, err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		return nil, "", fmt.Errorf("SLOPBALL_ADVERTISE=%s: binding 0.0.0.0:%d: %w", adv, port, err)
	}
	return ln, adv, nil
}

// ListenMode opens a TCP listener for the given mode and returns it alongside
// the host that should appear in advertised URLs (so peers dial a reachable
// address, never 0.0.0.0). Two branches only: routable ("auto") and loopback
// (empty / "loopback"). Port is always ephemeral; read it from ln.Addr().
func ListenMode(mode string) (net.Listener, string, error) {
	switch m := strings.ToLower(strings.TrimSpace(mode)); m {
	case "auto":
		ip, err := RoutableIP()
		if err != nil {
			return nil, "", err
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
		return ln, ip, err
	case "", "loopback", "local", "127.0.0.1":
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		return ln, "127.0.0.1", err
	default:
		return nil, "", fmt.Errorf("bind mode %q is not 'auto' or loopback", m)
	}
}

// AdvertiseHostMode returns the host part ListenMode would advertise, without
// opening a socket — for callers that need to know the address up front.
func AdvertiseHostMode(mode string) (string, error) {
	switch m := strings.ToLower(strings.TrimSpace(mode)); m {
	case "auto":
		return RoutableIP()
	case "", "loopback", "local", "127.0.0.1":
		return "127.0.0.1", nil
	default:
		return "", fmt.Errorf("bind mode %q is not 'auto' or loopback", m)
	}
}

// BindForControl is the sole producer of a direct-listener mode: a control
// plane on ANOTHER machine means peers need a routable address, so return
// "auto"; a local (or unknown) control plane keeps the safe loopback default
// — which means no direct address is published (a loopback direct is worse
// than none). The plain git HTTP listener ignores this and always binds
// loopback. Returns "" when there is nothing routable to offer — the caller
// should say so out loud.
func BindForControl(controlBase string) string {
	if strings.TrimSpace(controlBase) == "" || LoopbackURL(controlBase) {
		return ""
	}
	if _, err := RoutableIP(); err != nil {
		return ""
	}
	return "auto"
}

// RoutableIP returns the best address another machine could dial: the Tailscale
// address first (it works across networks and NATs, which is the whole reason a
// session uses a shared control plane), else the LAN IP. Both failing means
// there is genuinely nowhere to publish, and the caller must say so rather than
// quietly advertising loopback.
func RoutableIP() (string, error) {
	if ip, err := TailscaleIP(); err == nil {
		return ip, nil
	}
	ip, err := LANIP()
	if err != nil {
		return "", fmt.Errorf("no routable address (no Tailscale interface and %w)", err)
	}
	return ip, nil
}

// LoopbackURL reports whether raw addresses only the machine that produced it
// can reach. Publishing such a URL into a control plane other machines read is
// the classic strand-every-joiner bug: they dial their OWN localhost and get
// "connection refused".
func LoopbackURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "" {
		h = strings.Trim(raw, "[]")
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return false
}

// TailscaleIP returns this machine's Tailscale IPv4 — the address in the
// 100.64.0.0/10 CGNAT range Tailscale assigns (usually on interface
// tailscale0). BindForControl's "auto" prefers this so a session on a mesh
// publishes an address that works across networks without hardcoding it or
// accidentally advertising a public NIC.
func TailscaleIP() (string, error) {
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil && cgnat.Contains(v4) {
			return v4.String(), nil
		}
	}
	return "", fmt.Errorf("no Tailscale address (100.64.0.0/10) on any interface — is tailscale up?")
}

// LANIP returns this machine's primary outbound (LAN) IPv4. It opens a UDP
// socket "to" a public address to learn which local interface the OS would
// route through — no packets are sent and no connectivity is required, so it
// works on an isolated LAN with no internet.
func LANIP() (string, error) {
	c, err := net.Dial("udp", "203.0.113.1:9") // TEST-NET-3; unroutable, never contacted
	if err == nil {
		defer c.Close()
		if ua, ok := c.LocalAddr().(*net.UDPAddr); ok && ua.IP != nil && !ua.IP.IsLoopback() {
			return ua.IP.String(), nil
		}
	}
	// Fallback: first non-loopback IPv4 across interfaces.
	addrs, aerr := net.InterfaceAddrs()
	if aerr != nil {
		return "", aerr
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if v4 := ipnet.IP.To4(); v4 != nil {
				return v4.String(), nil
			}
		}
	}
	return "", fmt.Errorf("no non-loopback IPv4 interface")
}
