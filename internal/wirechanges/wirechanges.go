// Package wirechanges reads the pending wire-change ledger — the
// `.wire-changes/` entries an agent files when slopball's wire surface moves
// (plan 48 step 1).
//
// One entry is one file, one class, and one sentence written from an OLD
// CLIENT'S point of view: the reader who matters is the copy of slopball
// somebody installed last week and will not rebuild. Entries are PENDING until
// a tag consumes them — `make next-version` (step 7) folds the sentences into
// CHANGELOG.md verbatim and empties the folder, the same lifecycle this repo
// uses for a plan that shipped.
//
// This package is the parser and the validator, nothing more. Every error names
// the FILE and the FIX, because the person reading it has just been stopped by a
// guard they did not know existed.
//
// ⚠️ THE PAIRING IS CROSS-REPO SINCE PLAN 49, and this half is the half that can
// be checked here. A `breaking` entry must NAME a floor — that is a property of
// the entry, so it is enforced in Validate. Whether the deployed control plane
// actually refuses everything below that floor is a property of
// `service.ClientVersionFloor`, which is service code in the private repo (ADR
// 0006 decision 0: the floor is the deployment's own, never config and never
// this module's). That repo pairs the two when it bumps its `require
// github.com/nwylynko/slopball-cli` pin, reading the floors out of this repo's
// CHANGELOG.md — which is why the changelog prints them.
package wirechanges

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Dir is where entries live, relative to the repo root. Repo root rather than
// under docs/ or internal/: the ledger is a property of the tree being tagged,
// and it is meant to be seen in `ls` by whoever is about to cut a release.
const Dir = ".wire-changes"

// FormatDoc is the entry format, pointed at by every error this package
// returns.
const FormatDoc = ".wire-changes/README.md"

// FloorConstantRef names the constant a floor is ultimately enforced by, and
// says where it is — across the repo line, which is the part an agent stopped by
// a guard here will otherwise waste an hour grepping this module for.
const FloorConstantRef = "ClientVersionFloor, in the private services repo (internal/controlplane/service/floor.go)"

// Playbook names the rollout policy BY NAME, because naming it is the only part
// of this machinery that stops an agent building a release nobody can upgrade
// into: replacing a shape in place satisfies every guard here — one `breaking`
// entry with a floor, tree green — and produces a build whose drain check can
// never pass, since the floor names the release being deployed and every live
// session is below it until laptops upgrade to the thing the check is gating.
//
// It points INTO THIS REPO, at the ledger's own README, and that is deliberate
// (plan 49 step 4): the author half of the playbook — what the two releases are,
// which release the floor may name, how a shim is marked — is about writing the
// change, and the change is written here. The deploy half — the drain check, the
// deploy order, the schema — is about running the deployment and lives with the
// deployment, in the private services repo's docs/security.md, which this file
// links to but cannot resolve.
//
// The title is quoted exactly as the heading is written, and
// TestThePlaybookPointerResolvesToARealSection resolves it against the file — a
// pointer at a section nobody can find reads like guidance that was deleted.
const Playbook = `the expand/contract playbook — "Expand/contract: shipping a wire change" in .wire-changes/README.md`

// The three classes, from the point of view of a client that was installed
// before the change and is never rebuilt.
const (
	// ClassBreaking — that client stops working against this build.
	ClassBreaking = "breaking"
	// ClassAdditive — new field, route or constant; that client is unaffected.
	ClassAdditive = "additive"
	// ClassPatch — the shape moved, nothing that client can observe changed.
	ClassPatch = "patch"
)

// Classes is every valid class, in bump order (largest first). `make
// next-version` (step 7) bumps by max(class) across the pending entries.
var Classes = []string{ClassBreaking, ClassAdditive, ClassPatch}

// Entry is one pending wire change.
type Entry struct {
	// File is the path as written in errors — `.wire-changes/<slug>.md`.
	File string
	// Slug is the filename without `.md`.
	Slug string
	// Class is breaking | additive | patch.
	Class string
	// Floor is the oldest client version that still works after this change.
	// Set on breaking entries and only on breaking entries.
	Floor string
	// Sentence is the one old-client's-POV line, copied verbatim into
	// CHANGELOG.md at tag time.
	Sentence string
}

var (
	slugPattern  = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	floorPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
)

// LoadPending reads every pending entry under <root>/.wire-changes, sorted by
// slug. A missing folder is an empty ledger, not an error — most commits change
// no wire. The FIRST malformed entry is returned as an error: a half-read
// ledger would let a guard pass on a tree that has a broken entry in it.
func LoadPending(root string) ([]Entry, error) {
	dir := filepath.Join(root, Dir)
	items, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", Dir, err)
	}
	var entries []Entry
	for _, item := range items {
		name := item.Name()
		if item.IsDir() || name == "README.md" || !strings.HasSuffix(name, ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		entry, err := ParseEntry(name, string(data))
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Slug < entries[j].Slug })
	return entries, nil
}

// ParseEntry reads one entry. name is the bare filename; body is its contents.
//
// The shape is deliberately dull — `key: value` header lines, a blank line,
// then the sentence — so that an agent writing one from the tripwire's failure
// text alone gets it right on the first attempt, and so a malformed one can be
// named precisely rather than "could not parse".
func ParseEntry(name, body string) (Entry, error) {
	path := Dir + "/" + name
	slug := strings.TrimSuffix(name, ".md")
	if !strings.HasSuffix(name, ".md") {
		return Entry{}, fmt.Errorf("%s: a wire-change entry must be a .md file — rename it %s.md. See %s", path, slug, FormatDoc)
	}
	if !slugPattern.MatchString(slug) {
		return Entry{}, fmt.Errorf("%s: %q is not a slug — use lowercase-words-with-dashes, e.g. member-version-field. See %s", path, slug, FormatDoc)
	}

	entry := Entry{File: path, Slug: slug}
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	i := 0
	for ; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return Entry{}, fmt.Errorf("%s: %q is not a `key: value` header — the entry starts with `class: breaking | additive | patch`, then a blank line, then the sentence. See %s", path, line, FormatDoc)
		}
		key = strings.TrimSpace(strings.ToLower(key))
		value = strings.TrimSpace(value)
		switch key {
		case "class":
			entry.Class = value
		case "floor":
			entry.Floor = value
		default:
			return Entry{}, fmt.Errorf("%s: unknown header %q — an entry carries `class:` and, when breaking, `floor:`. Everything else belongs in the sentence. See %s", path, key, FormatDoc)
		}
	}

	var prose []string
	for ; i < len(lines); i++ {
		if line := strings.TrimSpace(lines[i]); line != "" {
			prose = append(prose, line)
		}
	}
	entry.Sentence = strings.Join(prose, " ")

	if err := entry.Validate(); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

// Validate is every rule a pending entry has to satisfy. It is separate from
// parsing so a test can build an Entry by hand and check the same rules the
// files go through.
func (e Entry) Validate() error {
	switch e.Class {
	case "":
		return fmt.Errorf("%s: no `class:` line — add one naming exactly one of %s, from the point of view of a client installed BEFORE this change. See %s",
			e.File, strings.Join(Classes, " | "), FormatDoc)
	case ClassBreaking, ClassAdditive, ClassPatch:
	default:
		return fmt.Errorf("%s: class %q is not one of %s — `breaking` means an already-installed client stops working, `additive` means it is unaffected, `patch` means nothing it can observe changed. See %s",
			e.File, e.Class, strings.Join(Classes, " | "), FormatDoc)
	}

	if e.Sentence == "" {
		return fmt.Errorf("%s: no sentence — after the header lines and a blank line, write ONE sentence from an old client's point of view. It is copied verbatim into CHANGELOG.md at tag time, so write it for a reader. See %s",
			e.File, FormatDoc)
	}

	switch {
	case e.Class == ClassBreaking && e.Floor == "":
		return fmt.Errorf("%s: class: breaking with no `floor:` — add `floor: vX.Y.Z` naming the OLDEST client that still works after this change.\n\n"+
			"The floor is not decoration: it is copied into CHANGELOG.md when this entry is consumed, and %s reads it from there when the services repo bumps its pin. Without it the deployment garbles the clients this entry says are refused instead of refusing them with a sentence. See %s",
			e.File, FloorConstantRef, FormatDoc)
	case e.Class != ClassBreaking && e.Floor != "":
		return fmt.Errorf("%s: `floor:` on a %s entry means nothing — only a breaking change moves the floor. Drop the line, or reclassify the entry as breaking if an already-installed client really does stop working. See %s",
			e.File, e.Class, FormatDoc)
	case e.Floor != "" && !floorPattern.MatchString(e.Floor):
		return fmt.Errorf("%s: floor %q is not a release tag — write it as vX.Y.Z, the tag whose clients still work. See %s",
			e.File, e.Floor, FormatDoc)
	}
	return nil
}

// Breaking returns the pending entries that named a floor.
func Breaking(entries []Entry) []Entry {
	var out []Entry
	for _, e := range entries {
		if e.Class == ClassBreaking {
			out = append(out, e)
		}
	}
	return out
}
