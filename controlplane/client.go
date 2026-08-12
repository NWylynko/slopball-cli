package controlplane

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/slopball-cli/logx"
	"github.com/nwylynko/slopball-cli/session"
	"github.com/nwylynko/slopball-cli/sessionnet"
	"github.com/nwylynko/slopball-cli/telemetry"
)

// controlClientLog is the client half's logger. It exists for exactly one line
// today — the full text of a version refusal, which the caller deliberately
// never prints — so `SLOPBALL_LOG=debug` can still answer "refused for being
// how old?" without the CLI turning one sentence into a paragraph.
var controlClientLog = logx.New("control-client")

// ErrDemoted is returned when a write is rejected with 409 — caller must become a client.
var ErrDemoted = errors.New("demoted to client: stale generation")

// ErrUnavailable is returned when the control plane cannot be reached.
var ErrUnavailable = errors.New("control plane unreachable")

// ErrNoSession is returned when the control plane answers 404 for a PIN: the
// session was never created, or it expired and the sweeper deleted it (sessions
// live 3h after the last real work). Distinct from ErrUnavailable, and the distinction is the whole
// point — unreachable means "ask again later, change nothing", while this is the
// arbiter authoritatively saying the session is gone.
var ErrNoSession = errors.New("session does not exist (unknown or expired pin)")

// ErrUnauthorized is a 401 — missing or unrecognised member secret, or a
// session that is gone presented the same way (plan 44 ticket 09). Terminal for
// daemons: never reconnect-loop on it.
var ErrUnauthorized = errors.New("unauthorized")

// ErrBadRequest is a 400 — a size or shape bound refused the body.
var ErrBadRequest = errors.New("bad request")

// ErrUpgradeRequired is a 426 — this binary is below the control plane's client
// version floor (plan 48).
//
// Its message IS the sentence, and it is deliberately the whole error: every
// surface that prints this — `slopball join`, the daemon's log, any verb that
// touches the control plane — prints exactly one plain line a person can act on.
// The server's 426 body says more (their version, the floor, the fix) and that
// is for a log or a support paste; wrapping it in here would turn one sentence
// into a paragraph on every failed call.
//
// Terminal only in the sense that retrying cannot help while the binary is what
// it is — a refused JOIN DAEMON keeps running best-effort: its git sync rides
// the relay on cached endpoints until its last ticket expires (an hour), and its
// member cycle keeps its normal cadence so a rollback that lowers the floor
// self-heals without a restart. Never turn this into an exit path.
var ErrUpgradeRequired = errors.New("slopball is too old for this control plane — re-run install.sh, then rejoin")

// ErrRateLimited is a 429 — a shared-limiter subject refused the call.
var ErrRateLimited = errors.New("rate limited")

// Door refusals from redeem (plan 44 ticket 07). Same HTTP shape as ErrNoSession;
// the body names which, and these sentinels let join say so.
var (
	ErrDoorClosed = errors.New("session is closed")
	ErrDoorFull   = errors.New("session is full")
)

// Client talks to the control-plane HTTP API with backoff and a last-known cache.
type Client struct {
	Base   string
	HTTP   *http.Client
	mu     sync.Mutex
	cache  map[string]Session // pin → last known
	buf    []bufferedWrite    // fire-and-forget buffer when unreachable
	maxBuf int
	// watching is pin → live SSE stream state (plan 43). Set only by Watch.
	watching map[string]*streamState
	// outboxes is pin → this process's member cycle, when it holds membership.
	outboxes map[string]*Outbox
	// secrets is pin → membership held in memory (overrides disk). Used by the
	// control-plane provisioner: it mints the box member and must present it
	// while waiting for the box to claim, with no SLOPBALL_HOME.
	secrets map[string]membership
	// tickets is pin → service → Ed25519 relay ticket (ticket 17). Filled from
	// Claim / redeem / invite / MemberSync responses; Dialable and holders read
	// it without another round trip.
	tickets map[string]map[string]string
	// StreamSilentAfter overrides the global StreamSilentAfter for this client.
	// Tests shrink it; nothing in production sets it.
	StreamSilentAfter time.Duration
}

type bufferedWrite struct {
	method, path string
	body         []byte
}

// NewClient builds a client against base (trailing slash stripped).
func NewClient(base string) *Client {
	return &Client{
		Base:    trimSlash(base),
		HTTP:    &http.Client{Timeout: 10 * time.Second},
		cache:   map[string]Session{},
		maxBuf:  64,
		secrets: map[string]membership{},
	}
}

// RememberMembership pins the credential this client presents for pin.
//
// It is set at every mint (claim, redeem, invite) as well as by the managed-box
// provisioner, which has no session home at all and would otherwise 401 on
// every poll. Pinning matters beyond that: the disk copy is read per request,
// so a second process sharing a SLOPBALL_HOME could otherwise swap this
// client's identity mid-flight and every call would start answering 403.
func (c *Client) RememberMembership(pin, memberID, secret string) {
	pin = strings.TrimSpace(pin)
	memberID, secret = strings.TrimSpace(memberID), strings.TrimSpace(secret)
	if pin == "" || secret == "" || memberID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.secrets == nil {
		c.secrets = map[string]membership{}
	}
	c.secrets[pin] = membership{id: memberID, secret: secret}
}

// RememberRelayTickets stores session-network tickets minted for this member.
// They are also written under the session home so a cold CLI process can dial
// the relay before its first member cycle.
func (c *Client) RememberRelayTickets(pin string, tickets map[string]string) {
	pin = strings.TrimSpace(pin)
	if pin == "" || len(tickets) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tickets == nil {
		c.tickets = map[string]map[string]string{}
	}
	cp := make(map[string]string, len(tickets))
	for k, v := range tickets {
		if k != "" && v != "" {
			cp[k] = v
		}
	}
	if len(cp) == 0 {
		return
	}
	c.tickets[pin] = cp
	_ = session.WriteRelayTickets(pin, cp)
}

// RelayTicket returns a cached ticket for pin/service, or "" if none.
// Memory first, then the on-disk cache written at mint time.
func (c *Client) RelayTicket(pin, service string) string {
	c.mu.Lock()
	if c.tickets != nil {
		if tok := c.tickets[pin][service]; tok != "" {
			c.mu.Unlock()
			return tok
		}
	}
	c.mu.Unlock()
	disk := session.ReadRelayTickets(pin)
	if len(disk) == 0 {
		return ""
	}
	c.mu.Lock()
	if c.tickets == nil {
		c.tickets = map[string]map[string]string{}
	}
	c.tickets[pin] = disk
	tok := disk[service]
	c.mu.Unlock()
	return tok
}

// DialTicket is the sessionnet.Dialer.Ticket callback for this pin. An empty
// ticket means the dialer omits the field — correct when the relay has no
// TicketPublic and this control plane never minted.
func (c *Client) DialTicket(pin string) func(service string) (string, error) {
	return func(service string) (string, error) {
		return c.RelayTicket(pin, service), nil
	}
}

// HolderTicket is the sessionnet.HolderConfig.Ticket callback for one service.
func (c *Client) HolderTicket(pin, service string) func() (string, error) {
	return func() (string, error) {
		return c.RelayTicket(pin, service), nil
	}
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// Session returns the full snapshot. While Watch is active for pin, this is a
// cache read with no HTTP — including brief reconnect gaps (a stream that ends
// is a reconnect, never a verdict; plan 43). One-shot verbs that never Watch
// keep doing exactly one GET.
func (c *Client) Session(ctx context.Context, pin string) (Session, error) {
	if c.watchingPin(pin) {
		c.mu.Lock()
		cached, ok := c.cache[pin]
		c.mu.Unlock()
		if ok {
			return cached, nil
		}
		// Watch started but no frame yet — fall through to one GET.
	}
	var s Session
	err := c.do(ctx, http.MethodGet, "/v1/sessions/"+pin, nil, &s)
	if err != nil {
		c.mu.Lock()
		cached, ok := c.cache[pin]
		c.mu.Unlock()
		if ok {
			return cached, fmt.Errorf("%w (using cache): %v", ErrUnavailable, err)
		}
		return s, err
	}
	c.mu.Lock()
	c.cache[pin] = s
	c.mu.Unlock()
	return s, nil
}

// Cached returns the last-known session without hitting the network.
func (c *Client) Cached(pin string) (Session, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.cache[pin]
	return s, ok
}

// Claim creates or resumes a session (or signals join-only).
// For an existing PIN the Bearer is required (plan 44 ticket 09) — attach it
// from disk even though the path is /v1/sessions with no pin segment.
func (c *Client) Claim(ctx context.Context, req ClaimRequest) (ClaimResponse, error) {
	var res ClaimResponse
	err := c.doWithPin(ctx, http.MethodPost, "/v1/sessions", req.PIN, req, &res)
	if err == nil {
		c.mu.Lock()
		c.cache[res.Session.PIN] = res.Session
		c.mu.Unlock()
		if res.Secret != "" {
			if err := session.WriteMembership(res.Session.PIN, res.MemberID, res.Secret); err != nil {
				return res, fmt.Errorf("persist membership: %w", err)
			}
			c.RememberMembership(res.Session.PIN, res.MemberID, res.Secret)
		}
		c.RememberRelayTickets(res.Session.PIN, res.RelayTickets)
		// A member comes into existence HERE, and it already has everything the
		// client emitter needs — so telemetry is live from the session's first
		// request rather than from its first cycle five seconds later. That gap
		// is the whole standup on a short run, which is where the interesting
		// lines are.
		mid := res.MemberID
		if mid == "" {
			// A resume or takeover mints nothing: this machine's id is the one
			// it already holds for this pin.
			mid = c.memberIDFor(res.Session.PIN)
		}
		c.useTelemetry(res.Session.PIN, mid, &res.Session)
	}
	return res, err
}

// PutEndpoint announces an endpoint. Returns ErrDemoted on 409.
func (c *Client) PutEndpoint(ctx context.Context, pin, kind string, put EndpointPut) error {
	err := c.do(ctx, http.MethodPut, "/v1/sessions/"+pin+"/endpoints/"+kind, put, nil)
	if err == nil {
		c.cacheEndpoint(pin, kind, put)
	}
	return err
}

// invalidate drops the cached document for pin so the next Session() re-reads
// it. Read-your-writes for the writes that are rare enough not to be worth
// projecting by hand — a join, an election, a cutover. The frequent ones (the
// member cycle's endpoint and lease writes) fold instead, because invalidating
// on those would put a GET back into the steady state, which is the request
// this whole plan exists to remove.
func (c *Client) invalidate(pin string) {
	c.mu.Lock()
	delete(c.cache, pin)
	c.mu.Unlock()
}

// cacheEndpoint folds this process's own endpoint write into the cached
// document. Read-your-writes: while a Watch is live Session() answers from the
// cache, and the pushed frame confirming a write is up to one coalescing window
// behind it. Without this a host that announces its git endpoint and reads the
// session back — which is what a cutover check and every "did that land?" does
// — sees the document from before its own write.
func (c *Client) cacheEndpoint(pin, kind string, put EndpointPut) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sess, ok := c.cache[pin]
	if !ok {
		return // nothing cached yet: the first frame or snapshot carries it
	}
	if put.URL == "" {
		if sess.Endpoints != nil {
			delete(sess.Endpoints, kind)
			c.cache[pin] = sess
		}
		return
	}
	eps := make(map[string]Endpoint, len(sess.Endpoints)+1)
	for k, v := range sess.Endpoints {
		eps[k] = v
	}
	now := time.Now().UTC()
	eps[kind] = Endpoint{
		URL: put.URL, Host: put.Host, Port: put.Port,
		Source: put.Source, Direct: put.Direct, UpdatedAt: &now,
	}
	sess.Endpoints = eps
	c.cache[pin] = sess
}

// ClearEndpoint removes a published endpoint (empty Put). Used to stop
// advertising a demo tunnel URL after the tunnel closes.
func (c *Client) ClearEndpoint(ctx context.Context, pin, kind string, generation int) error {
	return c.PutEndpoint(ctx, pin, kind, EndpointPut{URL: "", Generation: generation, Source: "clear"})
}

// PutEndpointBestEffort buffers on unavailability (never blocks the merge path).
func (c *Client) PutEndpointBestEffort(ctx context.Context, pin, kind string, put EndpointPut) {
	if err := c.PutEndpoint(ctx, pin, kind, put); err != nil && !errors.Is(err, ErrDemoted) {
		c.buffer(http.MethodPut, "/v1/sessions/"+pin+"/endpoints/"+kind, put)
	}
}

// Cutover posts a host handoff.
func (c *Client) Cutover(ctx context.Context, pin string, req CutoverRequest) (Session, error) {
	var s Session
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+pin+"/cutover", req, &s)
	if err == nil {
		c.mu.Lock()
		c.cache[pin] = s
		c.mu.Unlock()
	}
	return s, err
}

// InviteResult is what an invite hands the caller once — never recoverable later.
type InviteResult struct {
	MemberID     string
	Secret       string
	RelayTickets map[string]string
}

// Invite mints an admitted member under the caller's Bearer. The invitee secret
// is returned to the caller to hand over; it is not written over the inviter's
// on-disk secret (plan 44 ticket 04).
func (c *Client) Invite(ctx context.Context, pin string, req MemberJoinRequest) (InviteResult, error) {
	var out struct {
		MemberID     string            `json:"memberId"`
		Secret       string            `json:"secret"`
		RelayTickets map[string]string `json:"relayTickets"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+pin+"/members", req, &out)
	if err == nil {
		c.invalidate(pin)
	}
	return InviteResult{MemberID: out.MemberID, Secret: out.Secret, RelayTickets: out.RelayTickets}, err
}

// SetAccess flips the session door open or closed (plan 44 ticket 07).
// Closing declines every pending knock in the same transaction.
func (c *Client) SetAccess(ctx context.Context, pin, access string) error {
	err := c.do(ctx, http.MethodPut, "/v1/sessions/"+pin+"/access", AccessPut{Access: access}, nil)
	if err == nil {
		c.invalidate(pin)
	}
	return err
}

// PendingMembers lists join requests waiting on a human (plan 44 ticket 06).
func (c *Client) PendingMembers(ctx context.Context, pin string) ([]Member, error) {
	var out []Member
	err := c.do(ctx, http.MethodGet, "/v1/sessions/"+pin+"/pending", nil, &out)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []Member{}
	}
	return out, nil
}

// DecideMember accepts or declines a pending join request. First decision wins.
func (c *Client) DecideMember(ctx context.Context, pin, memberID, decision string) (Member, error) {
	var m Member
	err := c.do(ctx, http.MethodPut, "/v1/sessions/"+pin+"/members/"+memberID+"/decision",
		MemberDecision{Decision: decision}, &m)
	if err == nil {
		c.invalidate(pin)
	}
	return m, err
}

// ErrDeclined is a knock the session refused. Distinct so join can say so.
var ErrDeclined = errors.New("join request declined")

// ErrKnockSpent is a request id whose knock was already decided and whose
// secret cannot be handed over again — the accept landed while this joiner was
// disconnected and the control plane restarted before it came back. The
// membership exists and its secret is unrecoverable (only a hash is stored), so
// the only way forward is a new knock, which costs one more accept.
var ErrKnockSpent = errors.New("this join request was already decided and its secret is gone")

// Redeem holds until a member accepts or declines the knock (plan 44 ticket 06).
// onPending fires once when the queue entry is held — the joiner's wait line
// names who can accept (plan 44 ticket 10). On accept it persists the secret
// and returns the session document. On decline it returns ErrDeclined.
func (c *Client) Redeem(ctx context.Context, pin string, req RedeemRequest, onPending func(RedeemPending)) (RedeemResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return RedeemResult{}, err
	}
	httpReq, err := newVersionedRequest(ctx, http.MethodPost, c.Base+"/v1/sessions/"+pin+"/redeem", bytes.NewReader(body))
	if err != nil {
		return RedeemResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	// Deliberately no Bearer — redeem is the unauthenticated door.
	// And deliberately no Client.Timeout: a held knock waits on a human, which
	// is minutes, not the 10s budget ordinary calls use.
	hc := c.http()
	stream := *hc
	stream.Timeout = 0
	res, err := stream.Do(httpReq)
	if err != nil {
		return RedeemResult{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUpgradeRequired {
		b, _ := io.ReadAll(res.Body)
		return RedeemResult{}, upgradeRequired("redeem", b)
	}
	if res.StatusCode == 404 {
		b, _ := io.ReadAll(res.Body)
		msg := stringsTrim(string(b))
		low := strings.ToLower(msg)
		switch {
		case strings.Contains(low, "closed"):
			return RedeemResult{}, fmt.Errorf("%w: %s", ErrDoorClosed, msg)
		case strings.Contains(low, "full"):
			return RedeemResult{}, fmt.Errorf("%w: %s", ErrDoorFull, msg)
		default:
			return RedeemResult{}, fmt.Errorf("%w: %s", ErrNoSession, msg)
		}
	}
	if res.StatusCode == http.StatusConflict {
		b, _ := io.ReadAll(res.Body)
		return RedeemResult{}, fmt.Errorf("%w: %s", ErrKnockSpent, stringsTrim(string(b)))
	}
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(res.Body)
		msg := stringsTrim(string(b))
		if msg == "" {
			msg = res.Status
		}
		return RedeemResult{}, fmt.Errorf("redeem: %s", msg)
	}
	result, err := readRedeemSSE(res.Body, onPending)
	if err != nil {
		return RedeemResult{}, err
	}
	switch result.Decision {
	case "accepted":
		if result.Secret != "" {
			if err := session.WriteMembership(pin, result.MemberID, result.Secret); err != nil {
				return result, fmt.Errorf("persist membership: %w", err)
			}
			c.RememberMembership(pin, result.MemberID, result.Secret)
		}
		c.RememberRelayTickets(pin, result.RelayTickets)
		c.mu.Lock()
		c.cache[pin] = result.Session
		c.mu.Unlock()
		return result, nil
	case "declined":
		msg := result.Message
		if msg == "" {
			msg = "join request declined"
		}
		return result, fmt.Errorf("%w: %s", ErrDeclined, msg)
	default:
		return result, fmt.Errorf("redeem: unexpected decision %q", result.Decision)
	}
}

func readRedeemSSE(r io.Reader, onPending func(RedeemPending)) (RedeemResult, error) {
	sc := bufio.NewScanner(r)
	// Decision payloads carry a session document — allow a larger line.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var event string
	var data strings.Builder
	flush := func() (RedeemResult, bool, error) {
		payload := data.String()
		ev := event
		event, data = "", strings.Builder{}
		switch ev {
		case "pending":
			if onPending != nil {
				var p RedeemPending
				if err := json.Unmarshal([]byte(payload), &p); err == nil {
					onPending(p)
				}
			}
			return RedeemResult{}, false, nil
		case "accepted", "declined":
			var out RedeemResult
			if err := json.Unmarshal([]byte(payload), &out); err != nil {
				return RedeemResult{}, false, err
			}
			if out.Decision == "" {
				out.Decision = ev
			}
			return out, true, nil
		default:
			return RedeemResult{}, false, nil
		}
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "data: "):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(line[6:])
		case line == "":
			if out, ok, err := flush(); ok || err != nil {
				return out, err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return RedeemResult{}, err
	}
	if out, ok, err := flush(); ok || err != nil {
		return out, err
	}
	return RedeemResult{}, fmt.Errorf("redeem: stream ended without a decision")
}

// JoinMember registers presence. The raw secret rides the response once and is
// persisted on disk; later calls load it for the Bearer header.
// Prefer Invite when minting an identity for another machine.
func (c *Client) JoinMember(ctx context.Context, pin string, req MemberJoinRequest) (string, error) {
	var out struct {
		MemberID     string            `json:"memberId"`
		Secret       string            `json:"secret"`
		RelayTickets map[string]string `json:"relayTickets"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+pin+"/members", req, &out)
	if err == nil {
		c.invalidate(pin)
		if out.Secret != "" {
			if err := session.WriteMembership(pin, out.MemberID, out.Secret); err != nil {
				return out.MemberID, fmt.Errorf("persist membership: %w", err)
			}
			c.RememberMembership(pin, out.MemberID, out.Secret)
		}
		c.RememberRelayTickets(pin, out.RelayTickets)
	}
	return out.MemberID, err
}

// Heartbeat updates a member.
func (c *Client) Heartbeat(ctx context.Context, pin, id string, upd MemberUpdate) error {
	return c.do(ctx, http.MethodPut, "/v1/sessions/"+pin+"/members/"+id, upd, nil)
}

// MemberSync is the one uplink per member cycle (plan 43): alive + renews +
// published state. Conflict maps to ErrDemoted at the call site.
func (c *Client) MemberSync(ctx context.Context, pin, id string, sync MemberSync) (MemberSyncResult, error) {
	var res MemberSyncResult
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+pin+"/members/"+id+"/sync", sync, &res)
	if err != nil {
		return res, err
	}
	c.RememberRelayTickets(pin, res.RelayTickets)
	// The member cycle is the ONE place that already holds all four facts a
	// client's telemetry needs — the pin, the session uid, this member's id,
	// and a fresh ticket — so it is where the client emitter is pointed (plan
	// 46 ticket 13). Idempotent and cheap; it rebuilds only on a real change.
	c.useTelemetry(pin, id, res.Session)
	if res.Session != nil {
		c.mu.Lock()
		c.cache[pin] = *res.Session
		c.mu.Unlock()
		return res, nil
	}
	// Same read-your-writes as PutEndpoint: the endpoints this cycle just
	// published are true unless the generation conflicted, in which case the
	// server skipped them and the cache must not claim otherwise.
	if !res.Conflict {
		for kind, put := range sync.Endpoints {
			c.cacheEndpoint(pin, kind, put)
		}
	}
	return res, nil
}

// LeaveMember marks the row left (membership kept; plan 44 ticket 03). The
// HTTP verb stays DELETE — the body is gone from the present list either way.
func (c *Client) LeaveMember(ctx context.Context, pin, id string) error {
	err := c.do(ctx, http.MethodDelete, "/v1/sessions/"+pin+"/members/"+id, nil, nil)
	if err == nil {
		c.invalidate(pin)
	}
	return err
}

// EndSession marks the session ended. Used when a Claim-first box provision
// fails so the PIN does not leak as a live empty session (plan 44 ticket 05).
func (c *Client) EndSession(ctx context.Context, pin string) error {
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+pin+"/end", nil, nil)
	if err == nil {
		c.invalidate(pin)
	}
	return err
}

// PutConvergence publishes convergence and reports the failure. The merge path
// uses PutConvergenceBestEffort; this is for callers that want to know.
func (c *Client) PutConvergence(ctx context.Context, pin string, conv Convergence) error {
	err := c.do(ctx, http.MethodPut, "/v1/sessions/"+pin+"/convergence", conv, nil)
	if err == nil {
		c.invalidate(pin)
	}
	return err
}

// PutConvergenceBestEffort publishes convergence without blocking.
func (c *Client) PutConvergenceBestEffort(ctx context.Context, pin string, conv Convergence) {
	if err := c.PutConvergence(ctx, pin, conv); err != nil {
		c.buffer(http.MethodPut, "/v1/sessions/"+pin+"/convergence", conv)
	}
}

// PutConductor publishes the election discovery record.
func (c *Client) PutConductor(ctx context.Context, pin string, rec ConductorRecord) error {
	err := c.do(ctx, http.MethodPut, "/v1/sessions/"+pin+"/conductor", rec, nil)
	if err == nil {
		c.invalidate(pin)
	}
	return err
}

// PutBox publishes box facts.
func (c *Client) PutBox(ctx context.Context, pin string, b BoxFacts) error {
	err := c.do(ctx, http.MethodPut, "/v1/sessions/"+pin+"/box", b, nil)
	if err == nil {
		c.invalidate(pin)
	}
	return err
}

// GitURL resolves the session git dial address.
func (c *Client) GitURL(ctx context.Context, pin string) (string, int, error) {
	s, err := c.Session(ctx, pin)
	if err != nil && !errors.Is(err, ErrUnavailable) {
		return "", 0, err
	}
	if errors.Is(err, ErrUnavailable) {
		// Session still returned from cache inside Session().
	}
	ep, ok := s.Endpoints[EndpointGit]
	if !ok || ep.URL == "" {
		return "", s.Generation, fmt.Errorf("no git endpoint for %s", pin)
	}
	url, err := c.Dialable(ctx, s, ep.URL)
	if err != nil {
		return "", s.Generation, err
	}
	return url, s.Generation, nil
}

// Dialable turns whatever a session published into a URL local tools can use.
// A `slop://` address (plan 09) is carried on the session network via a
// loopback forwarder; anything else is already a machine address and is
// returned unchanged.
//
// This is deliberately the ONE place that knows the difference. Every caller —
// repoint, the join daemon, box add, the console — asks the control plane for a
// git URL and gets something git can clone, which is why the session network
// could be introduced without every one of them learning a new concept.
func (c *Client) Dialable(ctx context.Context, s Session, raw string) (string, error) {
	if !sessionnet.IsSessionURL(raw) {
		return raw, nil
	}
	key, err := sessionnet.ParseKey(s.SessionKey)
	if err != nil {
		return "", err
	}
	if key.Zero() {
		return "", fmt.Errorf("session %s published %s but handed out no session key — nothing can authenticate to it", s.PIN, raw)
	}
	if s.RelayAddr == "" {
		return "", fmt.Errorf("session %s is on the session network but its control plane names no relay (start one with `slopball relay`)", s.PIN)
	}
	// Direct dial: a holder that published a machine address AND is actually
	// reachable from here is dialed straight, and the relay becomes the
	// fallback rather than the toll everybody pays. It is resolved from the
	// session record per service, so it stays correct when the lease migrates —
	// the new holder publishes its own address, or none.
	return sessionnet.LocalURL(ctx, raw, s.RelayAddr, key, directFor(s), c.DialTicket(s.PIN))
}

// NoEndpointError says the session has not published this kind yet, which is a
// normal early-session state rather than a failure to report at people.
type NoEndpointError struct{ PIN, Kind string }

func (e *NoEndpointError) Error() string {
	return "publishes no " + e.Kind + " endpoint"
}

// EndpointURL is Dialable for callers that only know a kind: read the session's
// published endpoint and hand back something local tools can actually dial.
//
// It exists because the raw-URL mistake has now shipped three times in four
// callers (joindaemon.Join, standUpOnBox, and both log readers — plan 40).
// Reading `Endpoints[kind].URL` and using it is the bug; there is no correct
// version of it, so this is the shape every caller gets instead.
func (c *Client) EndpointURL(ctx context.Context, pin, kind string) (string, error) {
	sess, err := c.Session(ctx, pin)
	if err != nil {
		return "", err
	}
	ep, ok := sess.Endpoints[kind]
	if !ok || ep.URL == "" {
		return "", fmt.Errorf("session %s: %w", pin, &NoEndpointError{PIN: pin, Kind: kind})
	}
	return c.Dialable(ctx, sess, ep.URL)
}

// SessionDialer builds a session-network dialer for any service on this session.
// Exec and other sessionnet clients should use this rather than re-reading the
// endpoint map in the CLI.
func (c *Client) SessionDialer(ctx context.Context, pin string) (*sessionnet.Dialer, error) {
	sess, err := c.Session(ctx, pin)
	if err != nil {
		return nil, err
	}
	key, err := sessionnet.ParseKey(sess.SessionKey)
	if err != nil {
		return nil, err
	}
	if key.Zero() {
		return nil, fmt.Errorf("session %s has no session key — cannot reach services over the session network", pin)
	}
	if sess.RelayAddr == "" {
		return nil, fmt.Errorf("session %s has no relay — cannot reach services over the session network", pin)
	}
	return &sessionnet.Dialer{
		Relay:  sess.RelayAddr,
		PIN:    pin,
		Key:    key,
		Direct: directFor(sess),
		Ticket: c.DialTicket(pin),
	}, nil
}

// directFor resolves a service's directly-published machine address, or "" when
// there is none. On separate mobile hotspots that is everyone, which is exactly
// why direct is an optimisation and the relay is the mechanism.
func directFor(s Session) func(service string) string {
	return func(service string) string {
		ep, ok := s.Endpoints[service]
		if !ok {
			return ""
		}
		return ep.Direct
	}
}

// RequestBox asks the control plane to provision this session's box. It returns
// as soon as the request is recorded — provisioning runs server-side, and
// WaitForBox is how you find out how it went.
func (c *Client) RequestBox(ctx context.Context, pin string, req BoxRequest) (BoxFacts, error) {
	var b BoxFacts
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+pin+"/box", req, &b)
	return b, err
}

// GetBox reads the box record.
func (c *Client) GetBox(ctx context.Context, pin string) (BoxFacts, error) {
	var b BoxFacts
	err := c.do(ctx, http.MethodGet, "/v1/sessions/"+pin+"/box", nil, &b)
	return b, err
}

// DestroyBox tears the session's box down.
func (c *Client) DestroyBox(ctx context.Context, pin string) error {
	return c.do(ctx, http.MethodDelete, "/v1/sessions/"+pin+"/box", nil, nil)
}

// WaitForBox polls until the box is ready or the provision fails. One
// implementation, here, like WaitHealthy — a failed provision comes back as an
// error carrying the provisioner's own words, including the container log tail,
// because that is the only diagnostic the asking human will ever see.
func (c *Client) WaitForBox(ctx context.Context, pin string, timeout time.Duration) (BoxFacts, error) {
	deadline := time.Now().Add(timeout)
	var last BoxFacts
	for {
		b, err := c.GetBox(ctx, pin)
		if err == nil {
			last = b
			switch b.State {
			case BoxReady:
				return b, nil
			case BoxFailed:
				return b, fmt.Errorf("provisioning the box for %s failed: %s", pin, b.Error)
			}
		}
		if ctx.Err() != nil {
			return last, ctx.Err()
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("the box for %s was still %q after %s", pin, orState(last.State), timeout)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func orState(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// PublishEvent appends one work-feed event and returns the refusal (ticket 20).
func (c *Client) PublishEvent(ctx context.Context, pin, kind string, payload map[string]any) error {
	return c.do(ctx, http.MethodPost, "/v1/sessions/"+pin+"/events",
		EventPost{Kind: kind, Payload: payload}, nil)
}

// PublishEventBestEffort appends one work-feed event (plan 36 §2). Best-effort
// and buffered like convergence: the feed is what a member *watches*, never
// what the merge path waits on, so a control plane that blinks must not slow a
// sync or a merge down. A rate-limit or bad-request refusal is dropped, not
// buffered — retrying a flood would make the limiter permanent.
func (c *Client) PublishEventBestEffort(ctx context.Context, pin, kind string, payload map[string]any) {
	path := "/v1/sessions/" + pin + "/events"
	e := EventPost{Kind: kind, Payload: payload}
	if err := c.do(ctx, http.MethodPost, path, e, nil); err != nil {
		if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrBadRequest) {
			return
		}
		c.buffer(http.MethodPost, path, e)
	}
}

// EventsSince polls events (JSON, not SSE).
func (c *Client) EventsSince(ctx context.Context, pin string, since int64) ([]Event, error) {
	var events []Event
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/v1/sessions/%s/events?since=%d", pin, since), nil, &events)
	return events, err
}

// Health reads /healthz.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	err := c.do(ctx, http.MethodGet, "/healthz", nil, &h)
	return h, err
}

// WaitHealthy polls /healthz until the server reports OK or timeout elapses.
// Every harness that boots a control plane needs this, so it lives here rather
// than being re-implemented per caller.
func (c *Client) WaitHealthy(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		reqCtx, cancel := context.WithTimeout(ctx, time.Second)
		h, err := c.Health(reqCtx)
		cancel()
		if err == nil && h.OK {
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = fmt.Errorf("server reported not-ok")
			}
			return fmt.Errorf("control plane not healthy after %s: %w", timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// FlushBuffer retries buffered writes (best-effort).
func (c *Client) FlushBuffer(ctx context.Context) {
	c.mu.Lock()
	buf := c.buf
	c.buf = nil
	c.mu.Unlock()
	for _, w := range buf {
		req, err := newVersionedRequest(ctx, w.method, c.Base+w.path, bytes.NewReader(w.body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		c.attachBearer(req, w.path)
		res, err := c.http().Do(req)
		if err != nil {
			c.bufferRaw(w.method, w.path, w.body)
			continue
		}
		res.Body.Close()
	}
}

func (c *Client) buffer(method, path string, body any) {
	b, _ := json.Marshal(body)
	c.bufferRaw(method, path, b)
}

func (c *Client) bufferRaw(method, path string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.buf) >= c.maxBuf {
		c.buf = c.buf[1:] // drop oldest
	}
	c.buf = append(c.buf, bufferedWrite{method: method, path: path, body: body})
}

// newVersionedRequest is the ONE door every control-plane request goes through.
//
// It exists for the header, and the header is why it is a chokepoint rather
// than a line repeated five times: the floor check on the far side runs pre-auth
// on every route (plan 48 step 3), so a call site that forgets the version does
// not degrade — it gets refused, mid-session, with the garbled failure this
// whole plan exists to kill. A missing helper call is a build failure
// (TestEveryControlPlaneRequestIsBuiltByTheOneHelper), which is the only form of
// "don't forget" that survives the next feature.
// upgradeRequired turns a 426 into the one sentence, and puts the server's
// fuller answer (their version, the floor, the fix) somewhere a debug run can
// still find it. Returning the sentinel UNWRAPPED is the whole point: every
// caller that prints this error prints one line.
func upgradeRequired(path string, body []byte) error {
	if detail := stringsTrim(string(body)); detail != "" {
		controlClientLog.Debugf("%s refused this build: %s", path, strings.ReplaceAll(detail, "\n", " "))
	}
	return ErrUpgradeRequired
}

func newVersionedRequest(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(VersionHeader, ClientVersion)
	return req, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return c.doWithPin(ctx, method, path, "", body, out)
}

func (c *Client) doWithPin(ctx context.Context, method, path, pinHint string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := newVersionedRequest(ctx, method, c.Base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	pin := pinHint
	if pin == "" {
		pin = pinFromAPIPath(path)
	}
	c.attachBearerForPin(req, pin)
	res, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode == http.StatusUpgradeRequired {
		return upgradeRequired(path, b)
	}
	if res.StatusCode == 409 {
		return ErrDemoted
	}
	if res.StatusCode == 404 {
		return fmt.Errorf("%w: %s", ErrNoSession, stringsTrim(string(b)))
	}
	if res.StatusCode == 401 {
		return fmt.Errorf("%w: %s", ErrUnauthorized, stringsTrim(string(b)))
	}
	if res.StatusCode == 400 {
		msg := stringsTrim(string(b))
		if msg == "" {
			msg = res.Status
		}
		return fmt.Errorf("%w: %s", ErrBadRequest, msg)
	}
	if res.StatusCode == 429 {
		msg := stringsTrim(string(b))
		if msg == "" {
			msg = res.Status
		}
		return fmt.Errorf("%w: %s", ErrRateLimited, msg)
	}
	if res.StatusCode >= 300 {
		msg := stringsTrim(string(b))
		if msg == "" {
			msg = res.Status
		}
		return fmt.Errorf("%s: %s", path, msg)
	}
	if out == nil || res.StatusCode == 204 || len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// attachBearer sets Authorization from an in-memory or on-disk member secret
// when the path names a PIN. Never put the secret in the URL — proxy logs would keep it.
func (c *Client) attachBearer(req *http.Request, path string) {
	c.attachBearerForPin(req, pinFromAPIPath(path))
}

// attachBearerForPin sets Authorization and the member header: memory first
// (provisioner), then disk.
//
// Both or neither. Sending the secret without the id would reach a server that
// answers ErrMissingBearer — the upgrade sentence — which is a confusing thing
// to tell a current binary, so a half-present membership is treated as none.
func (c *Client) attachBearerForPin(req *http.Request, pin string) {
	if pin == "" {
		return
	}
	if c != nil {
		c.mu.Lock()
		m := c.secrets[pin]
		c.mu.Unlock()
		if m.secret != "" && m.id != "" {
			setMemberAuth(req, m.id, m.secret)
			return
		}
	}
	if id, secret := session.ReadMembership(pin); secret != "" && id != "" {
		setMemberAuth(req, id, secret)
	}
}

// memberIDFor is this machine's member id for pin, from memory then disk.
func (c *Client) memberIDFor(pin string) string {
	if c != nil {
		c.mu.Lock()
		m := c.secrets[pin]
		c.mu.Unlock()
		if m.id != "" {
			return m.id
		}
	}
	id, _ := session.ReadMembership(pin)
	return id
}

func setMemberAuth(req *http.Request, memberID, secret string) {
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set(MemberHeader, memberID)
}

// membership is the pair every authenticated call carries.
type membership struct{ id, secret string }

// pinFromAPIPath pulls the PIN out of /v1/sessions/{pin}/…. Empty for the
// claim route (/v1/sessions) which has no PIN in the path yet.
func pinFromAPIPath(path string) string {
	const prefix = "/v1/sessions/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func stringsTrim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}

// useTelemetry points this process's client telemetry at the session, from what
// the member cycle just learned. A snapshot-less cycle still refreshes the
// ticket, so it reads the advertised ingest out of the cache rather than
// forgetting where to post for eleven cycles out of twelve.
func (c *Client) useTelemetry(pin, id string, sess *Session) {
	advertised := ""
	uid := ""
	if sess != nil {
		advertised, uid = sess.TelemetryURL, sess.UID
	} else if cached, ok := c.Cached(pin); ok {
		advertised, uid = cached.TelemetryURL, cached.UID
	}
	telemetry.UseMember(telemetry.MemberConfig{
		PIN: pin, SessionUID: uid, MemberID: id,
		Advertised: advertised,
		Ticket:     c.RelayTicket(pin, telemetry.TicketService),
		// The same constant this cycle puts in VersionHeader, so a row and the
		// request that carried it name one build rather than two facts nobody
		// can join. telemetry cannot read it itself — this package imports it.
		Version: ClientVersion,
	})
}
