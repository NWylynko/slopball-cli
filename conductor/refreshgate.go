package conductor

import (
	"sync"
	"time"

	"github.com/nwylynko/slopball-cli/controlplane"
)

// RefreshGate decides when a conductor's *network* fetch should run (plan 43
// ticket 08).
//
// The fleet keeps ticking every 2s — that is the merge hot path and it stays
// local git. What moves is `canonical.Host.Refresh` on a REMOTE host, which is
// a full `remote update --prune` across the session network: 30 ref-listings a
// minute over the relay for a session where nobody pushed. It now reacts to a
// `sync.pushed` frame, with a floor underneath it.
//
// The floor is the load-bearing half. §8.1 says the control plane must never be
// on the merge hot path, so the stream may only make a merge happen *sooner*,
// never be what makes it happen: with the control plane dark, merging continues
// at the floor instead of stopping. The floor's value is set by how much merge
// lag is acceptable when a signal is lost, not by traffic — six fetches a
// minute against today's thirty is already noise.
type RefreshGate struct {
	// Remote is false for an on-host conductor, which is never gated: its
	// Refresh is a no-op and its BranchesAheadOfMain is local git.
	Remote bool
	Floor  time.Duration
	// Frames carries pushed session updates. Nil is legal — the gate then runs
	// on its floor alone, which is exactly the no-control-plane case.
	Frames <-chan controlplane.SessionUpdate

	// StreamLive reports whether the session stream is currently delivering.
	// Nil reads as not-live, which is the honest answer for a gate that never
	// subscribed. Exported like every other knob on this struct: the gate is
	// built by hand as often as by NewRefreshGate, and this is the input to the
	// one degradation rule it owns — a caller that cannot set it cannot express
	// "the stream is healthy", and a test that cannot set it cannot drive the
	// collapse to MemberCycle at all except by sleeping through a real timeout.
	StreamLive func() bool

	mu   sync.Mutex
	last time.Time
}

// NewRefreshGate subscribes to the session stream for pin. A nil client (or an
// empty pin) yields a gate that runs on its floor.
func NewRefreshGate(client *controlplane.Client, pin string, remote bool) *RefreshGate {
	g := &RefreshGate{Remote: remote, Floor: controlplane.RemoteConductorFloor}
	// A local conductor is never gated, so it has nothing to subscribe to — and
	// a subscription nobody drains is just a queue to keep bounded.
	if remote && client != nil && pin != "" {
		g.Frames = client.Updates(pin)
		g.StreamLive = func() bool { return client.StreamLive(pin) }
	}
	return g
}

// Due reports whether the network refresh should run now, draining whatever
// frames have arrived since the last call.
func (g *RefreshGate) Due(now time.Time) bool {
	if g == nil || !g.Remote {
		return true
	}
	pushed := g.drain()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last.IsZero() || pushed || now.Sub(g.last) >= g.floor() {
		g.last = now
		return true
	}
	return false
}

// floor is RemoteConductorFloor with a healthy stream and MemberCycle without
// one — the same collapse every other floor in the plan makes, so there is no
// second fallback path to rot.
func (g *RefreshGate) floor() time.Duration {
	f := g.Floor
	if f <= 0 {
		f = controlplane.RemoteConductorFloor
	}
	live := g.StreamLive != nil && g.StreamLive()
	return controlplane.Floor(f, live)
}

// drain reads whatever has arrived without blocking and reports whether any of
// it means somebody's work reached the hub.
func (g *RefreshGate) drain() bool {
	pushed := false
	for {
		select {
		case u, ok := <-g.Frames:
			if !ok {
				g.Frames = nil
				return pushed
			}
			for _, e := range u.Events {
				switch e.Kind {
				case controlplane.EventSyncPushed, controlplane.EventMergeApplied:
					pushed = true
				}
			}
		default:
			return pushed
		}
	}
}
