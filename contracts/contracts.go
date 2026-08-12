// Package contracts emits the session's agent contract into a joined work tree
// (plans/12): one document, AGENTS.md, that every harness reads, plus a
// CLAUDE.md that points at it. One protocol, written once.
package contracts

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BriefFile is where the session's one-line project brief lives on main. It is
// committed rather than held in the control plane so it survives host migration
// and box cutover, and so every joined agent can read it (plan 28 §2).
const BriefFile = ".slopball/brief.md"

// Files are the contracts Install writes, relative to the work tree root. They
// live on main from the session's first commits (plan 31), which means a
// scaffolding generator can collide with them — so the setup role needs to know
// exactly which paths are slopball's to move aside and give back.
//
// AGENTS.md is the ONE source of truth and CLAUDE.md points at it. Three
// separately-worded copies of one protocol is three things to keep in step, and
// they had already drifted: only the Codex file ever said sync pulls before it
// pushes, so a Claude or Cursor agent was told when to publish and never when
// to refresh. Cursor reads AGENTS.md too, which is why the .mdc is in Legacy.
var Files = []string{"AGENTS.md", "CLAUDE.md"}

// Legacy is what Install used to write and now removes. A session that started
// before the collapse has the .mdc committed on main with `alwaysApply: true`,
// so every cursor agent in it keeps reading a stale copy of the protocol —
// stopping writing it is not enough.
var Legacy = []string{".cursor/rules/slopball.mdc"}

// ProtocolMarker is the instruction that makes a slopball session work: without
// it an agent edits happily and never lands anything. It doubles as the
// assertion that a contract survived being merged with one a scaffold wrote for
// itself (plan 31).
const ProtocolMarker = "slopball sync --intent"

// pointerMarker is what makes CLAUDE.md worth having. Losing it is as bad as
// losing the protocol itself: the file still looks like a contract and says
// nothing at all.
const pointerMarker = "@AGENTS.md"

// Requires is the substring rel must survive a scaffold merge with. It is
// per-file because the files no longer say the same thing: AGENTS.md carries
// the protocol, CLAUDE.md carries the pointer at it, and a guard that demanded
// the protocol from both would force the duplication this collapse removed.
func Requires(rel string) string {
	switch rel {
	case "AGENTS.md":
		return ProtocolMarker
	case "CLAUDE.md":
		return pointerMarker
	default:
		return ""
	}
}

// readBrief loads the brief from the work tree being written into — the tree
// was already cloned from main at both call sites (host start, join daemon), so
// nothing needs threading through, and a client joining an hour late still gets
// the current text. Contracts are rewritten on every join/start, so they track
// the file.
func readBrief(workDir string) string {
	b, err := os.ReadFile(filepath.Join(workDir, filepath.FromSlash(BriefFile)))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// isScaffolded reports whether main holds an actual project yet, as opposed to
// slopball's own bookkeeping. Its answer only ever softens the contract's
// wording, so a false negative costs a sentence, not correctness.
func isScaffolded(workDir string) bool {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return true
	}
	for _, e := range entries {
		switch e.Name() {
		case ".git", ".slopball", ".cursor", ".gitignore",
			"AGENTS.md", "CLAUDE.md", "README.md":
			continue // slopball's own artifacts, or the seed README
		}
		return true
	}
	return false
}

// briefSection is appended to the contract. Omitted entirely when there is no
// brief, so a session without one never grows an empty heading.
func briefSection(brief string, unscaffolded bool) string {
	s := "\n## What we're building\n\n" + brief + "\n"
	if unscaffolded {
		s += "\nThe project has not been scaffolded yet — `main` holds no code. " +
			"Building it is fair game: create it in your work tree and `slopball sync`, " +
			"and everyone else picks it up.\n"
	}
	return s
}

// Install writes the session's agent contract into workDir: AGENTS.md, which
// every harness reads, plus a CLAUDE.md that points at it. It also removes what
// earlier versions wrote (Legacy).
func Install(workDir, pin string) error {
	agents := contractBody(pin)
	if brief := readBrief(workDir); brief != "" {
		agents += briefSection(brief, !isScaffolded(workDir))
	}
	claude := fmt.Sprintf(`# slopball (Claude)

Session PIN: %s

@AGENTS.md

The session protocol lives there, and it is the same for every agent in this
session. Read it before you touch anything.
`, pin)

	for rel, body := range map[string]string{Files[0]: agents, Files[1]: claude} {
		path := filepath.Join(workDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
	}
	return removeLegacy(workDir)
}

// removeLegacy deletes contracts an earlier slopball wrote, and the directories
// that existed only to hold them — an emptied `.cursor/` is still an entry a
// scaffolding generator counts when it refuses a non-empty directory. It stops
// at any directory somebody else has a file in: `.cursor/rules/` is a shared
// place, and taking a teammate's rules out with ours would be its own bug.
func removeLegacy(workDir string) error {
	for _, rel := range Legacy {
		path := filepath.Join(workDir, filepath.FromSlash(rel))
		if err := os.Remove(path); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("remove the superseded contract %s: %w", rel, err)
			}
			continue
		}
		for dir := filepath.Dir(path); dir != workDir && strings.HasPrefix(dir, workDir); dir = filepath.Dir(dir) {
			if err := os.Remove(dir); err != nil {
				break // not empty: somebody else's files live here
			}
		}
	}
	return nil
}

// contractBody is the protocol every agent in the session follows. It is one
// document because it is one protocol — see Files.
func contractBody(pin string) string {
	return fmt.Sprintf(`# slopball agent contract

You are building inside a slopball session (PIN %s), alongside other AI agents
working on the same project at the same time. This file is how you play with them.

## The loop

Run slopball commands from inside this work tree — the session is picked up from
the folder, so you never pass --pin (set $SLOPBALL_PIN only if you run from elsewhere).

1. **Before you start work, run `+"`"+`slopball pull`+"`"+`.** Your teammates have been landing
   changes since you last looked, and this brings them into your branch. Starting
   without it means building against a stale copy of the project.
2. **Do the work.** Edit freely — you are on your own branch and nobody else is on it.
   There are no file locks and no ownership: two agents editing the same file is
   expected and handled.
3. **When the task is done, run `+"`"+`slopball sync --intent "<what you changed and why>"`+"`"+`.**
   That publishes your work to the rest of the team and brings theirs back down.
   Until you run it, nothing you did exists for anyone else.

Then loop: pull, work, sync. Both ends matter — an agent that never pulls
duplicates work that already exists, and one that never syncs does work nobody
ever sees.

## Conflicts are yours

`+"`"+`pull`+"`"+` and `+"`"+`sync`+"`"+` both integrate the latest `+"`"+`main`+"`"+` into your branch before anything
is published. If that leaves conflict markers, resolve them — you have the
context on your own change that nobody else does — and then re-run the command.
Slopball refuses to publish a tree that still has markers in it, so an unresolved
conflict stops at your machine instead of reaching the team.

The intent note is not paperwork: when your change does collide with somebody
else's, it is what the merger reads to resolve it well. Say what you changed and why.

## Seeing it run

There are two different things to look at, and confusing them wastes an hour.

- **The session's site — `+"`"+`slopball site`+"`"+`** prints the URL it is open on from this
  machine. That site serves `+"`"+`main`+"`"+`: everyone's merged work, not yours. After a
  `+"`"+`sync`+"`"+`, this is where you check your change actually landed and still works
  alongside everyone else's.
- **Your own branch — `+"`"+`slopball dev-setup`+"`"+`** prints the project's install and dev
  commands and where to run them. Nothing serves your branch until you do this,
  so it is the only way to see a change you have not synced yet.

Both are commands rather than URLs written in this file, because both answers
differ per machine and this file is shared by every agent in the session.

## Other commands

- `+"`"+`slopball run -- <cmd>`+"`"+` — run a command on the host (where the dev server lives).
- `+"`"+`slopball elect`+"`"+` — cloud-box sessions only, and only if asked: it makes this
  machine the one that runs the session's conductor agent under your harness
  login. Nothing secret leaves this machine; `+"`"+`--revoke`+"`"+` undoes it.

Never ask the human to run a git command, and never surface git to them. Slopball
drives git for you; that is the whole point of it.

## Runtime coherence

- The dev server binds port 3000. Full stop. Frameworks that honour `+"`"+`PORT`+"`"+` get it
  from slopball; Vite and similar must set `+"`"+`server.port`+"`"+` (or the ecosystem
  equivalent) to 3000. Do not move it — the session-network splice dials 3000.
- Shared env goes in `+"`"+`.slopball/runtime.env`+"`"+` (tracked). The host materializes
  `+"`"+`.env`+"`"+` from it and restarts the dev server when it changes; everyone agrees via sync.
  A `+"`"+`PORT=`+"`"+` line there does not choose the splice target.
- Migration files (`+"`"+`migrations/`+"`"+` and the like) are applied on the host automatically
  after a merge. Do not run them yourself.
- A destructive database reset or reseed happens only when your intent note contains
  `+"`"+`slopball:reseed`+"`"+` — never silently.
- Everything else under `+"`"+`.slopball/`+"`"+` belongs to slopball. Do not edit it.
`, pin)
}
