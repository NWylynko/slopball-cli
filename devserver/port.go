package devserver

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// DefaultPort is where the supervised dev process must bind (abuse-surface
// ticket 21 / secure-by-default ticket 09). The session-network holder splices
// into this port; a committed PORT= no longer chooses the target, and there is
// no operator override — one supported answer.
const DefaultPort = 3000

// testLocalPort is a port a TEST has installed for this process, or 0.
var testLocalPort atomic.Int64

// SetTestLocalPort points every dev-port resolution in this process at port,
// and returns the restore — `t.Cleanup(devserver.SetTestLocalPort(p))`.
//
// It exists because the machine running the suite is allowed to have its own
// dev server: with the constant hardcoded, every test that needed the port
// either skipped ("DevPort busy" — a prerequisite skip TESTING.md rejects) or
// FAILED, because a foreign listener on 3000 makes "publish only when something
// is listening" observe a listener that is not the session's. Tests reserve an
// ephemeral port instead (`testharness.DevPortForTest`), so no test depends on
// what else this machine is running.
//
// It is a GO SYMBOL on purpose, and there is deliberately no env var, flag or
// file behind it: an operator cannot reach it, so ticket 21's "one supported
// answer" still holds for every shipped binary — a slopball a human runs
// resolves DefaultPort and nothing else. It is process-global, so install it
// before starting the code under test and let the cleanup put it back; the
// separate-process tiers (internal/e2e) cannot see it at all and use the real
// constant.
//
// Exported because the tests live in another module (plan 49: the client is
// public, its tests stay in the monorepo), which can only reach exported names.
func SetTestLocalPort(port int) (restore func()) {
	prev := testLocalPort.Swap(int64(port))
	return func() { testLocalPort.Store(prev) }
}

// ResolveLocalPort is the splice target on this machine: always DefaultPort.
// Never reads the repo or the environment.
func ResolveLocalPort() (int, string) {
	if p := testLocalPort.Load(); p > 0 {
		return int(p), "test override"
	}
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
