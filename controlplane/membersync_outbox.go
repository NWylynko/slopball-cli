package controlplane

import (
	"context"
	"github.com/nwylynko/slopball-cli/telemetry"
	"sync"
)

// Outbox collects one member cycle's worth of state from writers on different
// goroutines and drains it in a single MemberSync. Level-triggered, never
// queued: a failed Flush is not buffered or replayed — the next cycle carries
// the current state, and a replayed stale endpoint is a lie (plan 43).
type Outbox struct {
	Client *Client
	PIN    string
	ID     string

	mu          sync.Mutex
	update      MemberUpdate
	generation  int
	hold        map[string]struct{}
	endpoints   map[string]EndpointPut
	convergence *Convergence
	conductor   *ConductorRecord
	cycles      int
}

func NewOutbox(c *Client, pin, id string) *Outbox {
	return &Outbox{
		Client: c, PIN: pin, ID: id,
		hold:      map[string]struct{}{},
		endpoints: map[string]EndpointPut{},
	}
}

// RegisterOutbox publishes this process's member cycle for pin, so anything
// else in the process with something to say — the conductor fleet, above all —
// deposits into it instead of opening its own connection.
//
// It hangs off the Client rather than being threaded through every
// constructor because that is already how the session's other per-pin
// machinery (Watch) is addressed, and because `controlClient` memoizes one
// client per base URL: a wizard's foreground conductor and the join daemon
// underneath it therefore find the same cycle with nothing plumbed between them.
func (c *Client) RegisterOutbox(pin string, o *Outbox) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.outboxes == nil {
		c.outboxes = map[string]*Outbox{}
	}
	c.outboxes[pin] = o
}

// UnregisterOutbox drops the cycle for pin — the daemon holding it is going
// away. It is also where this process's client telemetry stops (plan 46 ticket
// 13): the cycle is what pointed it at the session, so the same call takes the
// logx hook back out and drains what is queued.
func (c *Client) UnregisterOutbox(pin string) {
	telemetry.StopMember()
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.outboxes, pin)
}

// OutboxFor returns this process's member cycle for pin, or nil when it has
// none. Nil means "this process holds no membership" — `slopball conductor`
// run standalone — not "the cycle is broken", so a nil result publishes
// directly rather than dropping the write.
func (c *Client) OutboxFor(pin string) *Outbox {
	// Nil-receiver safe: plenty of roles are constructed with no control plane
	// at all (every mechanical-merge test), and asking whether there is a cycle
	// must not be the thing that panics.
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.outboxes[pin]
}

func (o *Outbox) SetMember(upd MemberUpdate) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.update = upd
}

func (o *Outbox) SetGeneration(gen int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.generation = gen
}

func (o *Outbox) SetEndpoint(kind string, put EndpointPut) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.endpoints == nil {
		o.endpoints = map[string]EndpointPut{}
	}
	o.endpoints[kind] = put
}

func (o *Outbox) SetConvergence(c Convergence) {
	o.mu.Lock()
	defer o.mu.Unlock()
	cp := c
	o.convergence = &cp
}

func (o *Outbox) SetConductor(c ConductorRecord) {
	o.mu.Lock()
	defer o.mu.Unlock()
	cp := c
	o.conductor = &cp
}

func (o *Outbox) Hold(services ...string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.hold == nil {
		o.hold = map[string]struct{}{}
	}
	for _, s := range services {
		o.hold[s] = struct{}{}
	}
}

// FlushChange sends what is deposited right now, without counting a cycle.
//
// The cycle is what a member says *periodically*; this is for the rare moment
// something material actually happened and waiting up to MemberCycle to say so
// would be visible — a merge landing on main, above all, because the client
// mirrors and the remote conductor all react to that frame. It costs one
// request per real change, never one per tick, which is the distinction the
// whole plan turns on.
func (o *Outbox) FlushChange(ctx context.Context) error {
	_, err := o.flush(ctx, false)
	return err
}

// Flush sends the current deposits as one MemberSync and clears level state
// that should not sticky-repeat (endpoints keep being re-deposited by writers).
func (o *Outbox) Flush(ctx context.Context) (MemberSyncResult, error) {
	return o.flush(ctx, true)
}

func (o *Outbox) flush(ctx context.Context, isCycle bool) (MemberSyncResult, error) {
	o.mu.Lock()
	if isCycle {
		o.cycles++
	}
	// The lost-frame backstop. With no live stream it collapses to every cycle,
	// which is the degradation rule: the snapshot is then the only way this
	// member learns anything, so it asks every time.
	wantSnap := isCycle && (o.cycles%SnapshotEvery == 0 || (o.Client != nil && !o.Client.StreamLive(o.PIN)))
	sync := MemberSync{
		Update:       o.update,
		Generation:   o.generation,
		WantSnapshot: wantSnap,
	}
	for s := range o.hold {
		sync.Hold = append(sync.Hold, s)
	}
	if len(o.endpoints) > 0 {
		sync.Endpoints = make(map[string]EndpointPut, len(o.endpoints))
		for k, v := range o.endpoints {
			sync.Endpoints[k] = v
		}
	}
	if o.convergence != nil {
		cp := *o.convergence
		sync.Convergence = &cp
	}
	if o.conductor != nil {
		cp := *o.conductor
		sync.Conductor = &cp
	}
	// Level-triggered: clear hold/endpoints/convergence/conductor after copy so
	// a writer that stops depositing does not keep re-sending stale values.
	// Member update stays — last_seen must keep moving every cycle.
	o.hold = map[string]struct{}{}
	o.endpoints = map[string]EndpointPut{}
	o.convergence = nil
	o.conductor = nil
	client := o.Client
	pin, id := o.PIN, o.ID
	o.mu.Unlock()

	if client == nil || id == "" {
		return MemberSyncResult{}, nil
	}
	return client.MemberSync(ctx, pin, id, sync)
}
