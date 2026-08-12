package devserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// FollowOptions configures a held reader of a remote /logs endpoint.
type FollowOptions struct {
	// URL resolves the endpoint for each connection. It is a function, not a
	// string, because dev is a placed service and it migrates (plan 30): a
	// follower that reconnects to the address it first dialled follows a member
	// that no longer holds the lease.
	URL func(context.Context) (string, error)

	// Stream and Phase are the server-side filter. Empty means everything —
	// the merged human view a console shows. The error-watcher asks for
	// stderr ∧ dev, which is what keeps slopball's own narration away from it.
	Stream Stream
	Phase  Phase

	Client *http.Client

	// Floor is how long to wait before re-dialling after a stream ends.
	Floor time.Duration

	// OnStatus reports every connection attempt — nil on success, the failure
	// otherwise. A follower that swallowed this would leave a viewer showing
	// "no dev-server output yet" forever for an endpoint nobody can dial,
	// which is the exact lie plan 40 removed from the startup line.
	OnStatus func(error)
}

// Follower is a held /logs reader. Pages arrive on C as the server appends
// them; a reconnect to a *different* holder restarts the cursor and says so
// with Reset, because sequence numbers belong to one dev server's buffer.
type Follower struct {
	C <-chan LogPage

	cancel context.CancelFunc
	kick   chan struct{}
}

// Close ends the follower.
func (f *Follower) Close() {
	if f == nil || f.cancel == nil {
		return
	}
	f.cancel()
}

// Reconnect drops the current connection so the next one re-resolves the URL.
func (f *Follower) Reconnect() {
	if f == nil || f.kick == nil {
		return
	}
	select {
	case f.kick <- struct{}{}:
	default:
	}
}

// Follow holds a /logs stream, re-dialling on its floor. A stream that ends is
// a reconnect, never a verdict — the dev server may simply have restarted.
//
// OnReset fires when the resolved endpoint changes, so a caller accumulating a
// view can drop what the previous holder produced.
func Follow(ctx context.Context, opt FollowOptions, onReset func()) *Follower {
	ctx, cancel := context.WithCancel(ctx)
	ch := make(chan LogPage, 32)
	f := &Follower{C: ch, cancel: cancel, kick: make(chan struct{}, 1)}
	go func() {
		defer close(ch)
		var lastURL string
		var cursor int64
		floor := opt.Floor
		if floor <= 0 {
			floor = 5 * time.Second
		}
		for {
			if ctx.Err() != nil {
				return
			}
			base, err := opt.resolve(ctx)
			if err == nil && base != "" {
				if base != lastURL {
					if lastURL != "" && onReset != nil {
						onReset()
					}
					lastURL, cursor = base, 0
				}
				cursor, err = followOnce(ctx, f.kick, opt, base, cursor, ch)
			}
			if opt.OnStatus != nil && ctx.Err() == nil {
				opt.OnStatus(err)
			}
			select {
			case <-ctx.Done():
				return
			case <-f.kick:
			case <-time.After(floor):
			}
		}
	}()
	return f
}

func (o FollowOptions) resolve(ctx context.Context) (string, error) {
	if o.URL == nil {
		return "", fmt.Errorf("no logs URL")
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return o.URL(rctx)
}

func (o FollowOptions) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	// No Timeout: a held stream is not a request that finishes.
	return &http.Client{}
}

// FollowQuery is the cursor+filter URL both the held stream and a plain paged
// GET use, so the two can never drift into asking for different things.
func FollowQuery(base string, cursor int64, stream Stream, phase Phase) string {
	q := fmt.Sprintf("%s?since=%d", strings.TrimSuffix(base, "/"), cursor)
	if stream != "" {
		q += "&stream=" + string(stream)
	}
	if phase != "" {
		q += "&phase=" + string(phase)
	}
	return q
}

// followOnce holds one connection and returns the cursor to resume from plus
// why it ended (nil when the stream simply closed).
func followOnce(ctx context.Context, kick <-chan struct{}, opt FollowOptions, base string, cursor int64, out chan<- LogPage) (int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-kick:
			cancel()
		case <-ctx.Done():
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, FollowQuery(base, cursor, opt.Stream, opt.Phase), nil)
	if err != nil {
		return cursor, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := opt.client().Do(req)
	if err != nil {
		return cursor, fmt.Errorf("dev-server logs at %s: %w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return cursor, fmt.Errorf("dev-server logs at %s: %s", base, resp.Status)
	}

	deliver := func(page LogPage) {
		if page.Cursor > cursor {
			cursor = page.Cursor
		}
		if len(page.Lines) == 0 && page.Missed == 0 {
			return
		}
		select {
		case out <- page:
		case <-ctx.Done():
		}
	}

	// A server that answers the paged JSON instead of a stream is read once and
	// re-read on the floor. That is the degradation rule, not a second path.
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		var page LogPage
		if derr := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&page); derr != nil {
			return cursor, fmt.Errorf("dev-server logs at %s: %w", base, derr)
		}
		deliver(page)
		return cursor, nil
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, ":"): // ping
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "":
			if data != "" {
				var page LogPage
				if err := json.Unmarshal([]byte(data), &page); err == nil {
					deliver(page)
				}
			}
			data = ""
		}
		if ctx.Err() != nil {
			return cursor, nil
		}
	}
	return cursor, nil
}
