package conductor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/devserver"
)

// LogSource is the dev server's *watchable* output — stderr the dev server
// itself produced. Dependency-install noise and slopball's own narration are
// excluded at the source (internal/devserver tags every line by stream and
// phase), which is what lets the watcher trigger on the stream rather than on a
// list of magic substrings. It matters that the watcher cannot reach the merged
// view: the host writes its own failure reports into that buffer quoting the
// log tail, so reading it would make the watcher retrigger on itself.
//
// A local host uses the in-process *devserver.LogBuffer directly; an off-box
// conductor uses RemoteLogSource to read the box's /logs endpoint over HTTP.
type LogSource interface {
	Watchable() string
}

// RemoteLogSource reads the canonical dev server's logs from a box's /logs HTTP
// endpoint. This is what lets an elected conductor on a laptop watch a dev
// server that is actually running on the cloud box. Failures return "" (no new
// logs) rather than erroring, so a transient box hiccup never wedges the fleet.
//
// It follows a HELD stream (plan 43 ticket 10) rather than fetching per tick:
// the watcher reads Watchable() every 2s, and every one of those used to cross
// the session network. What it hands back is unchanged — a *level* view of the
// watchable buffer — because that is what Watchable() means and what the
// watcher's Settle / MaxWait / Cooldown debounces are built on. Only the
// arrival mechanism moved, and arriving sooner changes none of them.
type RemoteLogSource struct {
	URL    string
	Client *http.Client

	// Resolve re-derives the /logs URL for each connection. dev is a placed
	// service and it migrates (plan 30), so reconnecting to the address this
	// first dialled follows a member that no longer holds the lease. Nil keeps
	// using URL.
	Resolve func(context.Context) (string, error)

	// Floor is how long to wait before re-dialling after the stream ends. It is
	// LogsFloor with a live session stream and MemberCycle without one, which
	// is the same collapse every other floor in plan 43 makes.
	Floor time.Duration

	mu       sync.Mutex
	started  bool
	lines    map[int64]devserver.Line // level view, keyed by seq
	order    []int64
	cursor   int64
	cancel   context.CancelFunc
	follower *devserver.Follower
}

// maxRemoteLines bounds the local level view. The server's own buffer is
// bounded too; this only keeps a long-lived follower from growing without one.
const maxRemoteLines = 2000

// Close ends the follower. Idempotent.
func (r *RemoteLogSource) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.started = false
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Reconnect drops the current connection so the next one re-resolves the URL.
// Called when the dev lease is known to have moved.
func (r *RemoteLogSource) Reconnect() {
	if r == nil {
		return
	}
	r.mu.Lock()
	f := r.follower
	r.mu.Unlock()
	f.Reconnect()
}

func (r *RemoteLogSource) floor() time.Duration {
	if r.Floor > 0 {
		return r.Floor
	}
	return controlplane.LogsFloor
}

func (r *RemoteLogSource) httpClient() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	// No Timeout: a held stream is not a request that finishes. The per-attempt
	// catch-up read below sets its own.
	return &http.Client{}
}

// record folds one page into the level view. Pages re-send an open line under
// the same Seq, so this replaces rather than appends — a follower that appended
// blindly would show a dying server's last line twice.
func (r *RemoteLogSource) record(page devserver.LogPage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lines == nil {
		r.lines = map[int64]devserver.Line{}
	}
	if page.Missed > 0 {
		// The server told us it dropped lines rather than pretending it had
		// not. Say so out loud; deliberately not folded into the watchable
		// text, which would feed the watcher a line slopball wrote.
		watcherLog.Warnf("dev-server log stream missed %d line(s) — the watcher is reading an incomplete buffer", page.Missed)
	}
	for _, l := range page.Lines {
		if _, seen := r.lines[l.Seq]; !seen {
			r.order = append(r.order, l.Seq)
		}
		r.lines[l.Seq] = l
	}
	if page.Cursor > r.cursor {
		r.cursor = page.Cursor
	}
	if len(r.order) > maxRemoteLines {
		drop := r.order[:len(r.order)-maxRemoteLines]
		r.order = r.order[len(r.order)-maxRemoteLines:]
		for _, seq := range drop {
			delete(r.lines, seq)
		}
	}
}

// Probe answers "can I actually read this?" — the question the startup line
// used to assume rather than ask. Watchable deliberately swallows failures so a
// transient hiccup never wedges the fleet, which also meant an endpoint nobody
// could dial was indistinguishable from a quiet dev server (plan 40).
func (r *RemoteLogSource) Probe() error {
	if r == nil || r.URL == "" {
		return fmt.Errorf("no dev-server logs URL")
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", r.URL, resp.Status)
	}
	return nil
}

// Watchable returns the current level view, starting the follower on first
// call. The first call also does one synchronous catch-up so a watcher that
// reads immediately is not handed an empty buffer it would read as "quiet".
func (r *RemoteLogSource) Watchable() string {
	if r == nil || r.URL == "" {
		return ""
	}
	r.ensureFollowing()
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, seq := range r.order {
		b.WriteString(r.lines[seq].Text)
		b.WriteString("\n")
	}
	return b.String()
}

func (r *RemoteLogSource) ensureFollowing() {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return
	}
	r.started = true
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.mu.Unlock()

	// One synchronous page, so the very first Watchable() reflects what the
	// buffer already holds rather than racing the follower's first connection.
	if page, err := r.catchUp(ctx, r.currentURL()); err == nil {
		r.record(page)
	}

	f := devserver.Follow(ctx, devserver.FollowOptions{
		URL:    r.resolveURL,
		Stream: devserver.StreamStderr,
		Phase:  devserver.PhaseDev,
		Client: r.Client,
		Floor:  r.floor(),
	}, r.reset)
	r.mu.Lock()
	r.follower = f
	r.mu.Unlock()
	go func() {
		for page := range f.C {
			r.record(page)
		}
	}()
}

func (r *RemoteLogSource) currentURL() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.URL
}

// resolveURL re-derives the endpoint per connection. dev is a placed service
// and it moves, so the address this first dialled goes stale.
func (r *RemoteLogSource) resolveURL(ctx context.Context) (string, error) {
	if r.Resolve == nil {
		return r.currentURL(), nil
	}
	u, err := r.Resolve(ctx)
	if err != nil || u == "" {
		return r.currentURL(), err
	}
	r.mu.Lock()
	r.URL = u
	r.mu.Unlock()
	return u, nil
}

// reset drops the level view when the follower moves to a different holder: a
// different dev server's output is not this one's, and keeping it would leave
// the watcher triggering on an error from a machine that no longer runs dev.
func (r *RemoteLogSource) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines, r.order, r.cursor = nil, nil, 0
}

func (r *RemoteLogSource) catchUp(ctx context.Context, base string) (devserver.LogPage, error) {
	var page devserver.LogPage
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	url := devserver.FollowQuery(base, 0, devserver.StreamStderr, devserver.PhaseDev)
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return page, err
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return page, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return page, fmt.Errorf("%s answered %s", base, resp.Status)
	}
	err = json.NewDecoder(resp.Body).Decode(&page)
	return page, err
}

// LogsURLFromRemote derives the /logs endpoint URL from a canonical git remote
// URL (e.g. http://box:37253/canonical.git → http://box:37253/logs). Returns ""
// for non-HTTP remotes (a file path or bare-repo remote has no log server).
func LogsURLFromRemote(remote string) string {
	u, err := url.Parse(remote)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/logs"
}
