package box

import (
	"context"
	"fmt"
	"net"
	"strings"
)

// AdvertiseIP asks a machine for the address its containers should advertise. A
// container in its own netns sees a bridge IP (172.17.x) that is reachable from
// nowhere, so the routable address has to come from the machine hosting it —
// Tailscale first, then the primary LAN address.
//
// It is exported because the answer is worth resolving BEFORE a provision starts
// (cloudbox.Docker does): "this control plane cannot name an address for its
// boxes" is a fact about the machine, and finding it out halfway through booting
// a container someone already asked for makes it look like the box's fault.
//
// Nothing here pipes into another command, deliberately. It used to run
//
//	hostname -I 2>/dev/null | awk '{print $1}'
//
// and a pipeline exits with the status of its LAST command — so on alpine, whose
// busybox `hostname` has no -I at all, awk succeeded on empty input, the failure
// was reported as exit 0, and `2>/dev/null` had already discarded the one line
// that said what was wrong. What surfaced was "it reported none", which is not
// what the machine said. Run the command plainly and parse in Go.
func AdvertiseIP(ctx context.Context, r Runner) (string, error) {
	if out, err := r.Run(ctx, "tailscale ip -4"); err == nil {
		if ip := firstRoutableIPv4(out); ip != "" {
			return ip, nil
		}
	}
	out, err := r.Run(ctx, "hostname -I")
	if ip := firstRoutableIPv4(out); ip != "" {
		return ip, nil
	}
	said := strings.TrimSpace(firstLine(out))
	switch {
	case said != "":
		return "", fmt.Errorf("could not determine %s's routable address: `hostname -I` said %q",
			r.Target(), said)
	case err != nil:
		return "", fmt.Errorf("could not determine %s's routable address: `hostname -I`: %w",
			r.Target(), err)
	default:
		return "", fmt.Errorf("could not determine %s's routable address: `hostname -I` listed none "+
			"(no Tailscale address either)", r.Target())
	}
}

// firstRoutableIPv4 picks what `awk '{print $1}'` used to pick — the first
// address — but only from the ones a peer could actually dial. `hostname -I`
// mixes in link-local v6 on a dual-stack host, and loopback means "this machine"
// to every machine that is not this one.
func firstRoutableIPv4(out string) string {
	for _, f := range strings.Fields(out) {
		ip := net.ParseIP(strings.SplitN(f, "%", 2)[0])
		if ip == nil || ip.To4() == nil {
			continue
		}
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			continue
		}
		return ip.String()
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
