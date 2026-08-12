package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The per-session client log is what `slopball report` sends when something
// broke (plan 46 ticket 15).
//
// It is written for EVERYBODY, opted in or not, because it never leaves the
// machine: it is the same narration already on your terminal, kept so that a
// terminal you closed is not the only copy. Telemetry's opt-in governs what is
// SENT; this governs what exists locally to send if you later choose to.
//
// Bounded on open rather than on every write: a session's log is minutes to
// hours of one process's narration, and trimming per line would put a stat and
// a rewrite on the path of every log call in the program.
const (
	// ClientLogName is the file under sessions/<pin>/.
	ClientLogName = "client.log"
	// clientLogMax is how much of it is kept. Big enough to hold a whole
	// session's narration, small enough that a laptop never notices.
	clientLogMax = 4 << 20
)

// ClientLogPath is where a session's own narration is kept.
func ClientLogPath(pin string) string {
	return filepath.Join(ForPin(pin).Root, ClientLogName)
}

// ClientLog is an open per-session log. Concurrent writers are expected (every
// goroutine in the process logs), so writes are serialised.
type ClientLog struct {
	mu sync.Mutex
	f  *os.File
}

// OpenClientLog opens (creating) the session's log, trimming it first when it
// has grown past the cap. A failure is returned rather than swallowed: a
// missing log is why `report` would later have nothing to say.
func OpenClientLog(pin string) (*ClientLog, error) {
	path := ClientLogPath(pin)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > clientLogMax {
		if b, err := os.ReadFile(path); err == nil {
			_ = os.WriteFile(path, b[len(b)-clientLogMax/2:], 0o644)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &ClientLog{f: f}, nil
}

// Write records one line in the same shape the terminal shows, plus the date —
// a report read hours later needs to know which day it is looking at.
func (l *ClientLog) Write(ts time.Time, level, component, message string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.f, "%s %-5s %-6s %s\n",
		ts.Format("2006-01-02 15:04:05.000"), strings.ToUpper(level), component, message)
}

func (l *ClientLog) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// ReadClientLogTail returns the last n bytes of a session's log, or "" with a
// reason when there is none — `report` says which, rather than shipping an
// empty section that reads like the session was silent.
func ReadClientLogTail(pin string, n int) (string, error) {
	b, err := os.ReadFile(ClientLogPath(pin))
	if err != nil {
		return "", err
	}
	if n > 0 && len(b) > n {
		b = b[len(b)-n:]
		if i := strings.IndexByte(string(b), '\n'); i >= 0 && i+1 < len(b) {
			b = b[i+1:] // start at a line boundary
		}
	}
	return string(b), nil
}
