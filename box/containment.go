package box

import (
	"context"
	"fmt"
	"net"
	"strings"
)

// Containment flags for the box container (abuse-surface ticket 22). The box
// runs code the session's agents wrote; these are what bound that blast radius.
// Values are generous enough for npm install + a cold Next start, tight enough
// that a fork bomb or memory leak cannot take the docker host with it.
const (
	boxMemory    = "2g"
	boxCPUs      = "2"
	boxPIDsLimit = "2048"
	// Non-root. Numeric so the image does not need a matching passwd entry;
	// HOME / tmpfs uid match this pair. State lives at $HOME/.slopball
	// (session.Home fallback) — no separate SLOPBALL_HOME emission.
	boxUser    = "1000:1000"
	boxUID     = "1000"
	boxGID     = "1000"
	boxHome    = "/home/slopball"
	boxHomeDir = "/home/slopball/.slopball"
)

// appendContainment writes the ticket-22 docker run flags. --read-only is paired
// with tmpfs mounts so npm/git still have somewhere to write.
func appendContainment(b *strings.Builder, opt Options) {
	fmt.Fprintf(b, " --memory %s", boxMemory)
	fmt.Fprintf(b, " --cpus %s", boxCPUs)
	fmt.Fprintf(b, " --pids-limit %s", boxPIDsLimit)
	b.WriteString(" --read-only")
	b.WriteString(" --cap-drop ALL")
	fmt.Fprintf(b, " --user %s", boxUser)
	fmt.Fprintf(b, " -e HOME=%s", sh(boxHome))
	fmt.Fprintf(b, " -w %s", sh(boxHome))
	// Writable scratch for package managers and git; uid matches --user.
	fmt.Fprintf(b, " --tmpfs /tmp:uid=%s,gid=%s,mode=1777,exec,size=512m", boxUID, boxGID)
	fmt.Fprintf(b, " --tmpfs /run:uid=%s,gid=%s,mode=755,size=64m", boxUID, boxGID)
	if opt.Volume != "" {
		// Writable home parent (for -w) plus the persistent session dir.
		fmt.Fprintf(b, " --tmpfs %s:uid=%s,gid=%s,mode=755,size=16m", sh(boxHome), boxUID, boxGID)
		fmt.Fprintf(b, " -v %s:%s", sh(opt.Volume), sh(boxHomeDir))
	} else {
		fmt.Fprintf(b, " --tmpfs %s:uid=%s,gid=%s,mode=755,exec,size=2g", sh(boxHome), boxUID, boxGID)
	}
}

// ensurePrivateEgressBlocked installs host iptables rules so containers on the
// session bridge cannot reach RFC1918 destinations *or the docker host's own
// subnets* off that bridge — the internet-minus-private-networks policy
// (ticket 22). Same-bridge traffic (control plane, relay sidecar) stays
// allowed. Idempotent.
//
// Applied via a privileged one-shot container: the provisioner has the docker
// group, not host root, and DOCKER-USER lives in the host netns.
func ensurePrivateEgressBlocked(ctx context.Context, r Runner, network string) error {
	iface, err := bridgeIface(ctx, r, network)
	if err != nil {
		return err
	}
	if iface == "" {
		return fmt.Errorf("box: network %q has no bridge interface to attach egress rules to", network)
	}
	if err := validateBridgeIface(iface); err != nil {
		return err
	}
	cidrs := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	hostCIDRs, err := hostOwnSubnets(ctx, r)
	if err != nil {
		return err
	}
	for _, c := range hostCIDRs {
		if !containsCIDR(cidrs, c) {
			cidrs = append(cidrs, c)
		}
	}
	var cidrList strings.Builder
	for i, c := range cidrs {
		if i > 0 {
			cidrList.WriteByte(' ')
		}
		cidrList.WriteString(c)
	}
	// alpine is tiny and widely cached; apk adds iptables for the host netns.
	// iface + cidrs are validated so they are safe to splice into the script.
	script := fmt.Sprintf(`set -e
apk add --no-cache iptables >/dev/null
iptables -nL DOCKER-USER >/dev/null 2>&1 || iptables -N DOCKER-USER
for cidr in %s; do
  iptables -C DOCKER-USER -d "$cidr" -i %s ! -o %s -j DROP 2>/dev/null ||
    iptables -I DOCKER-USER -d "$cidr" -i %s ! -o %s -j DROP
done
`, cidrList.String(), iface, iface, iface, iface)
	// --network host + --privileged: rewrite the host's filter table. --rm so
	// nothing lingers. Image pin is deliberate — floating :latest is how a
	// supply-chain surprise lands on every provision.
	cmd := fmt.Sprintf("docker run --rm --privileged --network host alpine:3.20 sh -c %s", sh(script))
	if out, err := r.Run(ctx, cmd); err != nil {
		return fmt.Errorf("box: block private egress via %s: %w\n%s", iface, err, out)
	}
	return nil
}

// hostOwnSubnets are the docker host's global IPv4 prefixes excluding docker
// bridges — the "docker host's own subnet" half of ticket 22. A public /27 on
// eth0 is included on purpose: that is the co-lo neighbourhood a compromised
// box must not scan.
func hostOwnSubnets(ctx context.Context, r Runner) ([]string, error) {
	out, err := r.Run(ctx, "ip -4 -o addr show scope global")
	if err != nil {
		// No `ip` on the machine (unusual for a docker host) — RFC1918 alone
		// still lands; do not fail the provision over a missing nic list.
		return nil, nil
	}
	var cidrs []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		// ip -o: "2: eth0    inet 38.49.215.51/27 brd …"
		if len(fields) < 4 || fields[2] != "inet" {
			continue
		}
		iface := strings.TrimSuffix(fields[1], ":")
		if isDockerBridgeIface(iface) {
			continue
		}
		cidr := fields[3]
		if _, n, err := net.ParseCIDR(cidr); err != nil {
			continue
		} else if err := validateEgressCIDR(n.String()); err != nil {
			continue
		} else {
			cidrs = append(cidrs, n.String())
		}
	}
	return cidrs, nil
}

func isDockerBridgeIface(name string) bool {
	return name == "docker0" || strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "veth")
}

func validateEgressCIDR(cidr string) error {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}
	s := n.String()
	if strings.ContainsAny(s, " \t\n\"'`$;&|<>(){}") {
		return fmt.Errorf("box: refusing egress cidr %q", cidr)
	}
	return nil
}

func containsCIDR(list []string, c string) bool {
	for _, x := range list {
		if x == c {
			return true
		}
	}
	return false
}

func validateBridgeIface(name string) error {
	if name == "" || strings.ContainsAny(name, " \t\n\"'`$;&|<>(){}") {
		return fmt.Errorf("box: refusing bridge iface %q", name)
	}
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			continue
		}
		return fmt.Errorf("box: refusing bridge iface %q", name)
	}
	return nil
}

// bridgeIface is the linux bridge device docker attached to the named network.
func bridgeIface(ctx context.Context, r Runner, network string) (string, error) {
	// Prefer an explicit bridge name when the network was created with one.
	out, err := r.Run(ctx, fmt.Sprintf(
		"docker network inspect %s -f '{{index .Options \"com.docker.network.bridge.name\"}}'", sh(network)))
	if err == nil {
		if name := strings.TrimSpace(out); name != "" {
			return name, nil
		}
	}
	out, err = r.Run(ctx, fmt.Sprintf("docker network inspect %s -f '{{.Id}}'", sh(network)))
	if err != nil {
		return "", fmt.Errorf("inspect network %s: %w", network, err)
	}
	id := strings.TrimSpace(out)
	if len(id) < 12 {
		return "", fmt.Errorf("network %s has a short id %q", network, id)
	}
	return "br-" + id[:12], nil
}
