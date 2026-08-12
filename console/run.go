package console

import (
	"context"
	"io"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/devserver"
	"github.com/nwylynko/slopball-cli/logx"
)

// Feed is where the console's live data comes from. Everything here is a poll
// of something that already exists — the control plane for structured facts,
// /logs for the one bulk stream that legitimately travels.
type Feed struct {
	Control *controlplane.Client
	// PIN is the session name once known. On a fresh create it starts empty and
	// AwaitPIN delivers the minted name (abuse-surface ticket 11).
	PIN string
	// AwaitPIN receives the minted PIN when create returns. pump waits on it
	// rather than polling — the announce path is the signal.
	AwaitPIN <-chan string
	// LogsURL is resolved per tick rather than captured, because the dev
	// service can move to another member mid-session (plan 30). It returns the
	// resolution failure rather than "" so the dev tab can say why it is empty
	// — a placeholder that never changes is how a `slop://` URL nobody could
	// dial went unnoticed for a whole session (plan 40).
	LogsURL func(context.Context) (string, error)
	// DevURL is the dev endpoint resolved into something this machine can open,
	// re-resolved per tick for the same reason LogsURL is: dev is a placed
	// service and can move to another member mid-session.
	DevURL func(context.Context) (string, error)
	// Box probes git/dev reachability from this machine (~every 6s).
	Box func(context.Context, controlplane.Session) (BoxMsg, error)
	// Behind counts non-merge commits on main not yet in the local work tree.
	Behind func(context.Context) (int, error)
}

// Interval is how often the console redraws. Since plan 43 the session document
// and the work feed arrive pushed and the dev log is followed, so a tick costs
// nothing over the network — it only paces the two local reads (the behind
// count and the reachability probe) and the repaint.
const Interval = 2 * time.Second

// Interactive reports whether a console can be drawn at all. `--once`, CI, the
// emulator and piped output all fall back to the line log, whose output must
// stay byte-identical to what it has always been.
func Interactive(noConsole bool, out, in *os.File) bool {
	if noConsole {
		return false
	}
	return isCharDevice(out) && isCharDevice(in)
}

func isCharDevice(f *os.File) bool {
	if f == nil {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// Run draws the console until the user leaves or ctx is cancelled. It owns the
// terminal for its whole life, which is the point: the thing on screen is the
// thing holding the session open.
//
// While it runs, logx is diverted into the console's own buffers. Any stray
// write to stdout corrupts an alt-screen render, and slopball's loops narrate
// constantly — this is the piece that is easy to miss and guaranteed to be
// visible.
// work is the session itself — the scaffold, the fleet loop, the mirror
// daemon — run on its own goroutine because the console owns the main one. It
// is handed a writer to narrate through: anything it would have printed to
// stdout lands in the feed instead of corrupting the render.
func Run(ctx context.Context, opt Options, feed Feed, work func(ctx context.Context, out io.Writer) error) error {
	m := New(opt)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))

	restore := divert(p)
	defer restore()

	go pump(ctx, p, feed)
	go forward(ctx, p, opt.Announce)
	failed := make(chan error, 1)
	if work != nil {
		go func() {
			if err := work(ctx, &sendWriter{send: p.Send, component: "slopball"}); err != nil {
				// The standup runs behind this screen now, so its failures arrive
				// here rather than before the console existed. A session that never
				// came up has nothing to look at: take the screen down and let the
				// error land on the restored terminal, which is what it did when the
				// standup ran in front.
				failed <- err
				p.Quit()
			}
		}()
	}

	_, err := p.Run()
	select {
	case werr := <-failed:
		return werr
	default:
	}
	return err
}

// forward relays facts the standup resolves after the console is already
// drawing — this member's branch and work tree.
func forward(ctx context.Context, p *tea.Program, ch <-chan MemberMsg) {
	if ch == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			p.Send(msg)
		}
	}
}

// sendWriter turns writes into feed lines. This is what stops the console from
// silently swallowing output that used to be on the terminal — the alternative
// (discarding it) loses information exactly when a session is going wrong.
type sendWriter struct {
	send      func(tea.Msg)
	component string
	buf       strings.Builder
}

func (w *sendWriter) Write(b []byte) (int, error) {
	w.buf.Write(b)
	s := w.buf.String()
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		if line := strings.TrimSpace(s[:i]); line != "" {
			w.send(LogMsg{Component: w.component, Text: line})
		}
		s = s[i+1:]
	}
	w.buf.Reset()
	w.buf.WriteString(s)
	return len(b), nil
}

// divert points logx at the running program and returns the undo.
func divert(p *tea.Program) func() {
	return DivertTo(func(component, text string) {
		p.Send(LogMsg{Component: component, Text: text})
	})
}

// DivertTo captures logx for as long as the returned undo is uncalled, parsing
// logx's own line format back into a component and a message so a role's
// narration lands on that role's tab rather than in an undifferentiated scroll.
//
// Restoring matters as much as capturing: the fallback path narrates to stderr,
// and a console that closed without putting logx back would leave the rest of
// the process silent.
func DivertTo(emit func(component, text string)) func() {
	prev := logx.SetOutput(&logSink{emit: emit})
	return func() { logx.SetOutput(prev) }
}

type logSink struct {
	emit func(component, text string)
	buf  strings.Builder
}

func (l *logSink) Write(b []byte) (int, error) {
	l.buf.Write(b)
	s := l.buf.String()
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		l.line(s[:i])
		s = s[i+1:]
	}
	l.buf.Reset()
	l.buf.WriteString(s)
	return len(b), nil
}

// emit splits "15:04:05.000 INFO  merger merged client/ada" back into the
// component and the message. A line that does not match is still shown — a
// diverted logger that drops what it cannot parse is worse than an ugly line.
func (l *logSink) line(text string) {
	fields := strings.Fields(text)
	if len(fields) < 4 {
		l.emit("slopball", text)
		return
	}
	_, rest, _ := strings.Cut(strings.TrimSpace(text), fields[2])
	l.emit(fields[2], strings.TrimSpace(rest))
}

// pump polls every source the console reads and sends the results in. One
// goroutine, one ticker: the console is a viewer, and three competing pollers
// would only make it a noisier one.
func pump(ctx context.Context, p *tea.Program, feed Feed) {
	if feed.Control == nil {
		return
	}
	pin := feed.PIN
	if pin == "" {
		if feed.AwaitPIN == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case pin = <-feed.AwaitPIN:
			if pin == "" {
				return
			}
		}
	}
	var eventCursor int64
	tick := 0
	first := true
	t := time.NewTicker(Interval)
	defer t.Stop()

	// The work feed rides the session stream this process already holds, so the
	// console's redraw costs nothing (plan 43). With no live stream it falls
	// back to the paged GET on the same 2s redraw — the one degradation rule,
	// not a second source of truth.
	updates := feed.Control.Updates(pin)

	// The dev tab follows /logs rather than fetching a page per tick, and
	// re-resolves the endpoint per connection because dev is a placed service
	// that moves (plan 30). No filter: this is the merged human view.
	if feed.LogsURL != nil {
		logs := devserver.Follow(ctx, devserver.FollowOptions{
			URL:   feed.LogsURL,
			Floor: controlplane.LogsFloor,
		}, func() { p.Send(LogsResetMsg{}) })
		defer logs.Close()
		go func() {
			for page := range logs.C {
				if page.Missed > 0 {
					logx.New("console").Warnf("dev-server log stream missed %d line(s)", page.Missed)
				}
				if len(page.Lines) > 0 {
					p.Send(LogsMsg(page.Lines))
				}
			}
		}()
	}
	for {
		if !first {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
		first = false
		tick++

		// Every source reports its outcome every tick, success included. Sending
		// only on failure is what left a startup blip — the behind count asked
		// before the standup behind the screen had made a mirror to count
		// against — on the dashboard for the rest of the session.
		sess, err := feed.Control.Session(ctx, pin)
		p.Send(ErrorMsg{Source: "session", Err: err})
		wantPending := false
		if err == nil {
			p.Send(SessionMsg(sess))
			// First snapshot only — after that pending refreshes on knock events
			// (or every tick when the stream is dark). Never a steady-state poll.
			if tick == 1 {
				wantPending = true
			}
			if feed.Box != nil && tick%3 == 0 {
				if box, err := feed.Box(ctx, sess); err == nil {
					p.Send(box)
				}
			}
		}
		if feed.Behind != nil {
			n, err := feed.Behind(ctx)
			p.Send(ErrorMsg{Source: "behind", Err: err})
			if err == nil {
				p.Send(BehindMsg(n))
			}
		}
		if feed.Control.StreamLive(pin) {
			for drained := true; drained; {
				select {
				case u, ok := <-updates:
					if !ok {
						updates = nil
						drained = false
						continue
					}
					if len(u.Events) > 0 {
						if last := u.Events[len(u.Events)-1].Seq; last > eventCursor {
							eventCursor = last
						}
						p.Send(EventsMsg(u.Events))
						for _, e := range u.Events {
							switch e.Kind {
							case "member.knock", "member.declined", "member.joined", "member.left":
								wantPending = true
							}
						}
					}
				default:
					drained = false
				}
			}
		} else {
			// No live stream → same floor as Session: refresh pending each tick.
			wantPending = true
			if events, err := feed.Control.EventsSince(ctx, pin, eventCursor); err == nil && len(events) > 0 {
				eventCursor = events[len(events)-1].Seq
				p.Send(EventsMsg(events))
			}
		}
		if wantPending {
			if pending, perr := feed.Control.PendingMembers(ctx, pin); perr == nil {
				p.Send(PendingMsg(pending))
				p.Send(ErrorMsg{Source: "admission"}) // clear a prior fetch failure
			} else {
				p.Send(ErrorMsg{Source: "admission", Err: perr})
			}
		}
		if feed.DevURL != nil {
			if url, err := feed.DevURL(ctx); err == nil {
				p.Send(DevURLMsg(url))
			}
		}
		// The follower delivers lines on its own; this only reports whether the
		// endpoint can still be resolved, because "no dev-server output yet"
		// and "nobody can tell me where it is" must not look the same (plan 40).
		if feed.LogsURL != nil {
			_, err := feed.LogsURL(ctx)
			p.Send(ErrorMsg{Source: "logs", Err: err})
		}
	}
}
