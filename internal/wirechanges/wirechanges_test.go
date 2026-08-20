package wirechanges_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nwylynko/slopball-cli/internal/wirechanges"
)

const repoRoot = "../.."

// THE GUARD THAT MATTERS (plan 48 step 1): the tree this repo would tag has a
// well-formed ledger. It reads the real `.wire-changes/`, because a fixture
// cannot fail on the commit that actually gets an entry wrong — and `make
// next-version` failing at tag time, on the tree already committed, is the
// failure this moves an hour earlier.
//
// The OTHER half of the old guard — pairing a breaking floor against the
// deployed control plane's ClientVersionFloor — is not here and cannot be: that
// constant is service code in the private repo (plan 49 step 4). It runs there,
// against this repo's CHANGELOG.md, when that repo bumps its pin.
func TestPendingWireChangesAreWellFormed(t *testing.T) {
	if _, err := wirechanges.LoadPending(repoRoot); err != nil {
		t.Fatalf("the pending wire-change ledger is not readable:\n\n%v", err)
	}
}

func TestAWellFormedEntryParses(t *testing.T) {
	for _, tc := range []struct {
		name, file, body string
		want             wirechanges.Entry
	}{
		{
			name: "additive",
			file: "member-version-field.md",
			body: "class: additive\n\nAn old client keeps working: the control plane accepts a member row that carries no version.\n",
			want: wirechanges.Entry{
				File:     ".wire-changes/member-version-field.md",
				Slug:     "member-version-field",
				Class:    wirechanges.ClassAdditive,
				Sentence: "An old client keeps working: the control plane accepts a member row that carries no version.",
			},
		},
		{
			name: "breaking carries its floor",
			file: "drop-overlay-addr.md",
			body: "class: breaking\nfloor: v1.4.0\n\nAn old client is refused: the control plane no longer reads overlayAddr from a claim.\n",
			want: wirechanges.Entry{
				File:     ".wire-changes/drop-overlay-addr.md",
				Slug:     "drop-overlay-addr",
				Class:    wirechanges.ClassBreaking,
				Floor:    "v1.4.0",
				Sentence: "An old client is refused: the control plane no longer reads overlayAddr from a claim.",
			},
		},
		{
			name: "patch",
			file: "rename-internal-field.md",
			body: "class: patch\n\nAn old client sees no difference: the field it never decoded was renamed.\n",
			want: wirechanges.Entry{
				File:     ".wire-changes/rename-internal-field.md",
				Slug:     "rename-internal-field",
				Class:    wirechanges.ClassPatch,
				Sentence: "An old client sees no difference: the field it never decoded was renamed.",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := wirechanges.ParseEntry(tc.file, tc.body)
			if err != nil {
				t.Fatalf("well-formed entry rejected: %v", err)
			}
			if got != tc.want {
				t.Fatalf("parsed %#v, want %#v", got, tc.want)
			}
		})
	}
}

// A malformed entry fails NAMING THE FILE AND THE FIX — the same
// red-guides-the-agent rule as the tripwire. "invalid entry" would send
// somebody to read this package's source, which is exactly the cost the message
// exists to remove.
func TestAMalformedEntryNamesTheFileAndTheFix(t *testing.T) {
	for _, tc := range []struct {
		name, file, body string
		wants            []string
	}{
		{
			name:  "no class line",
			file:  "no-class.md",
			body:  "An old client keeps working.\n",
			wants: []string{".wire-changes/no-class.md", "class:", "breaking | additive | patch"},
		},
		{
			name:  "unknown class",
			file:  "feature-class.md",
			body:  "class: feature\n\nWe added a thing.\n",
			wants: []string{".wire-changes/feature-class.md", `"feature"`, "breaking | additive | patch", "already-installed client stops working"},
		},
		{
			name:  "breaking with no floor",
			file:  "drop-a-field.md",
			body:  "class: breaking\n\nAn old client is refused.\n",
			wants: []string{".wire-changes/drop-a-field.md", "floor: vX.Y.Z", "OLDEST client", "ClientVersionFloor", "private services repo", "CHANGELOG.md"},
		},
		{
			name:  "no sentence",
			file:  "silent.md",
			body:  "class: additive\n",
			wants: []string{".wire-changes/silent.md", "ONE sentence", "old client's point of view", "CHANGELOG.md"},
		},
		{
			name:  "floor on an additive entry",
			file:  "additive-with-floor.md",
			body:  "class: additive\nfloor: v1.4.0\n\nAn old client keeps working.\n",
			wants: []string{".wire-changes/additive-with-floor.md", "only a breaking change moves the floor"},
		},
		{
			name:  "floor is not a release tag",
			file:  "bad-floor.md",
			body:  "class: breaking\nfloor: 1.4\n\nAn old client is refused.\n",
			wants: []string{".wire-changes/bad-floor.md", "vX.Y.Z"},
		},
		{
			name:  "unknown header",
			file:  "extra-header.md",
			body:  "class: additive\nowner: nick\n\nAn old client keeps working.\n",
			wants: []string{".wire-changes/extra-header.md", `"owner"`},
		},
		{
			name:  "not a slug",
			file:  "Member_Version.md",
			body:  "class: additive\n\nAn old client keeps working.\n",
			wants: []string{"lowercase-words-with-dashes"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := wirechanges.ParseEntry(tc.file, tc.body)
			if err == nil {
				t.Fatalf("%s parsed clean — a malformed entry has to stop the tree", tc.file)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not say %q:\n%v", want, err)
				}
			}
			if !strings.Contains(err.Error(), wirechanges.FormatDoc) {
				t.Errorf("the error does not point at %s:\n%v", wirechanges.FormatDoc, err)
			}
		})
	}
}

// A breaking entry with no floor is refused, against fixtures — including the
// case this repo ships today, where nothing is pending at all. The floor is the
// only field whose absence is silent: an entry without one parses as prose, is
// consumed into CHANGELOG.md, and leaves the services repo with nothing to pin
// its ClientVersionFloor against, so the deployment garbles exactly the clients
// the entry said were refused.
func TestABreakingEntryWithoutAFloorIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entry   wirechanges.Entry
		wantErr bool
	}{
		{"breaking with a floor", wirechanges.Entry{File: ".wire-changes/a.md", Class: wirechanges.ClassBreaking, Floor: "v1.4.0", Sentence: "An old client is refused."}, false},
		{"breaking with no floor", wirechanges.Entry{File: ".wire-changes/a.md", Class: wirechanges.ClassBreaking, Sentence: "An old client is refused."}, true},
		{"breaking with a floor that is not a tag", wirechanges.Entry{File: ".wire-changes/a.md", Class: wirechanges.ClassBreaking, Floor: "1.4", Sentence: "An old client is refused."}, true},
		{"additive carrying a floor it cannot mean", wirechanges.Entry{File: ".wire-changes/a.md", Class: wirechanges.ClassAdditive, Floor: "v1.4.0", Sentence: "An old client keeps working."}, true},
		{"the shipped state: additive, no floor", wirechanges.Entry{File: ".wire-changes/a.md", Class: wirechanges.ClassAdditive, Sentence: "An old client keeps working."}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.entry.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("%+v passed validation", tc.entry)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("%+v refused: %v", tc.entry, err)
			}
		})
	}
}

// Breaking() is what the services repo's pin-time check counts, so it must pick
// out exactly the entries that named a floor and nothing else.
func TestBreakingSelectsOnlyTheEntriesThatNamedAFloor(t *testing.T) {
	entries := []wirechanges.Entry{
		{Slug: "add", Class: wirechanges.ClassAdditive, Sentence: "keeps working"},
		{Slug: "drop", Class: wirechanges.ClassBreaking, Floor: "v1.4.0", Sentence: "refused"},
		{Slug: "move", Class: wirechanges.ClassPatch, Sentence: "no difference"},
	}
	got := wirechanges.Breaking(entries)
	if len(got) != 1 || got[0].Slug != "drop" || got[0].Floor != "v1.4.0" {
		t.Fatalf("Breaking() returned %+v, want just the drop entry with its floor", got)
	}
}

// THE POINTER RESOLVES (plan 48 step 6). Three places hand an agent the same
// playbook mid-change — `wirechanges.Playbook`, the snapshot tripwire's drift
// text, and `.wire-changes/README.md` — and all three name ONE section by its
// title in ONE file. A title that drifts by a word makes every one of them
// dangle, and a pointer to a section nobody can find reads like a document that
// was deleted: the agent concludes the guidance is gone and ships the release
// the playbook exists to prevent. So the pointer is resolved here the way the
// agent will resolve it — open the file, look for the heading — and the title
// is read out of the pointer itself rather than retyped, because a copy in this
// test could drift with the pointer and still pass.
func TestThePlaybookPointerResolvesToARealSection(t *testing.T) {
	const page = wirechanges.FormatDoc
	if !strings.Contains(wirechanges.Playbook, page) {
		t.Fatalf("wirechanges.Playbook no longer says where the playbook lives:\n%s", wirechanges.Playbook)
	}
	title := betweenQuotes(t, wirechanges.Playbook)

	body, err := os.ReadFile(filepath.Join(repoRoot, page))
	if err != nil {
		t.Fatalf("%s is missing: %v", page, err)
	}
	doc := string(body)
	if !strings.Contains(doc, "## "+title) {
		t.Fatalf("%s has no section titled %q — every pointer at the expand/contract playbook dangles.\n"+
			"The heading must match the pointer text exactly.", page, title)
	}

	// The playbook without these is a heading with the wrong document under it:
	// each one is a decision the guards cannot teach and the agent needs at the
	// moment it lands here.
	for _, want := range []struct{ why, needle string }{
		{"names the two-release dance", "EXPAND"},
		{"names the second release", "CONTRACT"},
		{"says the floor names an already-tagged release", "already-tagged"},
		{"gives the shim comment convention verbatim", "accept-both until the floor reaches"},
		{"names the constant the floor is finally enforced by", "ClientVersionFloor"},
		{"says which repo enforces it", "private services repo"},
		{"says the deploy half lives with the deployment", "docs/security.md"},
	} {
		if !strings.Contains(doc, want.needle) {
			t.Errorf("the playbook in %s no longer %s (looked for %q)", page, want.why, want.needle)
		}
	}
}

// betweenQuotes lifts the section title out of a pointer string, so the pointer
// and the assertion cannot disagree.
func betweenQuotes(t *testing.T, s string) string {
	t.Helper()
	open := strings.Index(s, `"`)
	close := strings.Index(s[open+1:], `"`)
	if open < 0 || close < 0 {
		t.Fatalf("the playbook pointer names no section title in quotes:\n%s", s)
	}
	return s[open+1 : open+1+close]
}

// The ledger folder is where every message says it is, and the README that the
// messages point at exists. A pointer to a file nobody wrote is worse than no
// pointer: it reads like a document that has been deleted.
func TestTheLedgerFolderAndItsReadmeExist(t *testing.T) {
	if _, err := os.Stat(filepath.Join(repoRoot, wirechanges.Dir)); err != nil {
		t.Fatalf("%s is missing: %v", wirechanges.Dir, err)
	}
	readme, err := os.ReadFile(filepath.Join(repoRoot, wirechanges.FormatDoc))
	if err != nil {
		t.Fatalf("%s is missing: %v", wirechanges.FormatDoc, err)
	}
	page := string(readme)
	for _, want := range []struct{ why, needle string }{
		{"names the three classes", "breaking"},
		{"names the additive class", "additive"},
		{"names the patch class", "patch"},
		{"states the old-client's-POV rule", "old client"},
		{"names the floor key", "floor:"},
		{"names the floor constant", "ClientVersionFloor"},
		{"points at the playbook by its heading", "Expand/contract: shipping a wire change"},
		{"names the regeneration command", "make wire-snapshot"},
		{"admits the shape-only limitation", "shape"},
		{"says who consumes entries", "make next-version"},
		{"says where the sentences end up", "CHANGELOG.md"},
	} {
		if !strings.Contains(page, want.needle) {
			t.Errorf("%s no longer %s (looked for %q)", wirechanges.FormatDoc, want.why, want.needle)
		}
	}
}
