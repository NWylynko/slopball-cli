package boxexec

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"

	"github.com/nwylynko/slopball-cli/logx"
	"github.com/nwylynko/slopball-cli/sessionnet"
)

// Service is the session-network service name for box command execution.
const Service = "exec"

// HolderOptions publishes box exec onto the session network.
type HolderOptions struct {
	Relay      string
	PIN        string
	Key        sessionnet.Key
	WorkDir    string
	OnShutdown func()
	// Ticket returns a current relay ticket for the exec service (ticket 17).
	Ticket func() (string, error)
}

// Holder serves remote argv execution for the session's box.
type Holder struct {
	ln     net.Listener
	cancel context.CancelFunc
	once   sync.Once
	opt    HolderOptions
}

// StartHolder publishes exec and runs commands in WorkDir for each connection.
func StartHolder(ctx context.Context, opt HolderOptions) (*Holder, error) {
	if opt.Relay == "" {
		return nil, errors.New("boxexec: no relay address")
	}
	if opt.WorkDir == "" {
		return nil, errors.New("boxexec: no work directory")
	}
	if opt.Key.Zero() {
		return nil, fmt.Errorf("boxexec: serving %s needs the session key", Service)
	}
	ctx, cancel := context.WithCancel(ctx)
	ln, err := sessionnet.Serve(ctx, sessionnet.HolderConfig{
		Relay: opt.Relay, PIN: opt.PIN, Service: Service, Key: opt.Key,
		Ticket: opt.Ticket,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("publish box exec onto the session network: %w", err)
	}
	h := &Holder{ln: ln, cancel: cancel, opt: opt}
	go h.accept(ctx)
	logx.New("boxexec").Infof("box exec published on the session network for %s", opt.PIN)
	return h, nil
}

// Close stops publishing exec.
func (h *Holder) Close() error {
	if h == nil {
		return nil
	}
	h.once.Do(func() {
		h.cancel()
		_ = h.ln.Close()
	})
	return nil
}

func (h *Holder) accept(ctx context.Context) {
	log := logx.New("boxexec")
	for {
		c, err := h.ln.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			defer conn.Close()
			if err := h.serveConn(ctx, conn); err != nil && ctx.Err() == nil {
				log.Debugf("exec connection: %v", err)
			}
		}(c)
	}
}

func (h *Holder) serveConn(ctx context.Context, conn net.Conn) error {
	typ, body, err := readMsg(conn)
	if err != nil {
		return err
	}
	switch typ {
	case msgShutdown:
		if h.opt.OnShutdown != nil {
			h.opt.OnShutdown()
		}
		return nil
	case msgRun:
		argv, err := readArgv(body)
		if err != nil {
			_ = writeError(conn, err)
			return err
		}
		return h.run(ctx, conn, argv)
	default:
		return fmt.Errorf("boxexec: unexpected first message %d", typ)
	}
}

func (h *Holder) run(ctx context.Context, conn net.Conn, argv []string) error {
	if len(argv) == 0 {
		return writeError(conn, errors.New("empty argv"))
	}
	// Unattributable by policy (ticket 22): the session key authorises the
	// call, and an incident review can say what ran and from where — never who.
	src := ""
	if addr := conn.RemoteAddr(); addr != nil {
		src = addr.String()
	}
	logx.New("boxexec").Infof("exec argv=%q source=%s", argv, src)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wmu sync.Mutex
	write := func(typ byte, body []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return writeMsg(conn, typ, body)
	}

	stdinReader, stdinWriter := io.Pipe()

	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = h.opt.WorkDir
	cmd.Stdin = stdinReader
	// Framing writers rather than StdoutPipe/StderrPipe, deliberately.
	//
	// Wait closes the pipes those return as soon as the process exits, and Go
	// says outright that it is "incorrect to call Wait before all reads from
	// the pipe have completed". Reading them on their own goroutines alongside
	// Wait lost that race often enough to be a real defect: `slopball box run`
	// returned the correct exit status with no output, on a loaded box, which
	// is exactly when somebody is running a command to find out what broke.
	//
	// With cmd.Stdout/cmd.Stderr set, os/exec owns the pipe and Wait does not
	// return until its copies are done. (The documented cost is that a command
	// which daemonises a child holding the fd keeps Wait blocked — the caller's
	// context is the bound on that, and losing output silently was worse.)
	cmd.Stdout = &msgWriter{write: write, typ: msgStdout}
	cmd.Stderr = &msgWriter{write: write, typ: msgStderr}
	if err := cmd.Start(); err != nil {
		_ = write(msgError, appendString(nil, err.Error()))
		return err
	}

	go h.pumpStdin(conn, stdinWriter, cancel, func() {})

	code := 0
	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			_ = write(msgError, appendString(nil, err.Error()))
			code = 1
		}
	}
	_ = stdinWriter.Close()
	_ = stdinReader.Close()
	if code < 0 {
		// Killed by a signal: no exit status of its own, so report failure
		// rather than a negative number the client cannot act on.
		code = 1
	}
	var body [4]byte
	binary.BigEndian.PutUint32(body[:], uint32(code))
	return write(msgExit, body[:])
}

// msgWriter frames whatever the command writes as one stream message. Writes
// are serialised by the caller's mutex, so stdout and stderr cannot interleave
// mid-frame.
type msgWriter struct {
	write func(byte, []byte) error
	typ   byte
}

func (m *msgWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := m.write(m.typ, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (h *Holder) pumpStdin(conn net.Conn, stdin *io.PipeWriter, cancel context.CancelFunc, _ func()) {
	defer stdin.Close()
	for {
		typ, body, err := readMsg(conn)
		if err != nil {
			cancel()
			return
		}
		switch typ {
		case msgStdin:
			if _, err := stdin.Write(body); err != nil {
				cancel()
				return
			}
		case msgStdinEOF:
			return
		default:
			cancel()
			return
		}
	}
}

func writeError(conn net.Conn, err error) error {
	if err == nil {
		return nil
	}
	return writeMsg(conn, msgError, appendString(nil, err.Error()))
}
