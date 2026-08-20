package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Standup is the window between "this machine has a session directory" and
// "that directory holds a usable work tree". It exists because those two facts
// used to be reported by the same file at the same moment — the live marker,
// written as the LAST act of Join/Start — so for the whole of a clone plus
// every network round trip after it, `slopball open` (and cd, workspace,
// claude, codex) told you the session did not exist while you were standing in
// its folder.
//
// Two things are recorded, not one:
//
//   - the marker appears IMMEDIATELY, so the session is discoverable — a
//     process holds it, which is exactly what LiveMarker always claimed to
//     mean;
//   - LiveMarker.ReadyAt appears when the work tree is usable.
//
// The wait between them is a real lock, not a poll. The standing-up process
// holds an exclusive flock on standup.lock for the duration; a waiter asks for
// a shared one and the kernel wakes it the moment the lock is dropped. That is
// deliberate: polling for a file to appear is the timer this repo tells you to
// treat as a race you have not tackled yet, and it would trade a false answer
// for a slow one rather than for a correct one.
//
// It also means a standup that DIES unblocks its waiters for free — flock is
// released when the process exits, however it exits — so a waiter can never
// outlive the thing it is waiting for. The waiter re-reads the marker after
// the wake and says so, instead of dropping a human into a half-cloned tree.
type Standup struct {
	pin string
	f   *os.File
}

// standupLock is the lock file's name inside the session root.
const standupLock = "standup.lock"

// BeginStandup claims the session directory for this process and returns the
// handle that ends the claim. Call it as soon as the root exists and BEFORE
// the clone — the whole point is to be discoverable while the slow part runs.
//
// The caller must `defer st.Release()`: a standup that fails partway has to
// drop the lock, or every waiter blocks until the process exits.
func BeginStandup(pin string, role Role, branch string) (*Standup, error) {
	p := ForPin(pin)
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(p.Root, standupLock), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open standup lock for %s: %w", pin, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock standup for %s: %w", pin, err)
	}
	// Discoverable from here on: pid-checked, not yet ready.
	if err := WriteLiveMarker(pin, LiveMarker{
		PID:       os.Getpid(),
		Role:      role,
		Branch:    BranchLabel(branch),
		StartedAt: time.Now().UTC(),
	}); err != nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, err
	}
	return &Standup{pin: pin, f: f}, nil
}

// Ready records that the work tree is usable and wakes every waiter. Call it
// the moment that is true — after the clone and the contracts, not after the
// last network round trip, or the window this type exists to close reopens
// with a different name.
func (s *Standup) Ready(meta Session) error {
	if s == nil {
		return nil
	}
	m := LiveMarker{
		PID:       os.Getpid(),
		Role:      meta.Role,
		Branch:    BranchLabel(meta.Branch),
		StartedAt: time.Now().UTC(),
		ReadyAt:   time.Now().UTC(),
	}
	// Keep what the early marker already recorded — StartedAt is the honest
	// standup duration and a dev URL may already have been published.
	if prev, ok := readMarker(s.pin); ok {
		m.StartedAt = prev.StartedAt
		m.DevURL = prev.DevURL
		if m.Branch == "" {
			m.Branch = prev.Branch
		}
	}
	if err := WriteLiveMarker(s.pin, m); err != nil {
		return err
	}
	// unlock, NOT Release: Release treats a still-held lock as a failed standup
	// and sweeps the marker, which would delete the one just written.
	s.unlock()
	return nil
}

// Release drops the lock without claiming readiness. Idempotent, so the
// deferred call after a successful Ready is a no-op rather than a second
// unlock.
//
// Reaching here still holding the lock means the standup FAILED — Ready hands
// the lock over on the way out — so the marker it wrote comes back off disk
// too. Leaving it would advertise a session this machine never finished
// building, and the pid check would only sweep it once the process exited,
// which for the wizard is not until the human quits.
func (s *Standup) Release() {
	if s == nil || s.f == nil {
		return
	}
	_ = ClearLive(s.pin)
	s.unlock()
}

// unlock drops the flock and forgets the handle, leaving the marker alone.
func (s *Standup) unlock() {
	if s == nil || s.f == nil {
		return
	}
	syscall.Flock(int(s.f.Fd()), syscall.LOCK_UN)
	s.f.Close()
	s.f = nil
}

// WaitUntilReady blocks until pin's work tree is usable.
//
// It returns immediately when the session is already ready, which is the
// common case — the cost is one file read. Otherwise it waits on the standup
// lock and re-checks, so a standup that died while we waited is reported as
// what it is instead of being mistaken for success.
//
// ctx is honoured while waiting: the flock itself is not cancellable, so it
// runs on its own goroutine and this returns on whichever lands first. The
// goroutine is bounded by the standup it is waiting on, and this is a CLI verb
// about to exit either way.
func WaitUntilReady(ctx context.Context, pin string) error {
	if m, ok := LiveHere(pin); ok && openable(pin, m) {
		return nil
	}
	path := filepath.Join(ForPin(pin).Root, standupLock)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("wait for session %s: %w", pin, err)
	}
	defer f.Close()

	locked := make(chan error, 1)
	go func() { locked <- syscall.Flock(int(f.Fd()), syscall.LOCK_SH) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-locked:
		if err != nil {
			return fmt.Errorf("wait for session %s: %w", pin, err)
		}
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}

	if m, ok := LiveHere(pin); ok && openable(pin, m) {
		return nil
	}
	return fmt.Errorf("session %s stopped standing up before its work tree was ready — "+
		"check the terminal running `slopball join %s`", pin, pin)
}

// StandingUp reports whether a live process holds pin but has not finished
// building its work tree. It is what lets a verb say "waiting" instead of
// printing nothing while it blocks.
func StandingUp(pin string) bool {
	m, ok := LiveHere(pin)
	return ok && !openable(pin, m)
}

// openable is the real question — "is there a work tree here I can put someone
// into" — and ReadyAt is only the fast way to answer it.
//
// The fallback exists for VERSION SKEW, which is the one way this could have
// shipped as a hang. A join daemon started by a build from before ReadyAt
// existed keeps running across an upgrade, and its marker has no readyAt in it;
// a new `slopball open` reading that marker would decide a perfectly healthy
// session was mid-standup and refuse it. The tree on disk is the older, truer
// witness, so it settles the case.
//
// It is safe to consult only because the caller has already established that a
// LIVE process holds the session: an abandoned standup clears its marker
// (Standup.Release) and a dead one fails the pid check, so neither reaches
// here to have a half-written clone mistaken for a finished one.
func openable(pin string, m LiveMarker) bool {
	if !m.ReadyAt.IsZero() {
		return true
	}
	_, err := os.Stat(filepath.Join(ForPin(pin).Work, ".git"))
	return err == nil
}

// ReadyHere is LiveHere plus "and you can open it": a process holds this
// session on this machine AND its work tree is built.
//
// It is the answer to "is the daemon up", which is what LiveHere used to mean
// by accident — liveness now begins at the top of standup, so anything that
// wants the END of standup has to ask for readiness explicitly.
func ReadyHere(pin string) (LiveMarker, bool) {
	m, ok := LiveHere(pin)
	if !ok || !openable(pin, m) {
		return LiveMarker{}, false
	}
	return m, true
}

func readMarker(pin string) (LiveMarker, bool) {
	b, err := os.ReadFile(ForPin(pin).Live())
	if err != nil {
		return LiveMarker{}, false
	}
	var m LiveMarker
	if err := json.Unmarshal(b, &m); err != nil {
		return LiveMarker{}, false
	}
	return m, true
}
