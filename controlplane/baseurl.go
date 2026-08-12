package controlplane

import (
	"os"
	"strings"
)

// DefaultURL is the control-plane address a binary dials with an empty
// environment. Local builds leave this as loopback so `make build` needs no
// input and BindForControl stays on this machine. Release/CI stamps a
// deployment hostname via:
//
//	-ldflags "-X github.com/nwylynko/slopball/internal/controlplane.DefaultURL=<url>"
//
// (hence a var, not a const — Go cannot -X a const). A stale binary naming a
// retired deployment is an accepted outcome: the control-plane URL is the
// bootstrap, and nothing can carry its own replacement.
var DefaultURL = "http://127.0.0.1:7777"

// BaseURL resolves the control-plane URL.
// Order: override (typically --control) → $SLOPBALL_CONTROL → DefaultURL.
func BaseURL(override string) string {
	if u := strings.TrimSpace(override); u != "" {
		return strings.TrimRight(u, "/")
	}
	if u := strings.TrimSpace(os.Getenv("SLOPBALL_CONTROL")); u != "" {
		return strings.TrimRight(u, "/")
	}
	return DefaultURL
}
