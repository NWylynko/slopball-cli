package telemetry

import (
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/slopball-cli/logx"
	"github.com/nwylynko/slopball-cli/session"
)

// The client's logx stream, verbatim (plan 46 ticket 13).
//
// An opted-in machine's log output and its agent's activity land in the table,
// so a session can be read from the participants' side and not only the
// server's. There is exactly ONE path: a hook in logx, plus the harness
// stream — and no second "facts" channel, because role states, activity and
// merge outcomes are all already logx lines and a parallel structured path is
// the `session_facts` table this plan deleted, arriving under a new name.
//
// `client.log` is one emission site covering everything a laptop narrates,
// which is why level and component are their own columns: the merger tick can
// be switched off in a future binary without killing client logging.
const (
	eventClientLog     = "client.log"
	eventClientHarness = "client.harness"
)

var (
	memberMu     sync.Mutex
	memberCfg    MemberConfig
	memberEm     *Emitter
	memberUnhook func()

	// The per-session log is separate from the emitter and separately gated:
	// it is written for everybody because it never leaves the machine, while
	// the emitter only exists when a human opted in. `slopball report` is what
	// later sends it, once, for one session (plan 46 ticket 15).
	memberLog       *session.ClientLog
	memberLogUnhook func()
)

// UseMember points this process's client telemetry at a session. It is
// idempotent and cheap to call on every member cycle — which is where it is
// called from, because that cycle is the one place that already holds all four
// facts: the pin, the session uid, this member's id, and a fresh ticket.
//
// Rebuilding only on a real change matters: the ticket is renewed hourly, so a
// per-cycle rebuild would throw away a queue every 5 seconds.
//
// **Nothing that can log, and nothing that can wait, happens under memberMu.**
// That is the whole shape of this function and it is not a tidiness point: the
// logx sink takes this lock to find out where to send a line, so anything that
// logs while holding it wedges every log line in the process — permanently, and
// on the same goroutine when it is a direct call. The two that bite are the
// emitter's own shutdown (Close waits for the delivery goroutine, which LOGS the
// batch it gave up on — the one line that must survive when the telemetry path
// is itself the broken thing) and ForMember (SessionIngest warns when a session
// advertises no ingest). Both fire in the ordinary case of the ingest being
// unreachable, which is the one case telemetry is required never to be
// load-bearing in.
//
// So: decide, build, then install — with the lock held only for the third.
func UseMember(cfg MemberConfig) {
	if memberIs(cfg) {
		return
	}
	em := ForMember(cfg) // may log
	previous, noLocalLog := installMember(cfg, em)
	previous.Close() // may block, and logs while it does
	if noLocalLog != nil {
		logx.New("telemetry").Warnf("%s: no session log on this machine (%v) — `slopball report` will have nothing local to send", cfg.PIN, noLocalLog)
	}
}

// memberIs reports whether the live configuration already is cfg, so the common
// case — the member cycle re-reporting an unchanged session every 5 seconds —
// costs one lock and builds nothing.
func memberIs(cfg MemberConfig) bool {
	memberMu.Lock()
	defer memberMu.Unlock()
	return memberUnhook != nil && cfg == memberCfg
}

// installMember publishes the emitter UseMember built and hands back whatever
// the caller must now dispose of or say. If a concurrent caller won the race the
// new emitter is handed straight back as `previous`, so it is closed rather than
// leaked.
func installMember(cfg MemberConfig, em *Emitter) (previous *Emitter, noLocalLog error) {
	memberMu.Lock()
	defer memberMu.Unlock()
	if memberUnhook != nil && cfg == memberCfg {
		return em, nil
	}
	previous, memberEm = memberEm, em
	memberCfg = cfg
	if memberUnhook == nil {
		memberUnhook = logx.AddSink(logSink)
	}
	if memberLog == nil && cfg.PIN != "" {
		lf, err := session.OpenClientLog(cfg.PIN)
		if err != nil {
			return previous, err
		}
		memberLog = lf
		memberLogUnhook = logx.AddSink(func(ts time.Time, level, component, message string) {
			lf.Write(ts, level, component, message)
		})
	}
	return previous, nil
}

// StopMember removes the hook and drains what is queued. A session's Close
// calls it; after it, the process narrates to its terminal and nowhere else.
func StopMember() {
	memberMu.Lock()
	em := memberEm
	memberEm = nil
	memberCfg = MemberConfig{}
	if memberUnhook != nil {
		memberUnhook()
		memberUnhook = nil
	}
	if memberLogUnhook != nil {
		memberLogUnhook()
		memberLogUnhook = nil
	}
	lf := memberLog
	memberLog = nil
	memberMu.Unlock()
	_ = lf.Close()
	// Close outside the lock: it waits for the delivery goroutine, which may be
	// mid-POST, and holding the lock across that would stall every log line in
	// the process.
	em.Close()
}

// member returns the live emitter and the session it belongs to.
func member() (*Emitter, MemberConfig) {
	memberMu.Lock()
	defer memberMu.Unlock()
	return memberEm, memberCfg
}

func logSink(ts time.Time, level, component, message string) {
	em, cfg := member()
	if !em.Enabled() {
		return
	}
	body, truncated := CaptureString(message)
	em.Emit(eventClientLog, Event{
		TS: ts.UTC(), PIN: cfg.PIN, SessionUID: cfg.SessionUID, Member: cfg.MemberID,
		Level: level, Component: component,
		Body: body, Truncated: truncated,
	})
}

// EmitHarnessEvent records one readable step of an agentic run: what kind of
// step it was, which tool, the one-line activity the dashboard shows, and the
// text itself under the 64 KiB cap.
//
// Volume is fine — the event NAME is what makes it prunable later, which is
// why the agent's steps are their own name rather than more client.log rows.
func EmitHarnessEvent(kind, tool, text, activity string) {
	em, cfg := member()
	if !em.Enabled() {
		return
	}
	body, truncated := CaptureString(strings.TrimSpace(text))
	em.Emit(eventClientHarness, Event{
		PIN: cfg.PIN, SessionUID: cfg.SessionUID, Member: cfg.MemberID,
		Component: "harness",
		Body:      body, Truncated: truncated,
		Data: map[string]any{"kind": kind, "tool": tool, "activity": activity},
	})
}
