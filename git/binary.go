package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

// Package git is the *only* way the rest of slopball touches git. Every call
// goes through a hermetic wrapper around a pinned bundled binary — never the
// machine's system git — so identical merges produce identical results on every
// machine (plans/01, MASTERPLAN §9.1).

var (
	resolveMu      sync.Mutex
	resolvedPrefix string
	resolvedBin    string
	resolveErr     error
)

// Prefix returns the extracted git distribution root (contains cmd/, bin/,
// libexec/, share/).
func Prefix() (string, error) {
	home := os.Getenv("SLOPBALL_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(h, ".slopball")
	}
	return filepath.Join(home, "git", Version+"-"+runtime.GOOS+"-"+runtime.GOARCH), nil
}

// Bin returns the absolute path to the bundled git launcher (cmd/git). Extracts
// the embedded archive into Prefix() on first use.
//
// The resolution is memoized per-prefix, not globally: SLOPBALL_HOME does not
// move mid-process in production (so this is one resolve, as before), but in
// tests it moves constantly, and a globally-memoized answer meant a later test
// got a path inside an earlier test's already-deleted TempDir.
func Bin() (string, error) {
	prefix, err := Prefix()
	if err != nil {
		return "", err
	}

	resolveMu.Lock()
	defer resolveMu.Unlock()
	if resolvedPrefix == prefix && (resolvedBin != "" || resolveErr != nil) {
		return resolvedBin, resolveErr
	}
	resolvedBin, resolveErr = resolveBin()
	resolvedPrefix = prefix
	return resolvedBin, resolveErr
}

func resolveBin() (string, error) {
	prefix, err := Prefix()
	if err != nil {
		return "", err
	}
	launcher := filepath.Join(prefix, "cmd", "git")
	if st, err := os.Stat(launcher); err == nil && !st.IsDir() {
		return launcher, nil
	}
	// Also accept a prefix that only has bin/git (dev / alternate builds).
	alt := filepath.Join(prefix, "bin", "git")
	if st, err := os.Stat(alt); err == nil && !st.IsDir() {
		return alt, nil
	}
	if len(embeddedArchive) == 0 {
		// No bundled archive for this platform (e.g. darwin has no static git
		// asset yet, plan 01). Bundled git is load-bearing for MERGE DETERMINISM,
		// which happens on the host — a client that only clones/fetches/pushes
		// can safely borrow a system git rather than hard-fail. Prefer that over
		// a dead binary, but warn loudly so it's never silent.
		return SystemGitFallback()
	}
	if err := ExtractArchive(embeddedArchive, prefix); err != nil {
		return "", fmt.Errorf("extract bundled git: %w", err)
	}
	if _, err := os.Stat(launcher); err != nil {
		return "", fmt.Errorf("bundled git launcher missing after extract: %s", launcher)
	}
	return launcher, nil
}

// SystemGitFallback finds a git on PATH for platforms with no bundled asset.
//
// Exported because it is the entirety of the narrow client-only escape hatch
// (docs/packages.md → internal/git) and Bin() cannot reach it anywhere the
// suite runs: every platform with an embedded archive resolves a bundled git
// long before this line, so the only machine that would exercise it through
// the public door is the darwin laptop that never runs the suite. Calling it
// directly is what stops "borrow a system git rather than hard-fail" from
// being untested code on the day it matters.
func SystemGitFallback() (string, error) {
	if sys, lerr := exec.LookPath("git"); lerr == nil {
		warnSystemGitOnce(sys)
		return sys, nil
	}
	return "", fmt.Errorf("bundled git %s not available for %s/%s and no system git on PATH; run `make fetch-git` or install git", Version, runtime.GOOS, runtime.GOARCH)
}

var warnOnce sync.Once

// warnSystemGitOnce notes (once) that we fell back to a non-bundled git. Merge
// determinism only matters on the host, which ships bundled git; this path is
// for clients on platforms without a bundled asset.
func warnSystemGitOnce(path string) {
	warnOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "slopball: no bundled git for %s/%s — using system git at %s (fine for a client; the host still uses bundled git)\n",
			runtime.GOOS, runtime.GOARCH, path)
	})
}

// ResetResolveForTest clears the cached binary path so tests can point at a
// fresh prefix. Not for production use.
func ResetResolveForTest() {
	resolveMu.Lock()
	defer resolveMu.Unlock()
	resolvedPrefix = ""
	resolvedBin = ""
	resolveErr = nil
}
