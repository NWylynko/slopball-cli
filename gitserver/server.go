// Package gitserver serves a bare repo over HTTP using the bundled git's
// upload-pack / receive-pack — the live session remote (plans/03, §5.4/§5.6).
//
// The plain listener binds loopback, always — it exists only as this machine's
// own git origin. Everything arriving from off this machine reaches the same
// mux through the session network (relay or direct), and has completed the
// session-key handshake first — see SessionNet.
package gitserver

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/slopball-cli/devserver"
	sbGit "github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/netbind"
	"github.com/nwylynko/slopball-cli/sessionnet"
)

// Server is a session git HTTP endpoint backed by one bare repo.
type Server struct {
	Bare string // absolute path to the bare repo

	// Bind is the *direct* session-network listener mode from BindForControl
	// (see internal/netbind): "auto" or "" (loopback). The plain HTTP listener
	// ignores this and always binds loopback — BindForControl's auto-upgrade
	// exists so a remote control plane gets a publishable direct address, never
	// an open unauthenticated door on the LAN.
	Bind string

	// Logs, when set, is served as plain text at /logs on the same endpoint.
	// This is how the canonical dev server's output reaches an off-box
	// conductor's error-watcher (which lives on a laptop and cannot see the
	// box's process logs directly). Read at request time, so it may be set
	// after Start. nil = /logs returns empty.
	Logs func() string

	// LogsSelect, when set, serves a filtered view at /logs?stream=&phase=.
	// The off-box error-watcher asks for dev-phase stderr; a human (slopdebug,
	// curl) passes no query and keeps getting everything, in order.
	LogsSelect func(stream, phase string) string

	// LogsSince, when set, serves a followable view at /logs?since=<cursor>:
	// JSON lines plus the next cursor, so the console's dev tab streams instead
	// of re-reading a snapshot (plan 36 §2). Without `since` the response is
	// byte-for-byte the plain text it has always been — slopdebug, curl and the
	// error-watcher all depend on that.
	LogsSince func(cursor int64, stream devserver.Stream, phase devserver.Phase) ([]devserver.Line, int64)

	// LogsSubscribe, when set, serves a held SSE stream at /logs?since=<cursor>
	// with Accept: text/event-stream (plan 43 ticket 09).
	LogsSubscribe func(cursor int64, stream devserver.Stream, phase devserver.Phase) *devserver.LogSubscriber

	// Session, when set, publishes this endpoint onto the session network
	// (plan 09) in addition to the local listener. Clients then reach it by
	// session address rather than machine address, and only with the session
	// key.
	Session *SessionNet

	mu            sync.Mutex
	ln            net.Listener
	sessionLn     net.Listener
	srv           *http.Server
	url           string
	sessionURL    string
	sessionDirect string
	name          string
	// closing is closed by Close to end held /logs streams, which would
	// otherwise keep http.Server.Shutdown waiting forever.
	closing chan struct{}
}

// closingCh returns the shutdown signal for held handlers, creating it on first
// use so a Server built as a bare literal still works.
func (s *Server) closingCh() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing == nil {
		s.closing = make(chan struct{})
	}
	return s.closing
}

// SessionNet is what a git server needs to join the session network.
type SessionNet struct {
	Relay string
	PIN   string
	Key   sessionnet.Key
	// Context bounds the outbound tunnel; nil means context.Background.
	Context context.Context
	// Direct, when true, ALSO opens a machine-addressed listener speaking the
	// same handshake, and publishes its address so a client that can reach this
	// machine skips the relay (plan 38 step 4). It uses Server.Bind —
	// the plain HTTP listener is always loopback and is never this path.
	Direct bool
	// Ticket returns a current relay ticket for the git service (ticket 17).
	Ticket func() (string, error)
}

// Start listens on loopback and serves at http://127.0.0.1:<port>/<name>.
// name defaults to "canonical.git". The plain listener is this machine's own
// git origin only; off-box traffic arrives via Session (relay or direct).
func (s *Server) Start(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return s.url, nil
	}
	if name == "" {
		name = "canonical.git"
	}
	s.name = name
	abs, err := filepath.Abs(s.Bare)
	if err != nil {
		return "", err
	}
	s.Bare = abs

	mux := http.NewServeMux()
	prefix := "/" + name
	mux.HandleFunc(prefix+"/info/refs", s.infoRefs)
	mux.HandleFunc(prefix+"/git-upload-pack", s.service("upload-pack"))
	mux.HandleFunc(prefix+"/git-receive-pack", s.service("receive-pack"))
	mux.HandleFunc("/logs", s.logs)
	mux.HandleFunc(prefix+"/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	// Plain listener: ephemeral loopback port. Bind / BindForControl govern the
	// direct session-network listener below, never this door.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	s.ln = ln
	srv := &http.Server{Handler: mux}
	s.srv = srv
	port := ln.Addr().(*net.TCPAddr).Port
	s.url = fmt.Sprintf("http://%s/%s", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), name)
	go func() { _ = srv.Serve(ln) }()

	// The session-network listener serves the SAME mux. There is one git
	// server; the session network is another way in, not another endpoint —
	// which is what keeps a local dev loop and a cross-network session on
	// exactly one code path.
	if s.Session != nil {
		if err := s.publishSessionLocked(); err != nil {
			_ = ln.Close()
			s.ln = nil
			return "", err
		}
	}
	return s.url, nil
}

// UnpublishSession tears the session-network registration down while leaving
// the loopback git listener up. Placement Stop must call this when the git
// lease moves: a live-but-lease-less registration plus "first live holder wins"
// on the relay would lock the real owner out (abuse-surface ticket 15).
func (s *Server) UnpublishSession() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionLn == nil {
		return
	}
	_ = s.sessionLn.Close()
	s.sessionLn = nil
	s.sessionURL = ""
	s.sessionDirect = ""
}

// PublishSession (re)joins the session network. Used when this machine takes
// the git lease back after a stand-down — Start is a no-op once the loopback
// listener exists, so republishing has to be its own call.
func (s *Server) PublishSession() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Session == nil {
		return nil
	}
	if s.sessionLn != nil {
		return nil
	}
	if s.srv == nil {
		return fmt.Errorf("gitserver: PublishSession before Start")
	}
	return s.publishSessionLocked()
}

// publishSessionLocked assumes s.mu is held and s.srv is running.
func (s *Server) publishSessionLocked() error {
	ctx := s.Session.Context
	if ctx == nil {
		ctx = context.Background()
	}
	hc := sessionnet.HolderConfig{
		Relay: s.Session.Relay, PIN: s.Session.PIN,
		Service: "git", Key: s.Session.Key,
		Ticket: s.Session.Ticket,
	}
	if s.Session.Direct {
		dln, advHost, derr := netbind.ListenAdvertise(s.Bind, 0)
		if derr != nil {
			return fmt.Errorf("direct session listener: %w", derr)
		}
		hc.DirectListener = dln
		hc.DirectAdvertise = net.JoinHostPort(advHost, strconv.Itoa(dln.Addr().(*net.TCPAddr).Port))
	}
	sln, err := sessionnet.Serve(ctx, hc)
	if err != nil {
		return fmt.Errorf("publish canonical onto the session network: %w", err)
	}
	s.sessionLn = sln
	name := s.name
	if name == "" {
		name = "canonical.git"
	}
	s.sessionURL = sessionnet.FormatURL(s.Session.PIN, "git", name)
	s.sessionDirect = sessionnet.DirectAddr(sln)
	go func() { _ = s.srv.Serve(sln) }()
	return nil
}

// SessionURL is the address to publish into the control plane when this server
// is on the session network — empty otherwise. It names the session and its
// service, never a machine, so it survives the git lease moving.
func (s *Server) SessionURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionURL
}

// SessionDirect is the machine address peers may dial to reach this same server
// without the relay hop, or "" when none is published.
func (s *Server) SessionDirect() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionDirect
}

func (s *Server) infoRefs(w http.ResponseWriter, r *http.Request) {
	svc := r.URL.Query().Get("service")
	if svc != "git-upload-pack" && svc != "git-receive-pack" {
		http.Error(w, "unsupported service", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/x-"+svc+"-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	var buf bytes.Buffer
	pktWrite(&buf, "# service="+svc+"\n")
	buf.WriteString("0000")
	cmd := s.gitCmd(r.Context(), strings.TrimPrefix(svc, "git-"), "--stateless-rpc", "--advertise-refs", s.Bare)
	cmd.Stdout = &buf
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		http.Error(w, errBuf.String(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(buf.Bytes())
}

// gitFrameBuffer is how much upload-pack output is gathered before it reaches
// the session network. Bigger than a handful of 16 KiB records on purpose:
// net/http's own 4 KiB connection buffer emits one short write per chunk
// whatever we do, so a large chunk amortises that short frame across many full
// ones. 128 KiB puts the average frame within a whisker of the 16 KiB record
// ceiling; smaller buffers leave it near half.
const gitFrameBuffer = 128 << 10

// writerOnly hides every optimisation interface the wrapped writer has, so a
// copy has to go through Write. See its one use, above.
type writerOnly struct{ w io.Writer }

func (o writerOnly) Write(p []byte) (int, error) { return o.w.Write(p) }

func (s *Server) service(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := decodedRequestBody(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer body.Close()
		w.Header().Set("Content-Type", "application/x-git-"+name+"-result")
		w.Header().Set("Cache-Control", "no-cache")
		cmd := s.gitCmd(r.Context(), name, "--stateless-rpc", s.Bare)
		cmd.Stdin = body
		// Coalesced, not written straight through. git writes its pack in ~8 KiB
		// pieces, and on the session network every write becomes a WebSocket
		// FRAME — against a Durable Object's ~1,000 messages/second soft limit,
		// which a real clone brushes at that size. Buffering here turns those
		// pieces into full 16 KiB records; the measurement and the arithmetic
		// are in framerate_test.go.
		//
		// Safe because smart-HTTP is strictly request → response: this handler
		// is not an interactive stream, so there is nothing to deliver early and
		// no timer to flush on. The deferred flush is the whole of the tail.
		// writerOnly is load-bearing, not decoration: bufio.Writer.ReadFrom
		// hands the whole copy to the destination when the destination is an
		// io.ReaderFrom — and http.response is one — so without it os/exec's
		// io.Copy goes straight to net/http with a 32 KiB buffer, the buffer
		// below is never touched, and the measured frame size does not move.
		// That is exactly what happened on the first attempt at this fix.
		out := bufio.NewWriterSize(writerOnly{w}, gitFrameBuffer)
		cmd.Stdout = out
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf
		defer out.Flush()
		if err := cmd.Run(); err != nil {
			// Response may already be partially written; best-effort log.
			_, _ = io.WriteString(os.Stderr, "gitserver "+name+": "+errBuf.String()+"\n")
		}
	}
}

// decodedRequestBody is the body to hand to upload-pack/receive-pack, with any
// transport compression undone first. git's smart-HTTP client compresses a
// request body whenever compression wins and announces it with
// `Content-Encoding: gzip` — a decision it makes on its own, per request, based
// on size. Piping those bytes straight into `--stateless-rpc` is not a
// degraded fetch, it is a dead one: git reads gzip's magic number as a pkt-line
// length and dies with `protocol error: bad line length character`, which the
// client sees as `the remote end hung up unexpectedly`. It is invisible until a
// session's repo grows, because a small body (a first clone: a couple of wants
// and no haves) does not compress smaller and is sent plain.
func decodedRequestBody(r *http.Request) (io.ReadCloser, error) {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Content-Encoding")), "gzip") {
		return r.Body, nil
	}
	zr, err := gzip.NewReader(r.Body)
	if err != nil {
		return nil, fmt.Errorf("Content-Encoding: gzip but the body is not gzip: %w", err)
	}
	return zr, nil
}

// logs serves the current dev-server log buffer as plain text so a remote
// error-watcher can poll it. Empty when no log source is wired.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	stream, phase := r.URL.Query().Get("stream"), r.URL.Query().Get("phase")
	if raw := r.URL.Query().Get("since"); raw != "" && s.LogsSince != nil {
		cursor, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || cursor < 0 {
			http.Error(w, "since must be a cursor from a previous response", 400)
			return
		}
		st, ph := devserver.Stream(stream), devserver.Phase(phase)
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") && s.LogsSubscribe != nil {
			s.logsStream(w, r, cursor, st, ph)
			return
		}
		lines, next := s.LogsSince(cursor, st, ph)
		if lines == nil {
			lines = []devserver.Line{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(devserver.LogPage{Lines: lines, Cursor: next})
		return
	}
	if stream != "" || phase != "" {
		if s.LogsSelect == nil {
			return
		}
		_, _ = io.WriteString(w, s.LogsSelect(stream, phase))
		return
	}
	if s.Logs == nil {
		return
	}
	_, _ = io.WriteString(w, s.Logs())
}

func (s *Server) logsStream(w http.ResponseWriter, r *http.Request, cursor int64, stream devserver.Stream, phase devserver.Phase) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		lines, next := s.LogsSince(cursor, stream, phase)
		if lines == nil {
			lines = []devserver.Line{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(devserver.LogPage{Lines: lines, Cursor: next})
		return
	}
	sub := s.LogsSubscribe(cursor, stream, phase)
	if sub == nil {
		http.Error(w, "log stream unavailable", 503)
		return
	}
	defer sub.Close()
	closing := s.closingCh()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-closing:
			return
		case page, ok := <-sub.C:
			if !ok {
				return
			}
			writeLogSSE(w, page)
			flusher.Flush()
		case <-ticker.C:
			_, _ = io.WriteString(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func writeLogSSE(w http.ResponseWriter, page devserver.LogPage) {
	b, _ := json.Marshal(page)
	fmt.Fprintf(w, "event: page\ndata: %s\n\n", b)
}

func (s *Server) gitCmd(ctx context.Context, args ...string) *exec.Cmd {
	bin, err := sbGit.Bin()
	if err != nil {
		// Return a command that will fail predictably.
		cmd := exec.CommandContext(ctx, "false")
		return cmd
	}
	full := make([]string, 0, len(sbGit.HermeticConfig())*2+len(args))
	for _, kv := range sbGit.HermeticConfig() {
		full = append(full, "-c", kv)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, bin, full...)
	cmd.Env = sbGit.Env()
	return cmd
}

func pktWrite(w io.Writer, s string) {
	fmt.Fprintf(w, "%04x%s", len(s)+4, s)
}

// URL returns the clone/push URL once Start has succeeded.
func (s *Server) URL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.url
}

// Close shuts down the HTTP listener.
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil {
		return nil
	}
	// Held /logs streams first. Shutdown waits for active handlers to return,
	// and a subscriber blocks until its request context is done — which never
	// happens on a graceful shutdown. Without this, a host with one console or
	// one off-box error-watcher attached can never close its git server.
	if s.closing != nil {
		close(s.closing)
		s.closing = nil
	}
	// The session listener next: Shutdown waits for Serve to return, and Serve
	// only returns once its listener's Accept errors.
	if s.sessionLn != nil {
		_ = s.sessionLn.Close()
		s.sessionLn = nil
	}
	err := s.srv.Shutdown(ctx)
	s.srv = nil
	s.ln = nil
	s.url = ""
	s.sessionURL = ""
	return err
}
