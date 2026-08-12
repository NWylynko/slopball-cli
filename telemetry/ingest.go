package telemetry

import (
	"strings"
	"sync"

	"github.com/nwylynko/slopball-cli/logx"
)

// AdvertiseEnv is what the CONTROL PLANE is told, so it can tell everyone else
// (plan 46 ticket 11). It names a deployment rather than this source tree,
// which is why it is an env on the control plane and not a build stamp like
// CONTROL_URL — and why it is deliberately NOT a laptop env var: the channel
// follows the audience (ADR 0006 decision 3), and a laptop is told by the
// session it already fetches.
//
// Distinct from URLEnv, which is where a SERVICE posts its own envelopes. On
// the compose bridge those differ — a container name versus a public hostname
// — and one name serving both would be wrong for one of them.
const AdvertiseEnv = "SLOPBALL_TELEMETRY_ADVERTISE"

var (
	ingestWarnMu   sync.Mutex
	ingestWarnedAt map[string]bool
)

// SessionIngest resolves the ingest a member should post to, from what the
// session advertised, and reports an absent one EXACTLY ONCE per session.
//
// Once matters: the member cycle re-reads the snapshot every 5 seconds, so a
// warn per read would be the loudest line in the log for the life of the
// session and would train everyone to ignore it. Silence matters too — an
// opted-in machine that records nothing has to say why, or the missing data
// looks like a telemetry bug rather than a missing deployment value.
func SessionIngest(pin, advertised string) string {
	url := strings.TrimSpace(advertised)
	if url != "" {
		return url
	}
	ingestWarnMu.Lock()
	if ingestWarnedAt == nil {
		ingestWarnedAt = map[string]bool{}
	}
	first := !ingestWarnedAt[pin]
	ingestWarnedAt[pin] = true
	ingestWarnMu.Unlock()
	if first {
		logx.New("telemetry").Warnf("%s: telemetry is on for this machine but the session advertises no ingest, so nothing is being recorded — the control plane needs %s set (see .env.example)",
			pin, AdvertiseEnv)
	}
	return ""
}

// ForgetIngestWarnings resets the per-session warn state. Tests only: a
// process genuinely wants one line per session for its whole life.
func ForgetIngestWarnings() {
	ingestWarnMu.Lock()
	ingestWarnedAt = nil
	ingestWarnMu.Unlock()
}
