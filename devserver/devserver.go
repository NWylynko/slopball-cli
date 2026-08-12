// Package devserver supervises the product's dev process on canonical main
// (plans/05): start/stop, log capture, post-merge lockfile install, and the
// `slopball run` passthrough.
package devserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Stream is which fd a line came from, or that slopball wrote it itself.
//
// StreamSlopball exists because the host narrates its own failures into this
// buffer (hoststart.writeDevLog) and one of those messages quotes Tail(20) back
// into it. Merged, that is a feedback loop the moment detection is broadened
// past a fixed substring list: the watcher reads its own trigger and re-fires.
type Stream string

const (
	StreamStdout   Stream = "stdout"
	StreamStderr   Stream = "stderr"
	StreamSlopball Stream = "slopball"
)

// Phase separates dependency installation from the running dev server. npm puts
// its whole ERESOLVE peer-dependency wall on stderr, and none of it is the
// product being broken — there is no product yet.
type Phase string

const (
	PhaseInstall Phase = "install"
	PhaseDev     Phase = "dev"
)

type tag struct {
	stream Stream
	phase  Phase
}

// record is one line of output and where it came from. open means no newline
// has arrived yet: a pipe delivers whatever it delivers, so a line can land in
// several Writes, and a filtered view that dropped the tail would lose exactly
// the half a crash tends to end on.
type record struct {
	tag  tag
	text string
	open bool
}

// LogBuffer is a concurrency-safe record of process output for the
// error-watcher. Every write is tagged with its stream and phase so the watcher
// can read dev-server stderr alone while /logs still shows a human everything
// that happened, in order.
// logSubscriberQueue is the per-subscriber backlog before a "missed N lines"
// marker is emitted rather than dropping silently (plan 43 ticket 09).
const logSubscriberQueue = 8

// TestSubscribeQueueSize overrides logSubscriberQueue when > 0 (tests only).
var TestSubscribeQueueSize int

type LogBuffer struct {
	mu   sync.Mutex
	recs []record
	open map[tag]int // index of the unterminated record per tag
	// base is how many lines have been dropped by Reset, so Since's sequence
	// keeps counting across a dev-server restart.
	base int64
	subs []*logSubscriber
}

type logSubscriber struct {
	ch     chan LogPage
	cursor int64
	stream Stream
	phase  Phase
	missed int64
}

// LogSubscriber is a held /logs stream (plan 43 ticket 09). Close ends it.
type LogSubscriber struct {
	C     <-chan LogPage
	close func()
}

// Close stops delivery and unregisters the subscriber.
func (s *LogSubscriber) Close() {
	if s != nil && s.close != nil {
		s.close()
	}
}

// Write satisfies io.Writer for callers that hold the buffer directly. It tags
// as slopball-authored: anything writing here is slopball itself, and the safe
// direction for an untagged write is the one the watcher ignores.
func (l *LogBuffer) Write(p []byte) (int, error) {
	l.write(tag{StreamSlopball, PhaseDev}, p)
	return len(p), nil
}

// Writer returns an io.Writer that tags everything it receives.
func (l *LogBuffer) Writer(stream Stream, phase Phase) io.Writer {
	return &tagWriter{buf: l, tag: tag{stream, phase}}
}

// WriteLine appends one tagged line.
func (l *LogBuffer) WriteLine(stream Stream, phase Phase, text string) {
	l.write(tag{stream, phase}, []byte(strings.TrimSuffix(text, "\n")+"\n"))
}

type tagWriter struct {
	buf *LogBuffer
	tag tag
}

func (w *tagWriter) Write(p []byte) (int, error) {
	w.buf.write(w.tag, p)
	return len(p), nil
}

func (l *LogBuffer) write(t tag, p []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	defer l.notifySubs()
	if l.open == nil {
		l.open = map[tag]int{}
	}
	s := string(p)
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			if s != "" {
				l.appendOpen(t, s)
			}
			return
		}
		l.appendOpen(t, s[:i])
		if idx, ok := l.open[t]; ok {
			l.recs[idx].open = false
			delete(l.open, t)
		}
		s = s[i+1:]
	}
}

// appendOpen extends this tag's unterminated line, or starts one. The record
// keeps the position where the line *began*, so interleaved streams stay in the
// order a reader would have seen them.
func (l *LogBuffer) appendOpen(t tag, s string) {
	if idx, ok := l.open[t]; ok {
		l.recs[idx].text += s
		return
	}
	l.recs = append(l.recs, record{tag: t, text: s, open: true})
	l.open[t] = len(l.recs) - 1
}

func (l *LogBuffer) render(match func(tag) bool) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	for _, r := range l.recs {
		if match != nil && !match(r.tag) {
			continue
		}
		b.WriteString(r.text)
		if !r.open {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// String is the merged human view: everything, in order, as /logs has always
// served it.
func (l *LogBuffer) String() string { return l.render(nil) }

// Select returns only the lines matching a stream and phase — the watcher's
// view. An empty stream or phase matches any.
func (l *LogBuffer) Select(stream Stream, phase Phase) string {
	return l.render(func(t tag) bool {
		return (stream == "" || t.stream == stream) && (phase == "" || t.phase == phase)
	})
}

// Watchable is the error-watcher's view: stderr the dev server itself wrote.
// Install-phase output and slopball's own narration are excluded by
// construction rather than by pattern-matching them back out.
func (l *LogBuffer) Watchable() string { return l.Select(StreamStderr, PhaseDev) }

// Line is one tagged line with its position in the stream — what a follower
// reads through Since.
type Line struct {
	Seq    int64  `json:"seq"`
	Stream Stream `json:"stream,omitempty"`
	Phase  Phase  `json:"phase,omitempty"`
	Text   string `json:"text"`
}

// LogPage is what a follower gets back over the wire: the lines after its
// cursor, and where to resume. Missed is set only on held SSE streams when a
// subscriber's queue overflowed — the plain JSON GET never carries it.
type LogPage struct {
	Lines  []Line `json:"lines"`
	Cursor int64  `json:"cursor"`
	Missed int64  `json:"missed,omitempty"`
}

// Since returns every line after cursor matching stream and phase (empty
// matches any), plus the cursor to pass next time. Cursor 0 starts at the
// beginning. This is what makes /logs followable instead of a snapshot
// (plan 36 §2) — the buffer already knew each line's stream and phase, this
// only exposes it per line.
//
// A line still being written is returned but does NOT advance the cursor, so
// the next read re-sends it complete under the same Seq. A follower keyed on
// Seq replaces it in place; one that appended blindly would show a dying
// server's last line twice, or — if we withheld it — not at all, and that line
// is usually the reason it died.
func (l *LogBuffer) Since(cursor int64, stream Stream, phase Phase) ([]Line, int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sinceLocked(cursor, stream, phase)
}

func (l *LogBuffer) sinceLocked(cursor int64, stream Stream, phase Phase) ([]Line, int64) {
	next := cursor
	var out []Line
	for i, r := range l.recs {
		seq := l.base + int64(i) + 1
		if seq <= cursor {
			continue
		}
		if (stream == "" || r.tag.stream == stream) && (phase == "" || r.tag.phase == phase) {
			out = append(out, Line{Seq: seq, Stream: r.tag.stream, Phase: r.tag.phase, Text: r.text})
		}
		// Closed lines advance the cursor whether or not they matched: a filter
		// must not make a follower rescan the whole buffer forever.
		if !r.open {
			next = seq
		}
	}
	return out, next
}

// Subscribe holds a filtered view of the buffer from cursor forward. The first
// page is the same catch-up a plain ?since= JSON GET would return; later pages
// arrive as lines are appended. Close when done.
func (l *LogBuffer) Subscribe(cursor int64, stream Stream, phase Phase) *LogSubscriber {
	q := logSubscriberQueue
	if TestSubscribeQueueSize > 0 {
		q = TestSubscribeQueueSize
	}
	ch := make(chan LogPage, q)
	sub := &logSubscriber{ch: ch, cursor: cursor, stream: stream, phase: phase}
	l.mu.Lock()
	lines, next := l.sinceLocked(cursor, stream, phase)
	l.subs = append(l.subs, sub)
	l.mu.Unlock()
	if sub.deliver(LogPage{Lines: lines, Cursor: next}) {
		sub.cursor = next
	}
	return &LogSubscriber{C: ch, close: func() { l.unsubscribe(sub) }}
}

func (l *LogBuffer) unsubscribe(sub *logSubscriber) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, s := range l.subs {
		if s == sub {
			l.subs = append(l.subs[:i], l.subs[i+1:]...)
			lines, next := l.sinceLocked(sub.cursor, sub.stream, sub.phase)
			if len(lines) > 0 {
				page := LogPage{Lines: lines, Cursor: next}
				if sub.missed > 0 {
					page.Missed = sub.missed
					sub.missed = 0
				}
				sub.ch <- page
			} else {
				sub.flushPending()
			}
			close(sub.ch)
			return
		}
	}
}

func (sub *logSubscriber) deliver(page LogPage) bool {
	if sub.missed > 0 {
		page.Missed = sub.missed
		sub.missed = 0
	}
	if len(page.Lines) == 0 && page.Missed == 0 {
		return true
	}
	select {
	case sub.ch <- page:
		return true
	default:
		sub.missed += int64(len(page.Lines))
		if page.Missed > 0 {
			sub.missed += page.Missed
		}
		sub.flushMissed()
		return false
	}
}

func (sub *logSubscriber) flushMissed() {
	if sub.missed == 0 {
		return
	}
	select {
	case sub.ch <- LogPage{Missed: sub.missed}:
		sub.missed = 0
	default:
	}
}

func (sub *logSubscriber) flushPending() {
	if sub.missed > 0 {
		sub.ch <- LogPage{Missed: sub.missed}
		sub.missed = 0
	}
}

func (l *LogBuffer) notifySubs() {
	for _, sub := range l.subs {
		sub.flushMissed()
		lines, next := l.sinceLocked(sub.cursor, sub.stream, sub.phase)
		if len(lines) == 0 {
			continue
		}
		if sub.deliver(LogPage{Lines: lines, Cursor: next}) {
			sub.cursor = next
		}
	}
}

// Reset drops the buffer on a dev-server restart. The sequence keeps counting
// across it, so a follower's cursor never silently starts pointing at different
// lines — it just sees nothing until new output arrives.
func (l *LogBuffer) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.base += int64(len(l.recs))
	l.recs = nil
	l.open = nil
	l.notifySubs()
}

// Tail returns the last n non-empty lines — what a failure report should carry,
// since the whole buffer can be a full npm install.
func (l *LogBuffer) Tail(n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(l.String(), "\n"), "\n")
	var keep []string
	for i := len(lines) - 1; i >= 0 && len(keep) < n; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			keep = append([]string{lines[i]}, keep...)
		}
	}
	return strings.Join(keep, "\n")
}

// Supervisor runs a long-lived command against the main working tree.
//
// The process is reaped in the background so that a dev command which exits
// immediately — a missing package.json, no node_modules, a script name that
// doesn't exist — is reportable instead of looking identical to a healthy start.
type Supervisor struct {
	WorkDir string
	Command []string // e.g. ["npm", "run", "dev"] — empty = no-op stub
	Logs    *LogBuffer

	mu        sync.Mutex
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	running   bool
	stopping  bool
	exitErr   error
	startedAt time.Time
	exitedAt  time.Time
	done      chan struct{}

	// Liveness bookkeeping (plan 34). shortExits counts *consecutive* restarts
	// of a process that died almost as soon as it started; a run that lasted is
	// what clears it. gaveUp is the circuit breaker: set once, it stops the
	// retries and the log line that goes with them.
	shortExits int
	gaveUp     bool
}

// Start launches the dev server if Command is set.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Command) == 0 {
		return nil
	}
	if s.Logs == nil {
		s.Logs = &LogBuffer{}
	}
	cctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	cmd := exec.CommandContext(cctx, s.Command[0], s.Command[1:]...)
	cmd.Dir = s.WorkDir
	port, _ := ResolveLocalPort()
	cmd.Env = WithPortEnv(os.Environ(), port)
	// Tagged separately: aliasing both fds to one writer is what left the
	// watcher unable to tell a broken product from npm's warnings.
	cmd.Stdout = s.Logs.Writer(StreamStdout, PhaseDev)
	cmd.Stderr = s.Logs.Writer(StreamStderr, PhaseDev)
	s.startedAt = time.Now()
	if err := cmd.Start(); err != nil {
		cancel()
		s.cancel = nil
		s.exitErr = err
		s.exitedAt = time.Now()
		s.done = closedChan()
		return err
	}
	s.cmd = cmd
	s.running = true
	s.stopping = false
	s.exitErr = nil
	s.exitedAt = time.Time{}
	done := make(chan struct{})
	s.done = done
	go s.reap(cmd, done)
	return nil
}

// reap waits for the process and records why it went away. A kill we asked for
// is not a failure; anything else is.
func (s *Supervisor) reap(cmd *exec.Cmd, done chan struct{}) {
	err := cmd.Wait()
	s.mu.Lock()
	s.running = false
	s.exitedAt = time.Now()
	if !s.stopping {
		s.exitErr = err
	}
	if s.cmd == cmd {
		s.cmd = nil
	}
	s.mu.Unlock()
	close(done)
}

// Running reports whether the supervised process is alive.
func (s *Supervisor) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Err is why the process is gone, or nil when it is alive, was never started, or
// was stopped on purpose.
func (s *Supervisor) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitErr
}

// RanFor is how long the process has been up, or how long it lasted before it
// died — the difference between "the dev server is starting slowly" and "it fell
// over instantly".
func (s *Supervisor) RanFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startedAt.IsZero() {
		return 0
	}
	if s.exitedAt.IsZero() {
		return time.Since(s.startedAt)
	}
	return s.exitedAt.Sub(s.startedAt)
}

// Done closes when the current process exits. nil-safe for a supervisor with no
// command: a nil channel blocks forever, which is the right answer for "this
// will never exit because it never ran".
func (s *Supervisor) Done() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// Stop terminates the dev server.
func (s *Supervisor) Stop() error {
	s.mu.Lock()
	s.stopping = true
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	cmd := s.cmd
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	return nil
}

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// Reload restarts the process after main advances (hot-reload stand-in).
func (s *Supervisor) Reload(ctx context.Context) error {
	_ = s.Stop()
	return s.Start(ctx)
}

// Liveness (plan 34). The supervisor has always known whether its process is
// alive; until now nothing asked, so a dev server that exited on its own stayed
// dead for the rest of the session.
//
// The signal is deliberately narrow: `running` is set false by reap, after
// cmd.Wait returns. That is the OS telling us the process is gone — a fact. A
// live process that has not answered on its port yet is a cold `next dev` build,
// not a fault, and nothing here may act on it. See the package doc and plan 34's
// "What counts as dead": no port dialling, no HTTP probe, no ready-line parsing,
// because every one of those needs a timeout that is a guess about somebody
// else's build.
const (
	// maxShortExits is how many consecutive fast deaths are retried before the
	// breaker trips. A failure count, not a clock.
	maxShortExits = 3
	// shortRun is the run length below which an exit counts as "it fell over
	// instantly" rather than "it ran for a while and then died". Generous
	// enough that a real dev server which served requests and later crashed is
	// always treated as the recoverable case.
	shortRun = 5 * time.Second
)

// ErrGaveUp is returned by Restart on the attempt where the breaker trips, so
// the caller can log the loud line exactly once.
var ErrGaveUp = errors.New("dev server exited immediately too many times in a row")

// NeedsRestart reports a supervised process that went away by itself: it has a
// command, it has been started at least once, it is not running, and nobody
// asked it to stop. False once the breaker has tripped — the give-up is the
// answer, and repeating it every 2s would be its own kind of noise.
func (s *Supervisor) NeedsRestart() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Command) > 0 && !s.startedAt.IsZero() && !s.running && !s.stopping && !s.gaveUp
}

// Restart relaunches a process that exited on its own, counting the fast deaths
// so a command that can never work cannot hot-loop. Returns ErrGaveUp on the
// attempt that trips the breaker, and refuses quietly after that.
func (s *Supervisor) Restart(ctx context.Context) error {
	s.mu.Lock()
	if s.gaveUp {
		s.mu.Unlock()
		return ErrGaveUp
	}
	if s.startedAt.IsZero() || s.exitedAt.IsZero() {
		s.mu.Unlock()
		return nil
	}
	if s.exitedAt.Sub(s.startedAt) < shortRun {
		s.shortExits++
	} else {
		s.shortExits = 0
	}
	if s.shortExits > maxShortExits {
		s.gaveUp = true
		s.mu.Unlock()
		return ErrGaveUp
	}
	s.mu.Unlock()
	return s.Start(ctx)
}

// ShortExits is how many consecutive immediate deaths this command has had —
// what a restart line reports as the attempt number.
func (s *Supervisor) ShortExits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shortExits
}

// ClearGiveUp gives a given-up dev command another bounded run of attempts.
// Called when main advances: a new commit is a new reason to believe the command
// might work now — the project it was waiting for may have just arrived, or the
// commit that broke it may have just been fixed.
func (s *Supervisor) ClearGiveUp() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shortExits, s.gaveUp = 0, false
}

// Lockfiles that trigger a post-merge install when they change.
var lockfiles = []string{
	"pnpm-lock.yaml",
	"package-lock.json",
	"yarn.lock",
	"bun.lockb",
	"Cargo.lock",
	"go.sum",
}

// Install runs a dependency-install command in workDir, streaming stdout+stderr
// to out. Empty cmd falls through to DetectInstall; still empty is a no-op (an
// unrecognized stack is not an error). This is the first-boot path (plan 26) —
// PostMergeInstall keeps its lockfile diff gate and then delegates here.
func Install(ctx context.Context, workDir string, cmd []string, out io.Writer) error {
	if len(cmd) == 0 {
		cmd = DetectInstall(workDir)
	}
	if len(cmd) == 0 {
		return nil
	}
	if out == nil {
		out = io.Discard
	}
	// Tee into a local buffer so a failure names the argv *and* carries the
	// install's own output — matching the post-merge shape the error-watcher
	// already reads — while still streaming live for a multi-minute npm install.
	var buf bytes.Buffer
	w := io.MultiWriter(out, &buf)
	if err := Run(ctx, workDir, cmd, w); err != nil {
		return fmt.Errorf("install %v: %w\n%s", cmd, err, buf.String())
	}
	return nil
}

// PostMergeInstall runs the stack's install if any lockfile changed between
// oldHEAD and newHEAD in workDir. installCmd overrides auto-detection; out
// receives the install's output — pass the dev LogBuffer so a mid-session
// install lands in front of the error-watcher, the same as the first-boot one.
func PostMergeInstall(ctx context.Context, workDir, oldHEAD, newHEAD string, installCmd []string, gitRun func(ctx context.Context, dir string, args ...string) (string, error), out io.Writer) error {
	changed := false
	for _, lf := range lockfiles {
		out, err := gitRun(ctx, workDir, "diff", "--name-only", oldHEAD, newHEAD, "--", lf)
		if err == nil && strings.TrimSpace(out) != "" {
			changed = true
			break
		}
	}
	if !changed {
		return nil
	}
	return Install(ctx, workDir, installCmd, out)
}

// DetectInstall picks the stack's install command from lockfiles / manifests
// present in workDir. Returns nil when nothing recognizable is there.
func DetectInstall(workDir string) []string {
	switch {
	case exists(workDir, "pnpm-lock.yaml"):
		return []string{"pnpm", "install"}
	case exists(workDir, "yarn.lock"):
		return []string{"yarn", "install"}
	case exists(workDir, "package-lock.json"), exists(workDir, "package.json"):
		return []string{"npm", "install"}
	case exists(workDir, "go.mod"):
		return []string{"go", "mod", "download"}
	case exists(workDir, "Cargo.lock"):
		return []string{"cargo", "fetch"}
	default:
		return nil
	}
}

// DetectDev picks the stack's dev-server command from what the project
// actually declares — a `dev` script in package.json, or a Go main package.
// Returns nil when nothing recognizable is there, because a guessed dev command
// dies on its first boot and nothing restarts it (plan 26's note). Package
// manager precedence matches DetectInstall so the two answers agree.
func DetectDev(workDir string) []string {
	if hasNodeScript(workDir, "dev") {
		switch {
		case exists(workDir, "pnpm-lock.yaml"):
			return []string{"pnpm", "run", "dev"}
		case exists(workDir, "yarn.lock"):
			return []string{"yarn", "run", "dev"}
		default:
			return []string{"npm", "run", "dev"}
		}
	}
	if exists(workDir, "go.mod") && hasGoMainPackage(workDir) {
		return []string{"go", "run", "."}
	}
	return nil
}

// hasNodeScript reports whether package.json declares the named script.
func hasNodeScript(workDir, script string) bool {
	data, err := os.ReadFile(filepath.Join(workDir, "package.json"))
	if err != nil {
		return false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return false
	}
	return strings.TrimSpace(pkg.Scripts[script]) != ""
}

// hasGoMainPackage reports whether the module root holds a runnable main.
func hasGoMainPackage(workDir string) bool {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(workDir, e.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == "package main" {
				return true
			}
		}
	}
	return false
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// Run executes an arbitrary command against workDir and streams output to out.
// This is the `slopball run` passthrough (§5.7 #4).
func Run(ctx context.Context, workDir string, args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("run: no command")
	}
	c := exec.CommandContext(ctx, args[0], args[1:]...)
	c.Dir = workDir
	c.Stdout = out
	c.Stderr = out
	// Cancelling the context kills the direct child only. Anything it forked
	// (npm's helpers, a shell's `sleep`) inherits the stdout pipe, and Wait
	// blocks until the last holder closes it — so a Ctrl-C during a first-boot
	// install used to stall host start for the grandchild's full lifetime.
	// WaitDelay bounds that: force-close the pipes and return.
	c.WaitDelay = killGrace
	return c.Run()
}

// killGrace is how long Wait tolerates a cancelled command's descendants
// holding its pipes open before giving up on them.
const killGrace = 5 * time.Second
