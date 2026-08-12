// Package reach probes whether this machine can reach a session's published
// services — the same checks slopball monitor uses, shared so the console and
// monitor never disagree on stage (plan 42).
package reach

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/sessionnet"
)

// SessionService is git or dev on the session network.
type SessionService string

const (
	ServiceGit SessionService = controlplane.EndpointGit
	ServiceDev SessionService = controlplane.EndpointDev
)

// Result is what a local probe found from here.
//
// Published and Reachable are separate facts on purpose. "Nobody has published
// this yet" is a session coming up; "it is published and I cannot reach it" is
// a problem. Collapsing them into one zero value made the console report the
// first as the second, so a starting session read as a broken one.
type Result struct {
	Published bool
	Reachable bool
	Via       string // relay|direct path tail from sessionnet.LastPath (git only)
}

// ProbeSessionService dials a session service the way a member would — through
// Dialable for published endpoints, never the raw `slop://` URL (plan 38).
func ProbeSessionService(ctx context.Context, client *controlplane.Client, sess controlplane.Session, pin string, svc SessionService) Result {
	if client == nil {
		return Result{}
	}
	switch svc {
	case ServiceGit:
		// raw endpoint ok: Dialable resolves before probe; published URL is not dialed.
		ep, ok := sess.Endpoints[controlplane.EndpointGit]
		if !ok || ep.URL == "" {
			return Result{}
		}
		dialable, err := client.Dialable(ctx, sess, ep.URL)
		if err != nil || dialable == "" {
			return Result{}
		}
		return Result{
			Published: true,
			Reachable: probeHTTP(ctx, gitInfoRefs(dialable)),
			Via:       sessionnet.LastPath(pin, controlplane.EndpointGit),
		}
	case ServiceDev:
		url, err := client.EndpointURL(ctx, pin, controlplane.EndpointDev)
		if err != nil || url == "" {
			// Not published, or not resolvable from here — either way there is
			// nothing to claim about it. The session poll reports a control plane
			// that has stopped answering; this must not report it twice as a dev
			// server nobody can reach.
			return Result{}
		}
		return Result{Published: true, Reachable: probeHTTP(ctx, url)}
	default:
		return Result{}
	}
}

func gitInfoRefs(dial string) string {
	if dial == "" {
		return ""
	}
	return strings.TrimRight(dial, "/") + "/info/refs?service=git-upload-pack"
}

// ProbeHTTP reports whether a GET to url succeeds within the probe timeout.
func ProbeHTTP(ctx context.Context, url string) bool {
	return probeHTTP(ctx, url)
}

func probeHTTP(ctx context.Context, url string) bool {
	if url == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}
