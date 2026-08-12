package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/session"
	"github.com/nwylynko/slopball-cli/telemetry"
)

// `slopball report` is retroactive opt-in for ONE session (plan 46 ticket 15).
//
// A human whose session just broke runs one command, everything about that
// session is uploaded, and they get back an id to quote. It is human-run, one
// shot, one session — and it works on a machine with telemetry off, without
// turning it on, because deciding to send this session is not deciding to send
// every session.
//
// There is no unauthenticated ingest door: it exchanges the member Bearer this
// machine already holds on disk for a FRESH telemetry ticket, which is also
// why the 1h ticket TTL is not the limit on how long after the fact you can
// run it. Past session expiry (~3h) the session row and its members are gone,
// so nothing can mint or verify a ticket and it says so.
func newReportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Upload everything about this session so somebody can look at why it broke",
		Args:  cobra.NoArgs,
		RunE:  runReport,
	}
	addPinFlag(cmd)
	cmd.Flags().Int("log-lines", 400, "how much of this machine's session log to include")
	return cmd
}

// reportSection is one gathered source. A source that could not be read keeps
// its place and carries WHY — an absent section that simply vanished reads as
// "the session was silent", which is the opposite of what happened.
type reportSection struct {
	Name string
	Body string
	Err  string
}

func runReport(cmd *cobra.Command, _ []string) error {
	pin := resolvePin(cmd)
	if pin == "" {
		return fmt.Errorf("no session pin — run inside the session work tree, or pass --pin / set SLOPBALL_PIN")
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out := cmd.OutOrStdout()
	client := controlClient(cmd)

	id, secret := session.ReadMembership(pin)
	if id == "" || secret == "" {
		return fmt.Errorf("this machine is not a member of %s, so it cannot report on it — run `slopball report` on a machine that joined the session", pin)
	}

	// The exchange: the member Bearer this machine holds becomes a fresh
	// telemetry ticket. One member cycle does it, and it is the same call the
	// session makes every five seconds while it is alive.
	res, err := client.MemberSync(ctx, pin, id, controlplane.MemberSync{WantSnapshot: true})
	if err != nil {
		// An EXPIRED session and a membership that is no longer valid look the
		// same from here, and deliberately so: expiry deletes the session row
		// and its members together, so the credential this machine holds
		// resolves to nothing and the control plane answers unauthorized rather
		// than 404. Saying "expired" outright would be a guess; naming the
		// likely cause and the time bound is what an operator can act on.
		if errors.Is(err, controlplane.ErrNoSession) ||
			strings.Contains(err.Error(), "unknown or expired pin") ||
			strings.Contains(err.Error(), "unauthorized") {
			return fmt.Errorf("session %s is gone from the control plane, or this machine's membership of it is — "+
				"either way nothing can mint or verify a ticket for it any more, so there is nowhere to send a report. "+
				"Expired sessions take their members with them, so `slopball report` has to be run while the session is "+
				"still live (about 3 hours)", pin)
		}
		return fmt.Errorf("exchange this machine's membership for a telemetry ticket: %w", err)
	}
	ticket := res.RelayTickets[telemetry.TicketService]
	if ticket == "" {
		ticket = client.RelayTicket(pin, telemetry.TicketService)
	}
	if ticket == "" {
		return fmt.Errorf("the control plane minted no telemetry ticket for %s, so there is nothing to authenticate an upload with "+
			"(the deployment may be older than this binary)", pin)
	}

	sess := res.Session
	if sess == nil {
		s, err := client.Session(ctx, pin)
		if err != nil {
			return fmt.Errorf("read session %s: %w", pin, err)
		}
		sess = &s
	}
	ingest := strings.TrimSpace(sess.TelemetryURL)
	if ingest == "" {
		return fmt.Errorf("session %s advertises no telemetry ingest, so there is nowhere to send a report "+
			"(the control plane needs %s set)", pin, telemetry.AdvertiseEnv)
	}

	lines, _ := cmd.Flags().GetInt("log-lines")
	sections := gatherReport(ctx, client, pin, *sess, lines)

	// The report id IS the trace id, so `WHERE trace_id = '<id>'` finds every
	// part of a bundle that had to be split across the 64 KiB body cap.
	reportID := newReportID()
	em := telemetry.New(telemetry.Config{
		URL: ingest, Bearer: ticket, Service: "client", Timeout: 30 * time.Second,
		Version: controlplane.ClientVersion,
	})
	for i, sec := range sections {
		body := sec.Body
		if sec.Err != "" {
			body = "could not gather this source: " + sec.Err
		}
		captured, truncated := telemetry.CaptureString(body)
		em.Emit("client.report", telemetry.Event{
			PIN: pin, SessionUID: sess.UID, Member: id, TraceID: reportID,
			Component: sec.Name, Body: captured, Truncated: truncated,
			Data: map[string]any{
				"section": sec.Name, "part": i + 1, "parts": len(sections),
				"gathered": sec.Err == "",
			},
		})
	}
	// Close drains: a one-shot command has to finish sending before it exits,
	// which is the one place the emitter's give-up policy is waited on rather
	// than fired and forgotten.
	em.Close()

	fmt.Fprintf(out, "reported %s as %s (%d sections)\n", pin, reportID, len(sections))
	fmt.Fprintf(out, "quote that id — everything about this session is under it:\n")
	fmt.Fprintf(out, "  SELECT * FROM envelopes WHERE trace_id = '%s' ORDER BY id;\n", reportID)
	return nil
}

func newReportID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "rp_unknown"
	}
	return "rp_" + hex.EncodeToString(b[:])
}

// gatherReport collects what slopdebug already gathers — control-plane state,
// leases, convergence, the /logs dev-server buffer, the box container log,
// canonical branch topology — plus this machine's own log tail.
//
// It runs from INSIDE the session, which is what lets it read sources
// slopdebug cannot: a `slop://` logs endpoint names a session role and only a
// process holding the session key can dial it.
func gatherReport(ctx context.Context, client *controlplane.Client,
	pin string, sess controlplane.Session, logLines int) []reportSection {
	var out []reportSection
	add := func(name, body string, err error) {
		s := reportSection{Name: name, Body: body}
		if err != nil {
			s.Err = err.Error()
		}
		out = append(out, s)
	}

	// The control plane's whole picture: endpoints, leases, conductor,
	// convergence, members — the same document `monitor` renders.
	add("session", asJSON(sess), nil)

	events, err := client.EventsSince(ctx, pin, 0)
	add("events", asJSON(events), err)

	// The dev server's own output, resolved through the session network.
	logsURL, lerr := client.EndpointURL(ctx, pin, controlplane.EndpointLogs)
	if lerr != nil {
		add("dev-logs", "", lerr)
	} else {
		body, err := httpGetText(ctx, logsURL)
		add("dev-logs", body, err)
	}

	// Canonical branch topology: who is unmerged, from whichever local copy
	// this machine has.
	add("git", gitTopology(pin), nil)

	// The box container's log, when this machine is the one that can read it.
	if sess.Box != nil && strings.TrimSpace(sess.Box.Target) != "" {
		body, err := boxLogs(sess.Box.Target, pin)
		add("box", body, err)
	} else {
		add("box", "", fmt.Errorf("no BYO box target on this session — a managed box's container log lives on the control plane's docker host"))
	}

	tail, err := session.ReadClientLogTail(pin, logLines*200)
	add("client-log", tail, err)
	return out
}

// gitTopology is the branch picture from whichever local copy exists.
func gitTopology(pin string) string {
	p := session.ForPin(pin)
	var b strings.Builder
	for _, repo := range []struct{ name, dir string }{
		{"canonical", p.Canonical}, {"mirror", p.Mirror}, {"work", p.Work},
	} {
		out, err := git.Output(context.Background(), repo.dir, "branch", "-vv", "--all")
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "--- %s (%s)\n%s\n", repo.name, repo.dir, out)
	}
	if b.Len() == 0 {
		return "(no local git repository for this session on this machine)"
	}
	return b.String()
}

func asJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("(could not encode: %v)", err)
	}
	return string(b)
}

func httpGetText(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return string(b), err
}

// boxLogs shells out to this binary's own `box logs`, which is where the
// ssh-to-a-BYO-box knowledge already lives.
func boxLogs(target, pin string) (string, error) {
	bin, err := os.Executable()
	if err != nil {
		return "", err
	}
	out, err := exec.Command(bin, "box", "logs", target, "--pin", pin).CombinedOutput()
	return string(out), err
}
