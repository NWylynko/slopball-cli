package nextversion_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/internal/nextversion"
	"github.com/nwylynko/slopball-cli/internal/wirechanges"
)

const repoRoot = "../.."

// THE PIN (plan 48 step 7 acceptance): a fixture ledger derives one version and
// one changelog, BYTE FOR BYTE. Not "contains v1.0.0" — the whole printed page,
// because the page is the deliverable: it is what Nick reads at tag time and
// what he pastes, and a derivation that is right in a field and wrong in a
// sentence is a release cut on a misreading.
//
// The fixture holds one entry of every class plus the ledger's own README (which
// is never an entry), so the grouping, the ordering and the max(class) rule are
// all pinned by the same bytes.
func TestAFixtureLedgerDerivesThePinnedVersionAndPageByteForByte(t *testing.T) {
	rel := loadFixture(t, "ledger-mixed", "v0.1.0")

	if rel.Next != "v1.0.0" {
		t.Fatalf("derived %q from a ledger holding a breaking entry, want v1.0.0", rel.Next)
	}
	var out bytes.Buffer
	if err := nextversion.RenderNextVersion(&out, rel); err != nil {
		t.Fatalf("render: %v", err)
	}
	assertGolden(t, "mixed-print.txt", out.String())
}

// An empty ledger is a PATCH, and the page says why in the same breath — the
// rule that is least obvious to whoever reads this at tag time is the one it has
// to explain: releases change more than the wire.
func TestAnEmptyLedgerDerivesAPatchAndSaysWhy(t *testing.T) {
	rel := loadFixture(t, "ledger-empty", "v0.1.0")

	if len(rel.Entries) != 0 {
		t.Fatalf("the empty fixture loaded %d entries — its README is not an entry", len(rel.Entries))
	}
	if rel.Next != "v0.1.1" {
		t.Fatalf("derived %q from an empty ledger, want v0.1.1", rel.Next)
	}
	var out bytes.Buffer
	if err := nextversion.RenderNextVersion(&out, rel); err != nil {
		t.Fatalf("render: %v", err)
	}
	assertGolden(t, "empty-print.txt", out.String())
}

// The bump rule itself, against every shape of ledger — max(class), never the
// count, and never the order the entries were filed in.
func TestTheBumpIsMaxClassOverThePendingEntries(t *testing.T) {
	breaking := wirechanges.Entry{File: ".wire-changes/b.md", Slug: "b", Class: wirechanges.ClassBreaking, Floor: "v1.4.0", Sentence: "An old client is refused."}
	additive := wirechanges.Entry{File: ".wire-changes/a.md", Slug: "a", Class: wirechanges.ClassAdditive, Sentence: "An old client keeps working."}
	patch := wirechanges.Entry{File: ".wire-changes/p.md", Slug: "p", Class: wirechanges.ClassPatch, Sentence: "An old client sees no difference."}

	for _, tc := range []struct {
		name    string
		lastTag string
		entries []wirechanges.Entry
		want    string
	}{
		{"empty ledger is a patch", "v0.1.0", nil, "v0.1.1"},
		{"patch only", "v0.1.0", []wirechanges.Entry{patch, patch}, "v0.1.1"},
		{"additive beats patch", "v0.1.0", []wirechanges.Entry{patch, additive}, "v0.2.0"},
		{"breaking beats everything", "v0.1.0", []wirechanges.Entry{patch, additive, breaking}, "v1.0.0"},
		{"a minor resets the patch", "v0.4.7", []wirechanges.Entry{additive}, "v0.5.0"},
		{"a major resets both", "v0.4.7", []wirechanges.Entry{breaking}, "v1.0.0"},
		{"past 1.0 the major still just moves", "v1.9.9", []wirechanges.Entry{breaking}, "v2.0.0"},
		// A human overrode the suggestion for a reason the wire cannot see. The
		// tool tolerates it silently: the next derivation reads whatever tag it
		// finds, and carries on from there.
		{"a marketing jump is just the last tag", "v3.0.0", []wirechanges.Entry{additive}, "v3.1.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nextversion.DeriveNextVersion(tc.lastTag, tc.entries)
			if err != nil {
				t.Fatalf("derive: %v", err)
			}
			if got != tc.want {
				t.Fatalf("derived %q from %s, want %q", got, tc.lastTag, tc.want)
			}
		})
	}
}

// A tag the derivation cannot bump stops the tool NAMING the tag and the fix.
// Guessing (treating a garbled tag as v0.0.0) would print a plausible number
// derived from nothing, and a plausible number is the one that gets tagged.
func TestAnUnbumpableLastTagFailsNamingTheTag(t *testing.T) {
	for _, tc := range []struct{ name, lastTag string }{
		{"no tag at all", ""},
		{"not a semver", "release-3"},
		{"two components", "v1.4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := nextversion.DeriveNextVersion(tc.lastTag, nil)
			if err == nil {
				t.Fatalf("%q derived a version — a tag the tool cannot read has to stop it", tc.lastTag)
			}
			if !strings.Contains(err.Error(), "vX.Y.Z") {
				t.Errorf("the error does not say what a tag looks like:\n%v", err)
			}
			if tc.lastTag != "" && !strings.Contains(err.Error(), tc.lastTag) {
				t.Errorf("the error does not name the tag %q:\n%v", tc.lastTag, err)
			}
		})
	}
}

// CONSUMPTION: the entries are deleted, their sentences land in CHANGELOG.md
// VERBATIM under the new heading, and the README survives. Pinned byte for byte
// against a CHANGELOG that already has an older release in it, because "append"
// and "newest first" are different files and only one of them is readable.
func TestConsumingEmptiesTheLedgerAndAppendsTheSentencesVerbatim(t *testing.T) {
	tree := copyFixture(t, "ledger-mixed")
	rel := loadTree(t, tree, "v0.1.0")

	result, err := nextversion.ConsumeLedger(tree, rel)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(result.Removed) != 3 {
		t.Fatalf("consumed %d entries, want 3: %v", len(result.Removed), result.Removed)
	}
	if !result.Appended {
		t.Fatalf("consume reported nothing appended with three entries pending")
	}

	got, err := os.ReadFile(filepath.Join(tree, nextversion.ChangelogPath))
	if err != nil {
		t.Fatalf("CHANGELOG.md: %v", err)
	}
	assertGolden(t, "mixed-changelog-after.md", string(got))

	// The ledger is empty, and empty means "no entries" — not "no folder" and
	// not "no README": the next agent stopped by the tripwire arrives here to
	// read the format.
	left, err := wirechanges.LoadPending(tree)
	if err != nil {
		t.Fatalf("ledger after consumption: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("%d entries survived consumption: %v", len(left), left)
	}
	if _, err := os.Stat(filepath.Join(tree, wirechanges.Dir, "README.md")); err != nil {
		t.Fatalf("consumption deleted the ledger's README: %v", err)
	}

	// Run it again on the emptied tree: nothing pending, so nothing appended
	// and CHANGELOG.md is untouched. Running the sequence twice is a thing that
	// happens at 1am, and the second run must not write a second empty heading.
	again := loadTree(t, tree, "v0.1.0")
	result, err = nextversion.ConsumeLedger(tree, again)
	if err != nil {
		t.Fatalf("second consume: %v", err)
	}
	if result.Appended || len(result.Removed) != 0 {
		t.Fatalf("a second consume wrote something: %+v", result)
	}
	after, err := os.ReadFile(filepath.Join(tree, nextversion.ChangelogPath))
	if err != nil {
		t.Fatalf("CHANGELOG.md: %v", err)
	}
	if string(after) != string(got) {
		t.Fatalf("a second consume changed CHANGELOG.md:\n%s", string(after))
	}
}

// A CHANGELOG that is nothing but releases (no header) still gets the new one on
// top, with no blank line above it. The header is a convention, not a parser
// requirement — trimming it should not produce a file that starts with padding.
func TestConsumingIntoAHeaderlessChangelogPutsTheReleaseOnTop(t *testing.T) {
	tree := copyFixture(t, "ledger-additive-only")
	if err := os.WriteFile(filepath.Join(tree, nextversion.ChangelogPath), []byte("## v0.1.0\n\n- The first tag.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel := loadTree(t, tree, "v0.1.0")
	if _, err := nextversion.ConsumeLedger(tree, rel); err != nil {
		t.Fatalf("consume: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(tree, nextversion.ChangelogPath))
	if err != nil {
		t.Fatal(err)
	}
	want := nextversion.ChangelogBlock("v0.2.0", rel.Entries) + "\n## v0.1.0\n\n- The first tag.\n"
	if string(got) != want {
		t.Fatalf("headerless CHANGELOG came back as:\n%q\n\nwant:\n%q", string(got), want)
	}
}

// PRINT MODE WRITES NOTHING. The whole shape of this tool — print, never act —
// is one filesystem assertion away from being a claim in a comment.
func TestPrintModeWritesNothing(t *testing.T) {
	tree := copyFixture(t, "ledger-mixed")
	before := snapshotTree(t, tree)

	rel := loadTree(t, tree, "v0.1.0")
	var out bytes.Buffer
	if err := nextversion.RenderNextVersion(&out, rel); err != nil {
		t.Fatalf("render: %v", err)
	}
	if after := snapshotTree(t, tree); after != before {
		t.Fatalf("deriving the next version changed the tree:\n%s\n\nwant:\n%s", after, before)
	}
}

// A malformed entry stops the tool with the PARSER'S OWN sentence — the file and
// the fix, not "load release: 1 error". The agent reading it has just been
// stopped by a guard they did not know existed, and a wrapped error sends them
// to read this package's source instead of the entry they got wrong.
func TestAMalformedEntryFailsNamingTheFileAndTheFix(t *testing.T) {
	dir := filepath.Join("testdata", "ledger-malformed")
	_, err := nextversion.LoadRelease(context.Background(), dir, "v0.1.0")
	if err == nil {
		t.Fatal("a ledger holding class: feature loaded clean")
	}
	for _, want := range []string{
		".wire-changes/feature-class.md",
		`"feature"`,
		"breaking | additive | patch",
		wirechanges.FormatDoc,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}
}

// The last tag is the nearest one REACHABLE FROM HEAD, not the highest tag on
// the repo: the derivation describes the tree being released, and a tag on a
// branch this commit never saw does not order it. Run against a real repo built
// with bundled git, because that is the only way to know which of the two
// `git describe` answers this tool is actually getting.
func TestLastReleaseTagReadsTheNearestTagReachableFromHead(t *testing.T) {
	requireBundledGit(t)
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		id := git.SessionIdentity("tester", "000000")
		cmd := &git.Cmd{Dir: repo, Env: id.EnvVars()}
		if err := cmd.Run(ctx, args...); err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
	}
	commit := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", "-A")
		run("commit", "-m", name)
	}

	run("init")
	commit("one")

	// No tag yet: the tool must say so rather than invent a baseline.
	if _, err := nextversion.LastReleaseTag(ctx, repo); err == nil {
		t.Fatal("an untagged repo produced a last tag")
	} else if !strings.Contains(err.Error(), "git tag") {
		t.Errorf("the untagged error does not name the fix:\n%v", err)
	}

	run("tag", "-a", "v0.1.0", "-m", "v0.1.0")
	commit("two")
	commit("three")

	got, err := nextversion.LastReleaseTag(ctx, repo)
	if err != nil {
		t.Fatalf("last tag: %v", err)
	}
	if got != "v0.1.0" {
		t.Fatalf("last tag %q, want v0.1.0 — describe --abbrev=0 should drop the distance suffix", got)
	}

	// A tag on a branch HEAD cannot see is not this tree's last release.
	run("checkout", "-b", "sidebranch")
	commit("four")
	run("tag", "-a", "v9.9.9", "-m", "v9.9.9")
	run("checkout", "main")
	got, err = nextversion.LastReleaseTag(ctx, repo)
	if err != nil {
		t.Fatalf("last tag: %v", err)
	}
	if got != "v0.1.0" {
		t.Fatalf("last tag %q — an unreachable tag ordered this tree", got)
	}
}

// THIS REPO'S OWN LEDGER IS DERIVABLE, always. It is deliberately not pinned to
// a number: the real ledger is consumed at the real next tag, and a test that
// pinned v0.2.0 would go red on the release it exists to serve. What must never
// happen is `make next-version` failing on the tree it is about to tag, and that
// is what this asserts — every pending entry parses, and the derivation lands on
// a version above the tag it came from.
func TestTheRealLedgerAlwaysDerivesARelease(t *testing.T) {
	entries, err := wirechanges.LoadPending(repoRoot)
	if err != nil {
		t.Fatalf("this repo's pending ledger does not parse — `make next-version` would fail on it:\n\n%v", err)
	}
	next, err := nextversion.DeriveNextVersion("v0.1.0", entries)
	if err != nil {
		t.Fatalf("derive from this repo's ledger: %v", err)
	}
	if next == "v0.1.0" || !strings.HasPrefix(next, "v") {
		t.Fatalf("derived %q from this repo's ledger — a release always moves the number", next)
	}
}

// The ledger this repo holds TODAY, copied into testdata on 2026-08-11, derives
// v0.2.0 from v0.1.0 — the number plan 48's own `Member` field earns. Pinned
// against the copy rather than the folder so it keeps saying something true
// after the real entry is consumed.
func TestTheLedgerAsItStandsTodayDerivesAMinor(t *testing.T) {
	rel := loadFixture(t, "ledger-additive-only", "v0.1.0")
	if len(rel.Entries) != 1 || rel.Entries[0].Class != wirechanges.ClassAdditive {
		t.Fatalf("the snapshot fixture is not one additive entry: %+v", rel.Entries)
	}
	if rel.Next != "v0.2.0" {
		t.Fatalf("derived %q from one additive entry against v0.1.0, want v0.2.0", rel.Next)
	}
}

// The tool never tags and never pushes, and that is checked in the source rather
// than trusted: every printed `git tag` line is a string for a human, so a
// future edit that turns one into a call would read almost identically in review.
// Bundled git is the only git this repo may run (git.TestNoBareGitShellouts), so
// the check is on git.Run/git.Cmd.Run, which are the only ways to run one.
func TestTheDerivationRunsNoWritingGitCommand(t *testing.T) {
	for _, name := range []string{"nextversion.go", "render.go", "../../cmd/slopnextversion/main.go"} {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		src := string(body)
		for _, forbidden := range []string{"git.Run(", "cmd.Run(", ".Run(ctx"} {
			if strings.Contains(src, forbidden) {
				t.Errorf("%s runs a git command through %s — this tool reads one tag and prints; tagging and pushing are Nick's", name, forbidden)
			}
		}
	}
}

// The make targets exist and run this tool. The plan's deliverable is
// `make next-version`, not a package — and a target nobody wired is a tool
// nobody finds.
func TestTheMakeTargetsAreWired(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	mk := string(body)
	for _, want := range []string{
		"next-version:",
		"next-version-consume:",
		"./cmd/slopnextversion",
	} {
		if !strings.Contains(mk, want) {
			t.Errorf("the Makefile has no %q — `make next-version` is the deliverable", want)
		}
	}
}

// The CHANGELOG the consume step writes into exists, and says what it is. Every
// error and README in this machinery points at it by name.
func TestTheChangelogExists(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot, nextversion.ChangelogPath))
	if err != nil {
		t.Fatalf("%s is missing — the consume step appends into it: %v", nextversion.ChangelogPath, err)
	}
	page := string(body)
	for _, want := range []struct{ why, needle string }{
		{"names where its sentences come from", ".wire-changes"},
		{"names the tool that writes it", "make next-version"},
		{"states the verbatim rule", "verbatim"},
	} {
		if !strings.Contains(page, want.needle) {
			t.Errorf("%s no longer %s (looked for %q)", nextversion.ChangelogPath, want.why, want.needle)
		}
	}
}

func loadFixture(t *testing.T, name, lastTag string) nextversion.Release {
	t.Helper()
	return loadTree(t, filepath.Join("testdata", name), lastTag)
}

func loadTree(t *testing.T, root, lastTag string) nextversion.Release {
	t.Helper()
	rel, err := nextversion.LoadRelease(context.Background(), root, lastTag)
	if err != nil {
		t.Fatalf("loading %s: %v", root, err)
	}
	return rel
}

func copyFixture(t *testing.T, name string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "tree")
	if err := os.CopyFS(dst, os.DirFS(filepath.Join("testdata", name))); err != nil {
		t.Fatalf("copying fixture %s: %v", name, err)
	}
	return dst
}

// snapshotTree renders every file's path and contents, so "wrote nothing" is
// checked as the tree being identical rather than as mtimes being close.
func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		b.WriteString(rel + "\n" + string(body) + "\n---\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden %s: %v", path, err)
	}
	if got == string(want) {
		return
	}
	t.Fatalf("%s does not match byte for byte.\n\n--- got ---\n%s\n--- want ---\n%s\n--- first difference ---\n%s",
		path, got, string(want), firstDifference(got, string(want)))
}

func firstDifference(got, want string) string {
	g := strings.Split(got, "\n")
	w := strings.Split(want, "\n")
	for i := 0; i < len(g) || i < len(w); i++ {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			return "line " + itoa(i+1) + ":\n  got  " + gl + "\n  want " + wl
		}
	}
	return "(no line differs — trailing bytes)"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

// THE CROSS-REPO HANDOFF (plan 49 step 4). A breaking entry's `floor:` is the
// oldest client that still works, and what finally ENFORCES it is
// service.ClientVersionFloor — a constant in the private services repo, which
// bumps its `require github.com/nwylynko/slopball-cli` pin and then has to
// discover which floors that pin dragged in. The pending ledger cannot tell it:
// entries are consumed at tag time, so by the time a version is pinnable its
// entries are gone.
//
// CHANGELOG.md is what survives, so the floor is printed INTO it — verbatim,
// next to the sentence it belongs to. Drop the floor from the rendering and the
// pin-time pairing has nothing to read: the services repo bumps its pin, the
// deployment accepts clients the changelog says are refused, and they fail
// mid-session on a garbled decode instead of on a sentence.
func TestTheChangelogPrintsTheFloorABreakingEntryNamed(t *testing.T) {
	rel := loadFixture(t, "ledger-mixed", "v0.1.0")
	block := nextversion.ChangelogBlock(rel.Next, rel.Entries)

	if !strings.Contains(block, "floor v1.4.0") {
		t.Fatalf("the changelog block does not carry the breaking entry's floor:\n%s", block)
	}
	// On the breaking bullet and nowhere else — an additive entry has no floor
	// to name, and a line that invented one would be pinned against.
	for _, line := range strings.Split(block, "\n") {
		if strings.Contains(line, "floor ") && !strings.Contains(line, "no longer reads overlayAddr") {
			t.Errorf("a non-breaking line carries a floor: %q", line)
		}
	}
}

// requireBundledGit resolves the bundled git binary or fails naming the fix.
// Inlined rather than imported: the monorepo's internal/git/gittest is across
// the repo line, and a public test may import nothing outside this module (plan
// 49 step 4). An absent binary is a FAILURE and never a skip — `make test` here
// depends on `fetch-git`, so absent means the fetch silently failed.
func requireBundledGit(t *testing.T) {
	t.Helper()
	if _, err := git.Bin(); err != nil {
		t.Fatalf("%v\nrun `make fetch-git`", err)
	}
}
