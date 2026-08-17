package canonical

import (
	"fmt"
	"strings"

	"github.com/nwylynko/slopball-cli/sessionnet"
)

// gitDetailMax bounds how much of git's own error is carried into the sentence.
// The reason travels on a lease (controlplane.StartFailReasonMax, 400) and is
// rendered in a console cell, so the words a human acts on must not be the ones
// that get cut.
const gitDetailMax = 160

// RemoteCanonicalError is a failure to open or refresh a canonical that lives
// on another machine, said in one sentence a human can act on.
//
// Session 2lmymb (2026-08-17): a member whose feed said the conductor was off
// ran `slopball conductor` and got
//
//	open remote canonical http://127.0.0.1:62904/canonical.git: git remote
//	update --prune: fatal: unable to access 'http://127.0.0.1:59297/canonical
//	.git/': Failed to connect to 127.0.0.1 port 59297
//
// Two loopback ports — both of them this process's own forwarders, neither one
// an address the human has ever seen — and git's transport wording. There is no
// action in it. Worse, the failure git reports is often not the failure that
// happened: a forwarder that cannot reach a holder can only close the local
// connection, so "nobody in the session is serving git" arrives as
// `Recv failure: Connection reset by peer`.
//
// So the sentence is built from three things instead: the endpoint AS THE
// SESSION PUBLISHES IT, what actually went wrong (from the forwarder that made
// the attempt, where there is one), and the command that answers it.
type RemoteCanonicalError struct {
	PIN string
	// Endpoint is the session's git endpoint as it is published — the
	// `slop://<pin>/git/canonical.git` address or a machine URL. Never the
	// loopback forwarder the caller dialled.
	Endpoint string
	Err      error
}

// ExplainRemoteOpenFailure wraps err as the sentence above. It is the one place
// that phrasing lives: `slopball conductor` in a terminal and the join daemon's
// placement loop (whose reason rides the lease onto every console) both go
// through it, so the two screens cannot say different things about one failure.
func ExplainRemoteOpenFailure(pin, endpoint string, err error) error {
	if err == nil {
		return nil
	}
	return &RemoteCanonicalError{PIN: pin, Endpoint: endpoint, Err: err}
}

func (e *RemoteCanonicalError) Unwrap() error { return e.Err }

func (e *RemoteCanonicalError) Error() string {
	where := e.Endpoint
	if where == "" {
		where = "the address the control plane published for it"
	}
	return fmt.Sprintf("can't reach session %s's git at %s: %s", e.PIN, where, e.cause())
}

// cause is what happened, then what to do about it.
func (e *RemoteCanonicalError) cause() string {
	join := fmt.Sprintf("`slopball join %s`", e.PIN)
	monitor := fmt.Sprintf("`slopball monitor --pin %s`", e.PIN)
	onSessionNet := sessionnet.IsSessionURL(e.Endpoint)

	// The forwarder knows things git cannot: it is what talked to the relay.
	if why := forwarderReason(e.Endpoint); onSessionNet && why != "" {
		switch {
		case strings.Contains(why, "holder"):
			return "nobody in the session is serving git right now. Run " + monitor +
				" to see whether a member is taking it over — git moves to another machine on its own once one is there to take it."
		case strings.Contains(why, "ticket"):
			return "the session relay refused this machine (" + why + "). Rejoin the session with " + join + "."
		default:
			return "the session relay would not carry this connection (" + why + "). Run " + monitor + " to see what the session says."
		}
	}

	detail := gitFatalLine(e.Err)
	if nothingListening(detail) {
		if onSessionNet {
			return "this machine has no live connection to the session — nothing is listening on its local forwarder. " +
				"Is the join daemon running here? Start it with " + join + "."
		}
		return "nothing is listening at that address, so whoever published it has stopped serving git. Run " +
			monitor + " to see who holds git now."
	}
	return detail + ". Is the join daemon running here (" + join + "), and does " + monitor + " show somebody serving git?"
}

// forwarderReason is why this process's loopback forwarder for the endpoint's
// service last failed to reach a holder, "" if it has not failed.
func forwarderReason(endpoint string) string {
	pin, service, _, err := sessionnet.ParseURL(endpoint)
	if err != nil {
		return ""
	}
	ferr := sessionnet.LastForwardError(pin, service)
	if ferr == nil {
		return ""
	}
	return strings.TrimPrefix(flatten(ferr.Error()), "sessionnet: ")
}

// gitFatalLine is the line of a git failure a human needs — git says what went
// wrong on its `fatal:`/`error:` line and spends every other line on clone
// progress, template warnings and the path it was writing to.
func gitFatalLine(err error) string {
	lines := strings.Split(err.Error(), "\n")
	pick := flatten(lines[0])
	for _, l := range lines[1:] {
		if t := flatten(l); strings.HasPrefix(t, "fatal:") || strings.HasPrefix(t, "error:") {
			pick = t
			break
		}
	}
	if len(pick) > gitDetailMax {
		pick = pick[:gitDetailMax] + "…"
	}
	return strings.TrimSuffix(pick, ".")
}

func flatten(s string) string { return strings.Join(strings.Fields(s), " ") }

// nothingListening reads git's transport wording for "the port is closed",
// which is curl's across every platform git ships with.
func nothingListening(detail string) bool {
	for _, phrase := range []string{
		"Could not connect to server", "Couldn't connect to server",
		"Failed to connect", "Connection refused", "connection refused",
	} {
		if strings.Contains(detail, phrase) {
			return true
		}
	}
	return false
}
