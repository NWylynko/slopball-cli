package conductor

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/nwylynko/slopball-cli/canonical"
	"github.com/nwylynko/slopball-cli/controlplane"
)

// HTTPHealth probes the dev server the way a person checking the demo link
// would. It is the error-watcher's second trigger, independent of the logs: a
// wedged process, a route throwing at request time, or a server that died
// without a parting message all look fine in a log and broken in a browser.
//
// Only refusal, timeout and 5xx count as broken. A 4xx is the application
// answering — a route that does not exist is not a build the watcher should be
// rewriting, and treating it as one would have the fleet "fixing" every 404.
func HTTPHealth(url string) func(ctx context.Context) error {
	return HTTPHealthDynamic(func() string { return url })
}

// HTTPHealthDynamic is HTTPHealth for a URL that is not known yet at wiring
// time. A host that boots against an empty canonical has no dev endpoint until
// the project arrives and commits a PORT, so the probe has to ask each time
// rather than capture "" forever.
func HTTPHealthDynamic(urlFor func() string) func(ctx context.Context) error {
	client := &http.Client{Timeout: 5 * time.Second}
	return func(ctx context.Context) error {
		url := ""
		if urlFor != nil {
			url = urlFor()
		}
		if url == "" {
			return nil
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil // a URL we cannot even form is not the product's fault
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("dev server unreachable at %s: %w", url, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 500 {
			return fmt.Errorf("dev server answered %s with %s", url, resp.Status)
		}
		return nil
	}
}

// SessionDevURL is the announced dev endpoint for a session, which is what the
// health probe watches. Empty when the repo has not committed a PORT yet — the
// probe is then disabled rather than reporting a URL that was never meant to
// answer.
func SessionDevURL(ctx context.Context, client *controlplane.Client, pin string, host *canonical.Host) string {
	if client == nil {
		return ""
	}
	// Resolved, not raw: the health probe DIALS this, and a `slop://` address
	// probed literally reads as permanently down (plan 40).
	url, err := client.EndpointURL(ctx, pin, controlplane.EndpointDev)
	if err != nil {
		return ""
	}
	return url
}
