// Package telemetry is the one piece of code all three emitters — control
// plane, relay and client — use to get an envelope out of a process (plan 46).
//
// The whole design is one sentence: **capture is in the request path, delivery
// is a bounded background queue.** Capture happens where the facts are, so
// nothing is lost between a handler returning and the envelope existing;
// delivery is drop-oldest with one short timeout and no retry beyond the batch
// in hand, so a telemetry service that is down *or hung* costs rows and never
// latency.
//
// That policy is the same in all three processes with **no exception for the
// control plane**. A 2s in-path POST would violate §8.1 on every member cycle
// in every session — and a black-holed ingest, which is the case that actually
// happens, would violate it worse than a dead one.
//
// A gap in the data must be visible AS a gap rather than as silence, so the
// dropped-envelope count goes into the process's own logx output *and* rides
// the next batch that lands.
package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nwylynko/slopball-cli/logx"
)

// MaxBodyBytes is the capture cap. Nothing slopball legitimately handles is
// near it (the session document is single-digit KB), so full fidelity stays
// true in practice — and the cap is what stops a hostile POST being a memory
// attack recorded in your own database.
const MaxBodyBytes = 64 << 10

// Defaults. The queue depth is deep enough that a few seconds of a hung ingest
// costs nothing and shallow enough that a permanently hung one is bounded
// memory.
const (
	DefaultQueueDepth = 1000
	DefaultBatchSize  = 100
	// One short timeout, deliberately: the emitter is giving up, not retrying.
	DefaultTimeout = 2 * time.Second
)

// Event is what a caller hands Emit. It has **no name field** — the name is a
// required argument to Emit, which is what makes anonymous emission impossible
// and stops the taxonomy rotting. Names are per emission site, not per
// content: a new site is a new name, so `SELECT name, count(*)` says what is
// arriving too much and should be switched off in a future binary.
type Event struct {
	// TS overrides the capture time. Zero means now — which is what every
	// in-path caller wants; a captured log line carrying its own timestamp is
	// the exception.
	TS time.Time

	Source     string
	PIN        string
	SessionUID string
	Member     string
	// TraceID joins the two envelopes of one request. Deliberately not
	// "requestId": that name already means the client-minted knock
	// idempotency key, and reusing it would make every future query confuse a
	// knock with an HTTP call.
	TraceID string

	Level     string
	Component string

	Method       string
	PathTemplate string
	Status       int
	DurationMS   int64
	Bytes        int64
	Truncated    bool

	Headers string
	Body    string

	Data map[string]any
}

// Envelope is the wire form: an Event with the emission site's name and the
// emitting service stamped on it.
type Envelope struct {
	TS      time.Time `json:"ts"`
	Name    string    `json:"name"`
	Service string    `json:"service"`

	Source     string `json:"source,omitempty"`
	PIN        string `json:"pin,omitempty"`
	SessionUID string `json:"sessionUid,omitempty"`
	Member     string `json:"member,omitempty"`
	TraceID    string `json:"traceId,omitempty"`

	Level     string `json:"level,omitempty"`
	Component string `json:"component,omitempty"`

	Method       string `json:"method,omitempty"`
	PathTemplate string `json:"pathTemplate,omitempty"`
	Status       int    `json:"status,omitempty"`
	DurationMS   int64  `json:"durationMs,omitempty"`
	Bytes        int64  `json:"bytes,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`

	Headers string `json:"headers,omitempty"`
	Body    string `json:"body,omitempty"`

	Data map[string]any `json:"data,omitempty"`

	// Dropped is how many envelopes were thrown away before this one. It rides
	// the first envelope of the batch that lands after the loss, so the gap is
	// recorded in the same place the data is rather than only in a log nobody
	// kept.
	Dropped int64 `json:"dropped,omitempty"`
}

// Config configures an Emitter. An empty URL is a disabled emitter that
// queues nothing and dials nothing — which is what "a client with no telemetry
// enabled makes zero telemetry requests" rests on.
type Config struct {
	URL string
	// Key is the shared service key a SERVICE presents. Clients send Bearer
	// instead — a signed ticket, verified offline (ticket 10).
	Key string
	// Bearer is the Ed25519 telemetry ticket a CLIENT presents. Set one or the
	// other; a client has no service key and must never be given one.
	Bearer  string
	Service string // control | relay | client

	QueueDepth int
	BatchSize  int
	Timeout    time.Duration

	HTTP *http.Client
	Now  func() time.Time
}

// Emitter is the shared background sender. The zero value is not usable; call
// New.
type Emitter struct {
	cfg  Config
	log  *logx.Logger
	http *http.Client
	now  func() time.Time

	mu     sync.Mutex
	ring   []Envelope // drop-oldest ring, len == cfg.QueueDepth when full
	head   int
	count  int
	closed bool

	dropped atomic.Int64
	wake    chan struct{}
	done    chan struct{}
}

// New starts the delivery goroutine. A Config with no URL returns a disabled
// emitter — Emit is then a no-op and no goroutine runs.
func New(cfg Config) *Emitter {
	if cfg.QueueDepth <= 0 {
		cfg.QueueDepth = DefaultQueueDepth
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Service == "" {
		cfg.Service = "unknown"
	}
	e := &Emitter{
		cfg:  cfg,
		log:  logx.New("telemetry"),
		http: cfg.HTTP,
		now:  cfg.Now,
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.http == nil {
		// No client-level timeout: each POST carries its own context deadline,
		// so a slow batch cannot outlive the give-up point.
		e.http = &http.Client{}
	}
	if !e.Enabled() {
		close(e.done)
		return e
	}
	e.ring = make([]Envelope, cfg.QueueDepth)
	go e.deliver()
	return e
}

// Enabled reports whether anything will ever be sent.
func (e *Emitter) Enabled() bool { return e != nil && e.cfg.URL != "" }

// Dropped is the count waiting to ride the next batch that lands. It is
// cleared by a successful delivery and restored by a failed one, so it always
// describes loss that has not yet been recorded anywhere durable.
func (e *Emitter) Dropped() int64 {
	if e == nil {
		return 0
	}
	return e.dropped.Load()
}

// Emit queues one envelope and returns. It never touches the network, never
// blocks on a lock held across I/O, and never returns an error — a caller that
// could fail because telemetry failed is a caller telemetry is load-bearing
// for.
//
// name is required. An empty name is refused rather than silently recorded:
// an unnamed row is one nothing can ever turn off.
func (e *Emitter) Emit(name string, ev Event) {
	if e == nil || !e.Enabled() {
		return
	}
	if name == "" {
		e.log.Warnf("refusing an emission with no event name (source %q, pin %q)", ev.Source, ev.PIN)
		return
	}
	ts := ev.TS
	if ts.IsZero() {
		ts = e.now().UTC()
	}
	env := Envelope{
		TS: ts, Name: name, Service: e.cfg.Service,
		Source: ev.Source, PIN: ev.PIN, SessionUID: ev.SessionUID, Member: ev.Member,
		TraceID: ev.TraceID, Level: ev.Level, Component: ev.Component,
		Method: ev.Method, PathTemplate: ev.PathTemplate, Status: ev.Status,
		DurationMS: ev.DurationMS, Bytes: ev.Bytes, Truncated: ev.Truncated,
		Headers: ev.Headers, Body: ev.Body, Data: ev.Data,
	}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	if e.count == len(e.ring) {
		// Drop-oldest: the newest facts are the ones worth having when a
		// process is producing more than the ingest can take.
		e.head = (e.head + 1) % len(e.ring)
		e.count--
		e.dropped.Add(1)
	}
	e.ring[(e.head+e.count)%len(e.ring)] = env
	e.count++
	e.mu.Unlock()

	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Close stops the delivery goroutine after flushing what is already queued.
// It is idempotent. A caller stopping on a signal wants Shutdown instead — this
// one waits for the flush however long it takes.
func (e *Emitter) Close() { e.Shutdown(context.Background()) }

// Shutdown stops the emitter, delivering what is already queued, and gives up
// when ctx does.
//
// The bound is the point. A process told to stop is usually being replaced or
// put to sleep, and on a scale-to-zero platform that is the routine path rather
// than an operator's decision — so the tail of the queue is worth one delivery
// attempt. It is worth exactly that: telemetry is never load-bearing, so an
// ingest that accepts and never answers must not be able to hold a shutdown
// open. What the drain abandons is counted by the same accounting an overflow
// uses, because a gap has to be visible as a gap.
//
// Pass context.Background() to wait indefinitely — that is Close.
func (e *Emitter) Shutdown(ctx context.Context) {
	if e == nil {
		return
	}
	e.mu.Lock()
	already := e.closed
	e.closed = true
	pending := e.count
	e.mu.Unlock()
	if !already {
		select {
		case e.wake <- struct{}{}:
		default:
		}
		if pending > 0 {
			e.log.Infof("stopping: draining %d queued telemetry envelope(s)", pending)
		}
	}
	select {
	case <-e.done:
	case <-ctx.Done():
		// Abandon the rest. The delivery goroutine may still be inside one POST
		// with its own deadline; deliberately not waited for, because waiting is
		// what would make the ingest — not the caller — own the bound.
		e.mu.Lock()
		left := e.count
		e.count = 0
		e.mu.Unlock()
		if left > 0 {
			e.dropped.Add(int64(left))
		}
		e.log.Warnf("gave up draining telemetry after %v — %d envelope(s) undelivered; a shutdown never waits on the ingest", ctx.Err(), left)
	}
}

func (e *Emitter) deliver() {
	defer close(e.done)
	for {
		batch := e.take()
		if len(batch) > 0 {
			e.send(batch)
			continue
		}
		e.mu.Lock()
		closed := e.closed
		e.mu.Unlock()
		if closed {
			return
		}
		<-e.wake
	}
}

// take drains up to BatchSize envelopes and stamps the pending drop count onto
// the first of them. The count is taken (not read) here and put back by a
// failed send, so it is recorded exactly once.
func (e *Emitter) take() []Envelope {
	e.mu.Lock()
	n := e.count
	if n > e.cfg.BatchSize {
		n = e.cfg.BatchSize
	}
	if n == 0 {
		e.mu.Unlock()
		return nil
	}
	batch := make([]Envelope, n)
	for i := 0; i < n; i++ {
		batch[i] = e.ring[(e.head+i)%len(e.ring)]
	}
	e.head = (e.head + n) % len(e.ring)
	e.count -= n
	e.mu.Unlock()

	if d := e.dropped.Swap(0); d > 0 {
		batch[0].Dropped = d
		// The one thing that must be visible when the telemetry path is itself
		// the broken thing.
		e.log.Warnf("dropped %d envelope(s) — the ingest is not keeping up (queue depth %d)", d, e.cfg.QueueDepth)
	}
	return batch
}

func (e *Emitter) send(batch []Envelope) {
	body, err := json.Marshal(batch)
	if err != nil {
		e.log.Warnf("encoding a batch of %d failed, dropping it: %v", len(batch), err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.URL, bytes.NewReader(body))
	if err != nil {
		e.log.Warnf("building the ingest request failed, dropping %d: %v", len(batch), err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.Key != "" {
		req.Header.Set(KeyHeader, e.cfg.Key)
	}
	if e.cfg.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+e.cfg.Bearer)
	}
	resp, err := e.http.Do(req)
	if err != nil {
		e.fail(batch, err.Error())
		return
	}
	defer resp.Body.Close()
	// Drain so the connection is reusable — the emitter POSTs to the same
	// endpoint for the life of the process.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 300 {
		e.fail(batch, resp.Status)
		return
	}
}

// fail gives up on the batch in hand. There is no retry: a retry queue is a
// second unbounded buffer, and the whole policy is that loss is cheaper than
// latency. The count goes back so it rides whichever batch lands next.
func (e *Emitter) fail(batch []Envelope, why string) {
	e.dropped.Add(int64(len(batch)) + batch[0].Dropped)
	e.log.Warnf("ingest refused a batch of %d, dropping it: %s", len(batch), why)
}

// KeyHeader carries the shared service key. Clients authenticate with an
// Ed25519 ticket instead; that lands with the ingest itself.
const KeyHeader = "X-Slopball-Telemetry-Key"

// KeyEnv names the shared service key. It lives HERE, in the emitter package,
// rather than in the telemetry service: the relay reads it too, and the relay
// image deliberately links neither pgx nor a store — importing the service
// package for one constant would put a postgres driver in it.
const KeyEnv = "SLOPBALL_TELEMETRY_KEY"

// URLEnv names where a service posts its own envelopes. Deliberately distinct
// from what CLIENTS are told: on the compose bridge a service posts to
// `http://slopball-telemetry:7779/v1/telemetry` while a laptop needs the public
// hostname, and one name serving both would be wrong for one of them.
const URLEnv = "SLOPBALL_TELEMETRY_URL"

// RequireURL is the boot refusal for a service with nowhere to post.
// Configurable implies required: "unset means do not record" is the soft
// default that leaves a deployment silently blind, which is the one failure
// this whole plan exists to remove.
func RequireURL(url string) error {
	if strings.TrimSpace(url) == "" {
		return errors.New(URLEnv + " is unset — set it to this deployment's telemetry ingest (compose uses http://slopball-telemetry:7779" + IngestPath + ")")
	}
	return nil
}

// IngestPath is where every emitter POSTs. A constant, not config: it is the
// same in every deployment.
const IngestPath = "/v1/telemetry"

// TicketService is the session-network service name a client's telemetry
// ticket is minted for. It lives here because both ends read it — the client
// looks the ticket up by this name, the ingest refuses any other — and neither
// should own the other's constant.
const TicketService = "telemetry"

// RequireKey is the refusal every service that emits telemetry shares, so the
// sentence an operator reads is identical on all three. Configurable implies
// required (ADR 0006): no soft default, no "unset means off". The threat is not
// reading, it is writing — an open ingest means anyone can put rows in the one
// place you dig, and poisoned data is worse than no data because you will
// trust it.
func RequireKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New(KeyEnv + " is unset — set it in .env to the shared telemetry ingest key (any long random string; the same value on the control plane, the relay and slopball-telemetry)")
	}
	return nil
}

// GiveUpAfter is the per-batch deadline — how long one delivery attempt may
// take before the emitter abandons it. Exposed so a test can state its latency
// budget in terms of the cost that would ACTUALLY be paid if delivery were ever
// moved onto the caller's path, rather than in terms of a number somebody
// guessed (plan 46 ticket 14).
func (e *Emitter) GiveUpAfter() time.Duration {
	if e == nil {
		return 0
	}
	return e.cfg.Timeout
}
