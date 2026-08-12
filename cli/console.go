package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/nwylynko/slopball-cli/canonical"
	"github.com/nwylynko/slopball-cli/console"
	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/reach"
	"github.com/nwylynko/slopball-cli/session"
	"github.com/nwylynko/slopball-cli/syncengine"
	"github.com/spf13/cobra"
)

// consoleSession is one member's live screen plus the session work behind it.
//
// There is deliberately no `slopball console <pin>` verb: the console IS what
// `slopball` and `slopball join` show instead of the log scroll. One screen per
// session per machine, nothing new to discover, and the thing on screen is the
// thing holding the session open — which is also why quitting it leaves the
// session exactly as Ctrl-C does.
type consoleSession struct {
	PIN      string
	Me       string
	MemberID string
	Branch   string
	WorkPath string
	Elector  bool
	// Leave is what a confirmed quit runs. Both callers route it through the
	// shutdown they already had (Joined.Close / Running.Stop), which hands this
	// member's leases back so the next-best member picks them up at once rather
	// than waiting out a lease TTL.
	Leave func(context.Context) error
	// Work is the session itself, run on its own goroutine while the console
	// holds the main one. It narrates through the writer it is given.
	//
	// Everything slow lives in here — the box provision, the scaffold, the first
	// install — because the console draws before any of it runs. A human's first
	// frame after the last question is the dashboard, not a wall of standup
	// narration, and the narration lands in a tab instead of scrolling past.
	Work func(ctx context.Context, out io.Writer) error
	// Announce is how Work reports the facts the standup resolves — this
	// member's branch and work tree — to a screen that is already up.
	Announce *announcer
}

// leaver is the console's Quit and the cleanup behind it, which are one act:
// every daemon's own Close already hands its leases back (Placement.ReleaseAll,
// which releases what this member actually holds), drops the member and clears
// the live marker. So leaving IS that call, in all three verbs, rather than a
// hand-rolled lease loop beside it.
//
// It exists because the standup now runs behind the screen: the daemon to close
// does not exist when the console is constructed, and a human can quit before
// it does. holds() after a quit therefore closes immediately rather than
// registering something nobody will ever stop.
type leaver struct {
	mu     sync.Mutex
	close  func()
	closed bool
}

// holds registers the daemon this run started.
func (l *leaver) holds(close func()) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		close() // quit landed mid-standup; stop what it could not have stopped
		return
	}
	l.close = close
	l.mu.Unlock()
}

// Leave stops it. Idempotent: the console's confirmed quit and the deferred
// cleanup behind it both call this, and the second one must be a no-op rather
// than a second LeaveMember.
func (l *leaver) Leave(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if l.close != nil {
		l.close()
	}
	return nil
}

// announcer is the one-way channel from the standup to the screen in front of
// it. Sends never block: the fallback path has no console draining this, and a
// standup that wedged on a screen that is not there would be worse than a
// dashboard missing a branch name.
type announcer struct {
	ch   chan console.MemberMsg
	path atomic.Pointer[string]
}

func newAnnouncer() *announcer { return &announcer{ch: make(chan console.MemberMsg, 8)} }

func (a *announcer) Announce(msg console.MemberMsg) {
	if a == nil {
		return
	}
	if msg.WorkPath != "" {
		p := msg.WorkPath
		a.path.Store(&p)
	}
	select {
	case a.ch <- msg:
	default:
	}
}

// WorkPath is where this member edits, once the standup has said. The behind
// count reads it from here rather than capturing it, because the tree does not
// exist when the console is constructed.
func (a *announcer) WorkPath() string {
	if a == nil {
		return ""
	}
	if p := a.path.Load(); p != nil {
		return *p
	}
	return ""
}

// runConsole draws the console, or falls back to running the work exactly as it
// runs today when there is no terminal to draw on. `--once`, CI, the emulator
// and piped output must be byte-identical to before, so the fallback is a plain
// call with the caller's own stdout — no interception, no reformatting.
func runConsole(ctx context.Context, cmd *cobra.Command, cs consoleSession) error {
	if entered := testHooks().ConsoleEntered; entered != nil {
		entered(ConsoleUp{PIN: cs.PIN, Leave: cs.Leave})
	}
	noConsole, _ := cmd.Flags().GetBool("no-console")
	stdin, _ := cmd.InOrStdin().(*os.File)
	if !console.Interactive(noConsole, os.Stdout, stdin) {
		if cs.Work == nil {
			return nil
		}
		return cs.Work(ctx, cmd.OutOrStdout())
	}

	client := controlClient(cmd)
	if cs.MemberID == "" && cs.PIN != "" {
		if s, err := session.Load(cs.PIN); err == nil {
			cs.MemberID = s.MemberID
		}
	}
	var livePIN atomic.Value
	livePIN.Store(cs.PIN)
	currentPIN := func() string {
		v, _ := livePIN.Load().(string)
		return v
	}
	awaitPIN := make(chan string, 1)
	if cs.PIN != "" {
		awaitPIN <- cs.PIN
	}
	// Where this member edits is either known already (a join has cloned) or
	// announced by the standup running behind the screen.
	workPath := func() string {
		if p := cs.Announce.WorkPath(); p != "" {
			return p
		}
		return cs.WorkPath
	}
	var announce <-chan console.MemberMsg
	if cs.Announce != nil {
		// PIN can arrive as news on create — fold it into the live name the
		// feed and admission closures read, then forward to the screen.
		// Sends on out block so a PIN/branch message is never dropped; the
		// screen drains this channel.
		out := make(chan console.MemberMsg, 8)
		go func() {
			defer close(out)
			for msg := range cs.Announce.ch {
				if msg.PIN != "" {
					livePIN.Store(msg.PIN)
					select {
					case awaitPIN <- msg.PIN:
					default:
					}
				}
				out <- msg
			}
		}()
		announce = out
	}
	return console.Run(ctx, console.Options{
		PIN: cs.PIN, Me: cs.Me, MemberID: cs.MemberID, Branch: cs.Branch, WorkPath: cs.WorkPath,
		Elector: cs.Elector, Quit: cs.Leave, Announce: announce,
		Decide: func(ctx context.Context, id, decision string) error {
			_, err := client.DecideMember(ctx, currentPIN(), id, decision)
			return err
		},
		SetAccess: func(ctx context.Context, access string) error {
			return client.SetAccess(ctx, currentPIN(), access)
		},
	}, console.Feed{
		Control: client, PIN: cs.PIN, AwaitPIN: awaitPIN,
		LogsURL: func(ctx context.Context) (string, error) { return logsEndpoint(ctx, client, currentPIN()) },
		DevURL:  func(ctx context.Context) (string, error) { return devEndpoint(ctx, client, currentPIN()) },
		Box: func(ctx context.Context, sess controlplane.Session) (console.BoxMsg, error) {
			pin := currentPIN()
			return console.BoxMsg{
				Git: reach.ProbeSessionService(ctx, client, sess, pin, reach.ServiceGit),
				Dev: reach.ProbeSessionService(ctx, client, sess, pin, reach.ServiceDev),
			}, nil
		},
		Behind: func(ctx context.Context) (int, error) { return BehindMain(ctx, currentPIN(), workPath()) },
	}, cs.Work)
}

// BehindMain is how far this member's tree trails main, and "not yet" is one of
// its answers. The console draws before the standup that creates the work tree
// and the mirror, so for the first seconds there is no repo to count against —
// nothing to say, rather than a failure. Reporting it put a git fatal on a
// dashboard whose box line read "git ok", and there it stayed.
//
// A repo that exists and cannot be read is a different thing and still errors:
// that means something built it wrong, which is worth saying.
//
// Exported because that three-way answer — not yet / a number / an error — is a
// decision rule with its own test, and the only production caller is a closure
// inside a TUI feed that no test can drive.
func BehindMain(ctx context.Context, pin, work string) (int, error) {
	main := mainRepoFor(pin)
	if work == "" || main == "" {
		return 0, nil
	}
	if _, err := os.Stat(filepath.Join(work, ".git")); err != nil {
		return 0, nil
	}
	return syncengine.CommitsBehindMain(ctx, work, main)
}

// mainRepoFor is the join daemon's mirror on a client and canonical bare on a
// host — or "" when the standup has not created either yet.
func mainRepoFor(pin string) string {
	p := session.ForPin(pin)
	bare := filepath.Join(p.Canonical, canonical.BareDir)
	if _, err := os.Stat(bare); err == nil {
		return bare
	}
	if _, err := os.Stat(p.Mirror); err == nil {
		return p.Mirror
	}
	return ""
}

// logsEndpoint is where the dev server's log cursor lives right now. Resolved
// per tick because the dev service can move to another member mid-session.
func logsEndpoint(ctx context.Context, client *controlplane.Client, pin string) (string, error) {
	if client == nil {
		return "", nil
	}
	// Resolved, not raw: a `slop://` address is not something http.Get can
	// dial, and the dev tab reading "no dev-server output yet" forever was that
	// bug wearing a placeholder (plan 40).
	url, err := client.EndpointURL(ctx, pin, controlplane.EndpointLogs)
	var missing *controlplane.NoEndpointError
	if errors.As(err, &missing) {
		// Nothing published yet — early in a session that is not a failure, and
		// the tab's "no dev-server output yet" is the honest thing to show.
		return "", nil
	}
	return url, err
}

// devEndpoint is the dev server as a URL this machine can open. It goes through
// the resolver rather than the endpoint map because a URL printed for a human
// to click is a dial too — `slop://<pin>/dev/` in a browser is the whole bug
// plan 41 exists to fix, and "printed, not dialled" stopped being an excuse for
// the dev endpoint specifically.
func devEndpoint(ctx context.Context, client *controlplane.Client, pin string) (string, error) {
	if client == nil {
		return "", nil
	}
	url, err := client.EndpointURL(ctx, pin, controlplane.EndpointDev)
	var missing *controlplane.NoEndpointError
	if errors.As(err, &missing) {
		// Nothing published yet — normal early in a session, and the dashboard's
		// "no dev server up yet" is the honest thing to show.
		return "", nil
	}
	return url, err
}

// workPathFor is where this member's agent edits — the one path a human on the
// dashboard actually needs.
func workPathFor(pin string) string { return session.ForPin(pin).Work }
