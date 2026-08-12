// Package clientaddr resolves the client address both services key quotas on
// (plan 45 step 0 / ADR 0005). The subject is configuration, never inference:
// SLOPBALL_PROXY_HOPS says how many reverse-proxy hops sit in front, and
// X-Forwarded-For is read from the right — or ignored entirely at the default
// of zero.
package clientaddr

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/nwylynko/slopball-cli/logx"
)

// EnvHops is the environment variable both services read.
const EnvHops = "SLOPBALL_PROXY_HOPS"

// HopsFromEnv returns SLOPBALL_PROXY_HOPS, defaulting to 0. A missing or
// unparseable value is zero (peer socket only) — fail closed, never open.
func HopsFromEnv() int {
	n, _ := hopsFromEnv()
	return n
}

// Configured reports whether SLOPBALL_PROXY_HOPS was SET, as distinct from
// being zero.
//
// The distinction is the whole point on a new platform. Zero is a real answer —
// "nothing is in front of me, count the peer socket" — and it is the right
// answer for a laptop. Unset on a deployment behind an edge and a worker is not
// an answer at all: it is a value nobody has measured, and every quota then
// keys on whatever address the last hop happens to present. That is how the
// entire internet lands in one bucket, which is precisely the failure plan 45's
// counters exist to prevent.
//
// So the deployment says so out loud at boot rather than reporting a number it
// has no grounds for. The measurement itself needs the real chain (ADR 0005's
// method, phase 3 ticket 03) and cannot be done from here.
func Configured() bool {
	_, ok := hopsFromEnv()
	return ok
}

func hopsFromEnv() (int, bool) {
	n, err := ParseHops(os.Getenv(EnvHops))
	if err != nil {
		return 0, false
	}
	return n, true
}

// ErrHopsUnset is what ParseHops returns for an empty value, so a caller can
// tell "nobody said" from "somebody said something wrong" and word its refusal
// accordingly. Both are refusals on a deployment; only one is a typo.
var ErrHopsUnset = errors.New(EnvHops + " is unset")

// ParseHops validates a hop count as a VALUE, separately from reading the
// environment, so a service can refuse at boot instead of degrading to zero.
//
// Why this is exported at all: HopsFromEnv answers 0 for "unset" and 0 for
// "garbage" alike, which is the correct behaviour for a running process (fail
// closed, never open) and a terrible one for a deployment. On 2026-08-10 the
// deployed control plane counted every request in one bucket for exactly this
// reason, and nothing anywhere refused. Boot validation needs the distinction
// that Resolve deliberately throws away.
func ParseHops(raw string) (int, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return 0, ErrHopsUnset
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a whole number", EnvHops, v)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s=%q is negative; a hop count cannot be", EnvHops, v)
	}
	return n, nil
}

// PeerHost extracts the host from a net.Addr or RemoteAddr string.
func PeerHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// Resolve picks the client address given the TCP peer, the raw X-Forwarded-For
// header value, and the configured hop count.
//
// hops == 0: peer only; the header is ignored (default — safe behind nothing).
// hops == N: the Nth element from the right of the chain. A chain shorter than
// N falls back to the peer (fail closed: shared bucket, not minted sources).
func Resolve(peer, xff string, hops int) string {
	peer = strings.TrimSpace(peer)
	if peer == "" {
		peer = "unknown"
	}
	if hops <= 0 {
		return peer
	}
	parts := splitXFF(xff)
	if len(parts) == 0 || hops > len(parts) {
		// Fail closed — and say so. This branch means the configured hop count
		// does not match the chain actually arriving, so every request landing
		// here shares ONE bucket: the proxy's own address. That is a quota that
		// looks like it is working and is not, which is worse than no quota,
		// so it is counted and the first one is loud.
		if n := shortChains.Add(1); n == 1 {
			logx.New("clientaddr").Warnf(
				"%s=%d but the X-Forwarded-For chain arriving has %d entr(ies) — "+
					"falling back to the peer socket, which buckets every client behind that proxy "+
					"together. Measure the real chain (ADR 0005) and set the right value; "+
					"until then this deployment's quota counters cannot be trusted",
				EnvHops, hops, len(parts))
		}
		return peer
	}
	return parts[len(parts)-hops]
}

// ShortChains is how many requests hit the fallback above. Non-zero on a
// deployment means the configured hop count is wrong — it is the observable a
// wrong value produces, since a wrong value otherwise just quietly works.
func ShortChains() int64 { return shortChains.Load() }

var shortChains atomic.Int64

// FromRequest is Resolve against an HTTP request's peer and X-Forwarded-For.
func FromRequest(r *http.Request, hops int) string {
	if r == nil {
		return "unknown"
	}
	return Resolve(PeerHost(r.RemoteAddr), r.Header.Get("X-Forwarded-For"), hops)
}

// Describe is the one-line startup log both services print so a wrong N is
// visible before plan 46's counters make it loud.
func Describe(hops int) string {
	if hops <= 0 {
		if !Configured() {
			// Unset, not zero. On a laptop this is correct and unremarkable; on
			// a deployment behind an edge it means the quota subject has never
			// been measured, and the log is the only place that can say so.
			return fmt.Sprintf(
				"client address = peer socket (%s is UNSET — not measured). Behind a proxy or a "+
					"Cloudflare worker this buckets every client together; measure the chain the "+
					"ADR 0005 way and set it", EnvHops)
		}
		return fmt.Sprintf("client address = peer socket (%s=0); X-Forwarded-For ignored", EnvHops)
	}
	return fmt.Sprintf("client address = %s=%d (Nth from the right of X-Forwarded-For)", EnvHops, hops)
}

func splitXFF(xff string) []string {
	var out []string
	for _, p := range strings.Split(xff, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
