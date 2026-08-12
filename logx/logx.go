// Package logx is slopball's leveled, component-tagged logger for the
// long-running host/join loops. The point is operator visibility: when Nick (or
// a teammate) runs `slopball` or `slopball join` and watches the terminal, they
// should see what the session is doing — ticks, merges, fetches, pushes — not a
// silent process.
//
// Info is on by default so that activity is visible without extra flags. Debug
// (per-git-command traces, tick internals) is gated behind SLOPBALL_LOG=debug
// so a normal terminal stays readable while a deep-dive is one env var away.
package logx

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Level orders the four severities. Debug is the only one off by default.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (l Level) tag() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	default:
		return "ERROR"
	}
}

var (
	mu    sync.Mutex
	out   io.Writer = os.Stderr
	level           = LevelInfo
	clock           = time.Now

	sinks      map[int]Sink
	nextSinkID int
)

func init() {
	switch strings.ToLower(os.Getenv("SLOPBALL_LOG")) {
	case "debug", "trace":
		level = LevelDebug
	case "info":
		level = LevelInfo
	case "warn":
		level = LevelWarn
	case "error":
		level = LevelError
	}
}

// SetOutput redirects the log stream (tests, or piping to a file) and returns
// the previous writer, so a caller that borrows the stream — the session
// console, which owns the terminal for its whole life — puts back what was
// actually there rather than assuming stderr.
func SetOutput(w io.Writer) io.Writer {
	mu.Lock()
	defer mu.Unlock()
	prev := out
	out = w
	return prev
}

// Sink observes every line this process emits, after it has been written to
// the terminal. It is the ONE hook a client's telemetry uses (plan 46 ticket
// 13): role states, activity and merge outcomes are all already logx lines, so
// a parallel structured path would be a second source of the same truth.
//
// It sees exactly what the terminal saw — a line suppressed by the level
// threshold was never emitted, and recording it would make the table disagree
// with the log a human is reading beside it.
type Sink func(ts time.Time, level, component, message string)

// AddSink installs s and returns the function that removes it again.
//
// There is more than one observer of this stream and they are independent: a
// machine records telemetry only when a human opted in (plan 46 ticket 12),
// while the per-session log file `slopball report` reads is written for
// everybody, because it never leaves the machine. One slot would have made
// those two decisions the same decision.
func AddSink(s Sink) (remove func()) {
	if s == nil {
		return func() {}
	}
	mu.Lock()
	id := nextSinkID
	nextSinkID++
	if sinks == nil {
		sinks = map[int]Sink{}
	}
	sinks[id] = s
	mu.Unlock()
	return func() {
		mu.Lock()
		delete(sinks, id)
		mu.Unlock()
	}
}

// SetLevel overrides the active threshold. Returns the previous level so a test
// can restore it.
func SetLevel(l Level) Level {
	mu.Lock()
	defer mu.Unlock()
	prev := level
	level = l
	return prev
}

// DebugEnabled reports whether debug output is on — use it to guard expensive
// message construction (e.g. running an extra git command just to log its
// result).
func DebugEnabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return level <= LevelDebug
}

// Logger is a component-tagged front end (e.g. "host", "join", "git").
type Logger struct{ component string }

// New returns a logger tagged with component.
func New(component string) *Logger { return &Logger{component: component} }

func (l *Logger) emit(lv Level, format string, args ...any) {
	mu.Lock()
	if lv < level {
		mu.Unlock()
		return
	}
	now := clock()
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(out, "%s %-5s %-6s %s\n", now.Format("15:04:05.000"), lv.tag(), l.component, msg)
	var observers []Sink
	for _, s := range sinks {
		observers = append(observers, s)
	}
	mu.Unlock()

	// The sink runs OUTSIDE the lock, deliberately. It is telemetry (plan 46
	// ticket 13), and telemetry can itself log — an emitter warning that it
	// dropped a batch is exactly the line that must survive — so calling it
	// while holding this mutex is a self-deadlock waiting for the one failure
	// it exists to report.
	for _, s := range observers {
		s(now, strings.ToLower(lv.tag()), l.component, msg)
	}
}

// Debugf logs at debug level (off unless SLOPBALL_LOG=debug).
func (l *Logger) Debugf(format string, args ...any) { l.emit(LevelDebug, format, args...) }

// Infof logs at info level (on by default).
func (l *Logger) Infof(format string, args ...any) { l.emit(LevelInfo, format, args...) }

// Warnf logs at warn level.
func (l *Logger) Warnf(format string, args ...any) { l.emit(LevelWarn, format, args...) }

// Errorf logs at error level. Use it where the code currently swallows an error
// with `_ =` in a long-running loop — a silent failure is the worst kind here.
func (l *Logger) Errorf(format string, args ...any) { l.emit(LevelError, format, args...) }
