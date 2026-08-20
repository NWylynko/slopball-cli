// Package nextversion derives the next release number from the wire-change
// ledger, and prints what it would take to cut it (plan 48 step 7).
//
// The number is DERIVED, never chosen: `.wire-changes/` entries were classified
// at the moment the change was made, by whoever knew what an already-installed
// client would see, and the next version is the last tag bumped by the largest
// pending class — any `breaking` is a major, else any `additive` is a minor,
// else a patch. An EMPTY ledger is a patch, because a release changes more than
// the wire. That is the changesets *pattern*; the npm tool was considered and
// rejected (it would put a node toolchain in a Go repo's release path and
// cannot see these types or the floor constant).
//
// The changelog is the entries' own sentences, VERBATIM. There is deliberately
// no second write-up: the sentence in the entry was written for a reader, from
// the old client's point of view, by the person who knew — a rewrite at tag time
// is a copy that drifts, made by whoever is left.
//
// ⚠️ **This package never tags and never pushes.** It reads one tag and prints,
// the way `internal/deploychecklist` prints wrangler commands it will never run:
// cutting a release is Nick's action for the same reason deploying is. The one
// mode that writes is ConsumeLedger, and it is a step in the printed sequence
// (consume → commit → tag), so the tag lands on a tree whose CHANGELOG already
// says what shipped and whose ledger is empty.
//
// Nothing here reads a clock. The CHANGELOG carries no dates — the tag does, and
// a date written by a tool is a second answer to a question git already answers.
package nextversion

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/internal/wirechanges"
)

// ChangelogPath is where consumed sentences land, relative to the repo root.
const ChangelogPath = "CHANGELOG.md"

// Release is one derivation: what the tree is tagged at now, what the ledger
// says it should be tagged at next, and the entries that said so.
type Release struct {
	// LastTag is the tag this derivation counted from.
	LastTag string
	// Next is the derived version, `vX.Y.Z`.
	Next string
	// Entries are the pending ledger entries, sorted by slug.
	Entries []wirechanges.Entry
}

// ConsumeResult is what the writing mode did, so the caller can say it out loud
// rather than implying it by silence.
type ConsumeResult struct {
	// Removed are the entry files deleted, in the order they were consumed.
	Removed []string
	// Appended is whether CHANGELOG.md gained a section.
	Appended bool
	// Version is the heading it was written under.
	Version string
}

// classHeadings is the one place a class is described to a CHANGELOG reader,
// who is not the person who filed the entry and may hold the old client itself.
var classHeadings = map[string]string{
	wirechanges.ClassBreaking: "Breaking — an already-installed client stops working",
	wirechanges.ClassAdditive: "Additive — an already-installed client is unaffected",
	wirechanges.ClassPatch:    "Patch — nothing an already-installed client can observe changed",
}

var tagPattern = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)$`)

// FloorPrefix opens the CHANGELOG bullet of a breaking entry, and it is a
// CROSS-REPO CONTRACT, not a decoration: the private services repo parses these
// lines out of the CHANGELOG.md inside whatever tag it pins, to discover which
// floors that bump just dragged in, and pairs them against its own
// ClientVersionFloor (plan 49 step 4). It cannot import this package —
// `internal/` is per-module — so it holds its own copy of this literal and a
// guard that reads THIS LINE out of the pinned module's source to check they
// agree. Change the shape here and that guard says so; change it in both places
// and the changelog reads the same to a human either way.
const FloorPrefix = "floor "

// floorMark renders one floor as it appears in CHANGELOG.md.
func floorMark(floor string) string { return FloorPrefix + floor + " —" }

// LastReleaseTag is the nearest tag REACHABLE FROM HEAD — `git describe
// --tags --abbrev=0`, not the highest tag in the repo.
//
// The distinction is load-bearing: the derivation describes the tree about to be
// tagged, and a tag on a branch this commit never saw does not order it. It is
// also the same question `make`'s VERSION asks when it stamps the binary, so the
// number derived here and the version the build reports come from one answer.
//
// An untagged repo is an ERROR, not a v0.0.0 baseline: a tool that guesses a
// starting point prints a plausible number derived from nothing, and a plausible
// number is the one that gets tagged.
func LastReleaseTag(ctx context.Context, root string) (string, error) {
	out, err := git.Output(ctx, root, "describe", "--tags", "--abbrev=0")
	if err != nil {
		return "", fmt.Errorf("no release tag is reachable from HEAD in %s: %w\n\n"+
			"The next version is derived by bumping the last one, so there has to be a last one.\n"+
			"Tag the first release by hand (`git tag -a v0.1.0 -m v0.1.0`); every release after it\n"+
			"is derived from the ledger.", root, err)
	}
	return strings.TrimSpace(out), nil
}

// DeriveNextVersion bumps lastTag by the largest class pending in the ledger.
//
// The rule, in full: any `breaking` → major, else any `additive` → minor, else
// patch — and an empty ledger is a patch, because a release changes more than
// the wire (a CLI-only feature moves nothing an old client can see, and that is
// correct: these numbers describe the wire and what it does, not the marketing
// surface).
func DeriveNextVersion(lastTag string, entries []wirechanges.Entry) (string, error) {
	if lastTag == "" {
		return "", fmt.Errorf("no last tag to bump — the next version is derived from the one before it, written vX.Y.Z")
	}
	m := tagPattern.FindStringSubmatch(lastTag)
	if m == nil {
		return "", fmt.Errorf("the last tag %q is not a release number this tool can bump — releases are written vX.Y.Z (e.g. v0.2.0).\n\n"+
			"If a tag was cut by hand under another shape, retag it or pick the next number yourself:\n"+
			"the derivation is a suggestion, and the one after it reads whatever tag it finds.", lastTag)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])

	switch LargestClass(entries) {
	case wirechanges.ClassBreaking:
		return fmt.Sprintf("v%d.0.0", major+1), nil
	case wirechanges.ClassAdditive:
		return fmt.Sprintf("v%d.%d.0", major, minor+1), nil
	default:
		return fmt.Sprintf("v%d.%d.%d", major, minor, patch+1), nil
	}
}

// LargestClass is max(class) over the pending entries — the count never matters,
// only the worst thing that happened to somebody's installed binary. An empty
// ledger answers `patch`.
func LargestClass(entries []wirechanges.Entry) string {
	for _, class := range wirechanges.Classes {
		if len(byClass(entries, class)) > 0 {
			return class
		}
	}
	return wirechanges.ClassPatch
}

// LoadRelease reads the ledger under root and derives the next version from
// lastTag. A malformed entry comes back as the PARSER'S OWN error, unwrapped:
// it already names the file and the fix, and the person reading it has just been
// stopped by a guard they did not know existed.
func LoadRelease(ctx context.Context, root, lastTag string) (Release, error) {
	entries, err := wirechanges.LoadPending(root)
	if err != nil {
		return Release{}, err
	}
	next, err := DeriveNextVersion(lastTag, entries)
	if err != nil {
		return Release{}, err
	}
	return Release{LastTag: lastTag, Next: next, Entries: entries}, nil
}

// ChangelogBlock is the exact bytes ConsumeLedger appends to CHANGELOG.md, and
// the exact bytes RenderNextVersion shows first — one function, so the preview
// cannot be a description of what will happen instead of the thing itself.
//
// An empty ledger produces an empty block: no heading, no "no changes" line.
// A release with nothing to say says nothing, and the tag is the record.
func ChangelogBlock(version string, entries []wirechanges.Entry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n", version)
	for _, class := range wirechanges.Classes {
		group := byClass(entries, class)
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n### %s\n\n", classHeadings[class])
		for _, e := range group {
			// The floor rides the bullet it belongs to. A reader holding the
			// old binary gets the number that un-breaks them in the same line
			// that tells them they are broken — and the private services repo
			// reads the same text at pin time, because the entry that carried
			// it was consumed and this file is what is left of it.
			if e.Floor != "" {
				fmt.Fprintf(&b, "- %s %s\n", floorMark(e.Floor), e.Sentence)
				continue
			}
			fmt.Fprintf(&b, "- %s\n", e.Sentence)
		}
	}
	return b.String()
}

// ConsumeLedger is the ONE mode that writes: it appends the block to
// CHANGELOG.md under the new heading and deletes the entries it folded in.
//
// Newest first — the section lands above every older release and below the
// file's header, because a changelog is read from the top by somebody asking
// what just changed.
//
// Consuming an empty ledger writes nothing and says so, which is what makes
// running the printed sequence twice at 1am harmless: the second run finds
// nothing pending, appends no empty heading, and leaves CHANGELOG.md byte
// identical.
//
// The ledger's README is never an entry and is never deleted — the next agent
// stopped by the wire tripwire arrives at that folder to read the format.
func ConsumeLedger(root string, rel Release) (ConsumeResult, error) {
	result := ConsumeResult{Version: rel.Next}
	if len(rel.Entries) == 0 {
		return result, nil
	}

	block := ChangelogBlock(rel.Next, rel.Entries)
	path := filepath.Join(root, ChangelogPath)
	existing, err := os.ReadFile(path)
	if err != nil {
		return result, fmt.Errorf("%s: %w\n\nThe consumed sentences have nowhere to land. Create it with a header and re-run.", ChangelogPath, err)
	}
	if err := os.WriteFile(path, []byte(insertRelease(string(existing), block)), 0o644); err != nil {
		return result, err
	}
	result.Appended = true

	for _, e := range rel.Entries {
		if err := os.Remove(filepath.Join(root, wirechanges.Dir, e.Slug+".md")); err != nil {
			return result, fmt.Errorf("%s is in CHANGELOG.md but could not be deleted: %w\n\n"+
				"Delete it by hand before committing — an entry that survives its own release is folded in twice.", e.File, err)
		}
		result.Removed = append(result.Removed, e.File)
	}
	return result, nil
}

// insertRelease puts block above the first `## ` heading, or at the end of a
// changelog that has none yet.
func insertRelease(existing, block string) string {
	head, rest := existing, ""
	switch {
	case strings.HasPrefix(existing, "## "):
		head, rest = "", existing
	default:
		if at := strings.Index(existing, "\n## "); at >= 0 {
			head, rest = existing[:at], existing[at+1:]
		}
	}
	out := block
	if head = strings.TrimRight(head, "\n"); head != "" {
		out = head + "\n\n" + block
	}
	if rest = strings.TrimLeft(rest, "\n"); rest != "" {
		out += "\n" + rest
	}
	return out
}

func byClass(entries []wirechanges.Entry, class string) []wirechanges.Entry {
	var out []wirechanges.Entry
	for _, e := range entries {
		if e.Class == class {
			out = append(out, e)
		}
	}
	return out
}
