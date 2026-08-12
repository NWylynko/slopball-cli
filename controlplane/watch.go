package controlplane

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
)

// streamState tracks whether Watch is live and receiving bytes for a pin.
// Freshness comes from bytes actually received (including the server's
// SSEPingInterval ping) — never from the fact that a socket is still open, and
// never from the server's opinion, because a buffering proxy makes the
// server's view a lie.
type streamState struct {
	cancel   context.CancelFunc
	started  bool      // Watch has claimed this pin; Updates alone must not
	lastByte time.Time // zero until the first byte of the first connection
	mu       sync.Mutex
	subs     []chan SessionUpdate
}

// sawBytes records that the stream delivered something — any byte, including a
// ping comment, proves the far end is still there.
func (st *streamState) sawBytes() {
	st.mu.Lock()
	st.lastByte = time.Now()
	st.mu.Unlock()
}

// silent marks the stream as having delivered nothing, which is what a
// disconnect means. The next connection's first byte revives it.
func (st *streamState) silent() {
	st.mu.Lock()
	st.lastByte = time.Time{}
	st.mu.Unlock()
}

// SessionUpdate is one pushed session frame (plan 43). Subscribers use it to
// wake mirror/conductor fetches without polling.
type SessionUpdate struct {
	Session Session
	Events  []Event
}

// Watch holds the session's SSE stream for pin, reconnects with jittered
// backoff, and writes each `event: session` frame into the client's cache.
// Watching is explicit — one-shot verbs must not call this. Cancel ctx (or
// the client's CloseWatch) to stop; a stream that ends is a reconnect, never
// a verdict that the session is gone.
func (c *Client) Watch(ctx context.Context, pin string) {
	c.mu.Lock()
	if c.watching == nil {
		c.watching = map[string]*streamState{}
	}
	st := c.watching[pin]
	if st == nil {
		st = &streamState{}
		c.watching[pin] = st
	}
	// A state built by Updates() has subscribers but no stream. Adopt it rather
	// than treating it as an existing watch — returning here would leave the
	// member silently polling, with nothing logged to say why.
	if st.started {
		c.mu.Unlock()
		return // already watching
	}
	wctx, cancel := context.WithCancel(ctx)
	st.started, st.cancel = true, cancel
	c.mu.Unlock()

	go c.watchLoop(wctx, pin, st)
}

// CloseWatch stops the stream for pin. Idempotent.
func (c *Client) CloseWatch(pin string) {
	c.mu.Lock()
	st := c.watching[pin]
	if st != nil {
		delete(c.watching, pin)
	}
	c.mu.Unlock()
	if st != nil {
		if st.cancel != nil {
			st.cancel()
		}
		st.mu.Lock()
		for _, ch := range st.subs {
			close(ch)
		}
		st.subs = nil
		st.mu.Unlock()
	}
}

// Updates returns a channel of session frames for pin. The caller must drain
// it; slow consumers drop frames (level-triggered — the next one carries the
// latest). CloseWatch closes all update channels.
func (c *Client) Updates(pin string) <-chan SessionUpdate {
	ch := make(chan SessionUpdate, 8)
	c.mu.Lock()
	if c.watching == nil {
		c.watching = map[string]*streamState{}
	}
	st := c.watching[pin]
	if st == nil {
		st = &streamState{}
		c.watching[pin] = st
	}
	st.mu.Lock()
	st.subs = append(st.subs, ch)
	st.mu.Unlock()
	c.mu.Unlock()
	return ch
}

// maxFoldedEvents bounds how many work-feed events one un-drained subscriber
// can accumulate. A consumer that far behind is not coming back; every real
// one drains each cycle.
const maxFoldedEvents = 1000

func (st *streamState) publish(u SessionUpdate) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, ch := range st.subs {
		next := u
		// The document is level-triggered — latest wins — but the events are a
		// log, so a full channel folds the queued frame's events forward
		// instead of dropping them. A dropped `main.advanced` costs a client
		// mirror a whole MirrorFloor, silently.
		for {
			select {
			case ch <- next:
			default:
				select {
				case old := <-ch:
					merged := make([]Event, 0, len(old.Events)+len(next.Events))
					merged = append(merged, old.Events...)
					merged = append(merged, next.Events...)
					if len(merged) > maxFoldedEvents {
						merged = merged[len(merged)-maxFoldedEvents:]
					}
					next = SessionUpdate{Session: next.Session, Events: merged}
				default:
				}
				continue
			}
			break
		}
	}
}

// watchingPin reports whether Watch has been started for pin (even mid-reconnect).
func (c *Client) watchingPin(pin string) bool {
	c.mu.Lock()
	st := c.watching[pin]
	c.mu.Unlock()
	return st != nil && st.started
}

// silentAfter is how long without a byte before the stream is judged dead.
func (c *Client) silentAfter() time.Duration {
	if c.StreamSilentAfter > 0 {
		return c.StreamSilentAfter
	}
	return StreamSilentAfter
}

// StreamLive reports whether Watch is holding a stream for pin that has
// delivered bytes inside StreamSilentAfter. This is the one input to the
// degradation rule, so it must measure delivery and not connectedness — a
// black-holed connection stays open indefinitely and would otherwise read live
// forever, leaving every floor at its healthy value on a stream carrying
// nothing. Session() uses watchingPin instead, so a reconnect blip serves the
// cache rather than starting a GET storm.
func (c *Client) StreamLive(pin string) bool {
	c.mu.Lock()
	st := c.watching[pin]
	c.mu.Unlock()
	if st == nil {
		return false
	}
	st.mu.Lock()
	last := st.lastByte
	st.mu.Unlock()
	return !last.IsZero() && time.Since(last) < c.silentAfter()
}

func (c *Client) watchLoop(ctx context.Context, pin string, st *streamState) {
	var since int64
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			st.silent()
			return
		}
		next, err := c.watchOnce(ctx, pin, since, st)
		if ctx.Err() != nil {
			st.silent()
			return
		}
		st.silent()
		if err == nil {
			since = next
			backoff = time.Second
			continue
		}
		// 401 / gone session are terminal — never reconnect-loop (plan 44 ticket 09).
		if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrNoSession) {
			return
		}
		// Jittered backoff; a stream that ends is a reconnect, never a verdict.
		jitter := time.Duration(rand.Int63n(int64(backoff/2) + 1))
		sleep := backoff/2 + jitter
		if sleep > 30*time.Second {
			sleep = 30 * time.Second
		}
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

// WatchOnce holds the session's SSE stream for exactly one attempt and returns
// when that attempt ends: the last event id it consumed, and the reason it
// stopped. Watch is the reconnect loop around this; WatchOnce is one turn of it.
//
// Exported because the held stream is the one control-plane door whose answer
// Watch deliberately throws away — watchLoop treats every error as a reconnect,
// by design (a stream that ends is never a verdict). That leaves no other way
// to ask what the far side actually said, and one of the answers is load-bearing:
// a 426 must render as plan 48's single upgrade sentence here exactly as it does
// on every request door, or a refused daemon sits for hours reporting a
// paragraph nobody can act on. The state a run accumulates stays private —
// this builds its own and drops it, because a single attempt has nothing to
// carry forward.
func (c *Client) WatchOnce(ctx context.Context, pin string, since int64) (int64, error) {
	return c.watchOnce(ctx, pin, since, &streamState{})
}

func (c *Client) watchOnce(ctx context.Context, pin string, since int64, st *streamState) (int64, error) {
	// No overall Timeout — this is a held connection. The caller's ctx cancels it.
	httpClient := &http.Client{Timeout: 0}
	if c.HTTP != nil && c.HTTP.Transport != nil {
		httpClient.Transport = c.HTTP.Transport
	}
	// This connection dies when it stops delivering, not only when it closes.
	// Nothing else notices a black hole: there is no read deadline on a held
	// stream, and TCP will sit on a socket a buffering proxy is holding open
	// for minutes. Cancelling here is what turns "silent" into a reconnect.
	reqCtx, dropSilent := context.WithCancel(ctx)
	defer dropSilent()
	req, err := newVersionedRequest(reqCtx, http.MethodGet,
		fmt.Sprintf("%s/v1/sessions/%s/events?since=%d", c.Base, pin, since), nil)
	if err != nil {
		return since, err
	}
	req.Header.Set("Accept", "text/event-stream")
	c.attachBearer(req, "/v1/sessions/"+pin+"/events")
	res, err := httpClient.Do(req)
	if err != nil {
		return since, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUpgradeRequired {
		b, _ := io.ReadAll(res.Body)
		return since, upgradeRequired("the session stream", b)
	}
	if res.StatusCode == 404 {
		return since, fmt.Errorf("%w", ErrNoSession)
	}
	if res.StatusCode == 401 {
		return since, fmt.Errorf("%w", ErrUnauthorized)
	}
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(res.Body)
		return since, fmt.Errorf("watch %s: %s", pin, stringsTrim(string(b)))
	}
	st.sawBytes()

	silent := c.silentAfter()
	watchdog := time.NewTicker(silent / 3)
	defer watchdog.Stop()
	go func() {
		for {
			select {
			case <-reqCtx.Done():
				return
			case <-watchdog.C:
				st.mu.Lock()
				last := st.lastByte
				st.mu.Unlock()
				if !last.IsZero() && time.Since(last) >= silent {
					dropSilent()
					return
				}
			}
		}
	}()

	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var event, data string
	last := since
	for sc.Scan() {
		line := sc.Text()
		// Any byte (including ": ping") proves the far end is still there.
		st.sawBytes()
		switch {
		case strings.HasPrefix(line, ":"):
			// comment / ping
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "":
			if event == "session" && data != "" {
				var frame struct {
					Session Session `json:"session"`
					Events  []Event `json:"events"`
				}
				if err := json.Unmarshal([]byte(data), &frame); err == nil && frame.Session.PIN != "" {
					c.mu.Lock()
					c.cache[pin] = frame.Session
					c.mu.Unlock()
					for _, e := range frame.Events {
						if e.Seq > last {
							last = e.Seq
						}
					}
					st.publish(SessionUpdate{Session: frame.Session, Events: frame.Events})
				}
			} else if data != "" {
				// Work-feed event frame — advance since so reconnects do not
				// re-play, but do not touch the session cache.
				var e Event
				if err := json.Unmarshal([]byte(data), &e); err == nil && e.Seq > last {
					last = e.Seq
				}
			}
			event, data = "", ""
		}
		if ctx.Err() != nil {
			return last, ctx.Err()
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return last, err
	}
	return last, io.EOF
}
