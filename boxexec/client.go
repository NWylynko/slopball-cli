package boxexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"github.com/nwylynko/slopball-cli/sessionnet"
)

// Client runs commands on the session's box over the session network.
type Client struct {
	Dialer *sessionnet.Dialer
}

type connWriter struct {
	net.Conn
	mu sync.Mutex
}

func (c *connWriter) writeMsg(typ byte, body []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return writeMsg(c.Conn, typ, body)
}

// Run executes argv in the box work tree, streaming stdio until exit.
func (c *Client) Run(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if c == nil || c.Dialer == nil {
		return -1, errors.New("boxexec: no dialer")
	}
	if len(argv) == 0 {
		return -1, errors.New("boxexec: empty argv")
	}
	conn, err := c.Dialer.Dial(ctx, Service)
	if err != nil {
		return -1, err
	}
	defer conn.Close()
	w := &connWriter{Conn: conn}

	if err := w.writeMsg(msgRun, appendArgv(nil, argv)); err != nil {
		return -1, err
	}

	stdinDone := make(chan struct{})
	if stdin != nil {
		go func() {
			defer close(stdinDone)
			buf := make([]byte, 32<<10)
			for {
				select {
				case <-ctx.Done():
					_ = w.writeMsg(msgStdinEOF, nil)
					return
				default:
				}
				n, err := stdin.Read(buf)
				if n > 0 {
					if werr := w.writeMsg(msgStdin, buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					_ = w.writeMsg(msgStdinEOF, nil)
					return
				}
			}
		}()
	} else {
		close(stdinDone)
		_ = w.writeMsg(msgStdinEOF, nil)
	}

	var exitCode int
	var runErr error
	for {
		typ, body, err := readMsg(conn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				if runErr == nil {
					runErr = errors.New("boxexec: connection closed before exit status")
				}
				break
			}
			return -1, err
		}
		switch typ {
		case msgStdout:
			if stdout != nil {
				_, _ = stdout.Write(body)
			}
		case msgStderr:
			if stderr != nil {
				_, _ = stderr.Write(body)
			}
		case msgExit:
			exitCode, runErr = readExit(body)
		case msgError:
			msg, _, _ := readString(body)
			runErr = errors.New(msg)
		default:
			runErr = fmt.Errorf("boxexec: unexpected message %d", typ)
		}
		if typ == msgExit || typ == msgError {
			break
		}
	}
	<-stdinDone
	if runErr != nil {
		return exitCode, runErr
	}
	if exitCode != 0 {
		return exitCode, &ExitError{Code: exitCode}
	}
	return 0, nil
}

// ExitError carries a remote command's non-zero exit status.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("command exited with status %d", e.Code) }

// RunMain is Run with stdin nil and os stdout/stderr.
func (c *Client) RunMain(ctx context.Context, argv []string) (int, error) {
	code, err := c.Run(ctx, argv, nil, os.Stdout, os.Stderr)
	if err != nil {
		return code, err
	}
	if code != 0 {
		return code, &ExitError{Code: code}
	}
	return 0, nil
}

// Shutdown asks the live box to stop hosting and exit.
func (c *Client) Shutdown(ctx context.Context) error {
	if c == nil || c.Dialer == nil {
		return errors.New("boxexec: no dialer")
	}
	conn, err := c.Dialer.Dial(ctx, Service)
	if err != nil {
		return err
	}
	defer conn.Close()
	return writeMsg(conn, msgShutdown, nil)
}
