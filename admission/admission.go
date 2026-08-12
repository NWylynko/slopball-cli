package admission

import (
	"context"
	"fmt"
	"strings"

	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/logx"
)

// LogEvents writes join-request arrivals and decisions to the admission
// component. The fallback path (--no-console / --once / non-TTY) has no users
// tab, so these lines are how a host notices a knock without already
// suspecting one (plan 44 ticket 10).
func LogEvents(events []controlplane.Event) {
	if len(events) == 0 {
		return
	}
	log := logx.New("admission")
	for _, e := range events {
		switch e.Kind {
		case "member.knock":
			who := payload(e, "name")
			if m := payload(e, "machine"); m != "" {
				who += "@" + m
			}
			log.Infof("join request from %s — `slopball members accept %s`", who, payload(e, "id"))
		case "member.declined":
			log.Infof("join request from %s declined", payload(e, "name"))
		case "member.joined":
			// Invites also emit member.joined; only an acceptance carries acceptedBy.
			if payload(e, "acceptedBy") == "" {
				continue
			}
			log.Infof("accepted %s into the session", payload(e, "name"))
		}
	}
}

func payload(e controlplane.Event, key string) string {
	s, _ := e.Payload[key].(string)
	return strings.TrimSpace(s)
}

// WaitLine is the joiner-facing sentence while a redeem is held.
func WaitLine(pin string, acceptors []string) string {
	who := strings.Join(acceptors, ", ")
	if who == "" {
		who = "nobody human is in the session yet"
	}
	return fmt.Sprintf("waiting to be let into %s — anyone already in the session can accept (%s)", pin, who)
}

// Log carries the admission line for a session, and knows the two ways events
// reach it: pushed down the SSE stream, or fetched when that stream is dark.
type Log struct {
	Control *controlplane.Client
	PIN     string
	cursor  int64
}

// Stream folds in events that arrived on the session stream.
func (l *Log) Stream(events []controlplane.Event) {
	l.advance(events)
	LogEvents(events)
}

// Poll fetches what the stream would have delivered. Callers run it on the
// member cycle only when no stream is live — with a live stream the frames
// already carry these events, and polling as well would put a request per cycle
// back into the steady state plan 43 exists to have emptied.
//
// The cursor is shared with Stream, so a stream that drops mid-session picks up
// where the frames stopped instead of re-announcing knocks already logged.
func (l *Log) Poll(ctx context.Context) {
	if l == nil || l.Control == nil || l.PIN == "" {
		return
	}
	events, err := l.Control.EventsSince(ctx, l.PIN, l.cursor)
	if err != nil || len(events) == 0 {
		return
	}
	l.advance(events)
	LogEvents(events)
}

func (l *Log) advance(events []controlplane.Event) {
	for _, e := range events {
		if e.Seq > l.cursor {
			l.cursor = e.Seq
		}
	}
}
