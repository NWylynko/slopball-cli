// Package cutover flips a session onto a relocated canonical (cloud box or
// migrated host) via the control plane and optionally catch-ups main first
// (plan 22 + 24). Client half is syncengine.FollowHost on the next sync —
// this package is only the host-side announce.
package cutover

import (
	"context"
	"fmt"

	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/logx"
	"github.com/nwylynko/slopball-cli/syncengine"
)

var log = logx.New("cutover")

// Options for a control-plane flip.
type Options struct {
	PIN         string
	NewDialAddr string // box / new host git URL
	LogsURL     string // optional /logs sibling
	OverlayAddr string // empty → slop-<pin>.host
	Control     *controlplane.Client
	// Generation is the writer's current generation (required).
	Generation  int
	HostMachine string
	// FromURL, when set, is the old canonical. Catch-up pushes its main into
	// NewDialAddr before the flip.
	FromURL string
	Drain   bool
}

// Result of a completed cutover.
type Result struct {
	DialAddr   string
	Generation int
	CaughtUp   bool
	Drained    bool
}

// Flip catch-ups (optional) then posts /cutover on the control plane. Existing
// clients pick it up on their next sync via syncengine.FollowHost — no re-join.
func Flip(ctx context.Context, opt Options) (*Result, error) {
	if opt.PIN == "" || opt.NewDialAddr == "" {
		return nil, fmt.Errorf("cutover: PIN and NewDialAddr required")
	}
	if opt.Control == nil {
		return nil, fmt.Errorf("cutover: need Control client")
	}
	if opt.Generation <= 0 {
		// Look up current generation if caller didn't know.
		sess, err := opt.Control.Session(ctx, opt.PIN)
		if err != nil {
			return nil, fmt.Errorf("cutover: resolve generation: %w", err)
		}
		opt.Generation = sess.Generation
	}
	res := &Result{DialAddr: opt.NewDialAddr, Drained: opt.Drain}
	if opt.FromURL != "" && !sameURL(opt.FromURL, opt.NewDialAddr) {
		if opt.Drain {
			log.Infof("drain: refreshing %s from %s before flip", opt.NewDialAddr, opt.FromURL)
		} else {
			log.Infof("catch-up: pushing main %s → %s before flip", opt.FromURL, opt.NewDialAddr)
		}
		if err := syncengine.CatchUp(ctx, opt.FromURL, opt.NewDialAddr); err != nil {
			return nil, fmt.Errorf("cutover catch-up: %w", err)
		}
		res.CaughtUp = true
	}
	sess, err := opt.Control.Cutover(ctx, opt.PIN, controlplane.CutoverRequest{
		NewGitURL:   opt.NewDialAddr,
		From:        opt.FromURL,
		Drain:       opt.Drain,
		Generation:  opt.Generation,
		LogsURL:     opt.LogsURL,
		HostMachine: opt.HostMachine,
	})
	if err != nil {
		return nil, fmt.Errorf("cutover: %w", err)
	}
	res.Generation = sess.Generation
	log.Infof("control plane flipped: %s → %s (gen %d; clients follow on next sync)",
		opt.PIN, opt.NewDialAddr, sess.Generation)
	return res, nil
}

func sameURL(a, b string) bool {
	trim := func(s string) string {
		for len(s) > 0 && s[len(s)-1] == '/' {
			s = s[:len(s)-1]
		}
		return s
	}
	return trim(a) == trim(b)
}
