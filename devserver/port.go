package devserver

import (
	"fmt"
	"strings"
)

// DefaultPort is where the supervised dev process must bind (abuse-surface
// ticket 21 / secure-by-default ticket 09). The session-network holder splices
// into this port; a committed PORT= no longer chooses the target, and there is
// no operator override — one supported answer.
const DefaultPort = 3000

// ResolveLocalPort is the splice target on this machine: always DefaultPort.
// Never reads the repo or the environment.
func ResolveLocalPort() (int, string) {
	return DefaultPort, ""
}

// WithPortEnv injects PORT=<port>, stripping any existing PORT= so a
// materialized .env that still says 4000 cannot win over the splice target.
func WithPortEnv(base []string, port int) []string {
	if port <= 0 {
		return base
	}
	out := make([]string, 0, len(base)+1)
	for _, e := range base {
		if strings.HasPrefix(e, "PORT=") {
			continue
		}
		out = append(out, e)
	}
	return append(out, fmt.Sprintf("PORT=%d", port))
}
