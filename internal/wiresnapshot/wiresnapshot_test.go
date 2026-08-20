package wiresnapshot_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/nwylynko/slopball-cli/internal/wirechanges"
	"github.com/nwylynko/slopball-cli/internal/wiresnapshot"
)

const repoRoot = "../.."

// The tripwire (plan 48 step 1).
//
// A wire change that nobody classified must not get past the build. The
// guidance lives HERE, in the failure text of a red test, rather than in a doc
// — the `whatisrecorded_test.go` pattern — because this is the one moment an
// agent is standing in front of the decision, and a doc it has not read cannot
// reach it. The message below is the deliverable: it has to be enough on its
// own, for somebody who has read nothing else.
func TestWireSurfaceMatchesTheCommittedSnapshot(t *testing.T) {
	current, err := wiresnapshot.GenerateWireSurface(repoRoot)
	if err != nil {
		t.Fatalf("generating the wire surface: %v", err)
	}
	committed, err := wiresnapshot.LoadWireSurface(repoRoot)
	if err != nil {
		t.Fatalf("reading %s: %v\n\nIf it is missing, run `%s`.", wiresnapshot.SnapshotPath, err, wiresnapshot.RegenerateCommand)
	}
	if current == committed {
		return
	}
	t.Fatalf("%s\n\n%s", wiresnapshot.WireSurfaceDiff(committed, current), driftInstructions())
}

// driftInstructions is the contract, written to be acted on cold.
func driftInstructions() string {
	return strings.Join([]string{
		"THE WIRE SURFACE MOVED AND NOTHING CLASSIFIES THE CHANGE.",
		"",
		"Above is what " + wiresnapshot.SnapshotPath + " says (-) against what this tree says (+).",
		"Those shapes are what an already-installed slopball on somebody else's laptop can see:",
		"the control-plane HTTP types and vocabulary, the session-network framing, the telemetry",
		"envelope, and the relay ticket. Do all three of these, in this order:",
		"",
		"1. FILE THE ENTRY. Create `.wire-changes/<slug>.md` (lowercase-words-with-dashes):",
		"",
		"       class: additive",
		"",
		"       An old client keeps working: <one sentence, from an OLD CLIENT'S point of view>.",
		"",
		"   `class:` is exactly one of, judged against a client installed BEFORE this change",
		"   that is never rebuilt:",
		"",
		"       breaking   that client stops working against this build",
		"       additive   new field / route / constant; that client is unaffected",
		"       patch      the shape moved, nothing that client can observe changed",
		"",
		"   The sentence is written from THAT client's point of view, not yours — \"an old client",
		"   still…\" / \"an old client no longer…\". It is copied verbatim into CHANGELOG.md when",
		"   the next tag consumes the ledger, so write it for a reader.",
		"",
		"2. IF IT IS BREAKING, PAIR IT WITH THE FLOOR. The entry also carries `floor: vX.Y.Z` —",
		"   the oldest client that still works after the change. A guard here fails a breaking",
		"   entry that names none (go test ./internal/wirechanges/).",
		"",
		"   THE OTHER HALF OF THAT PAIR IS ACROSS THE REPO LINE:",
		"   " + wirechanges.FloorConstantRef + ".",
		"   That constant is the DEPLOYED control plane's property, so it moves there, and the",
		"   guard that pairs it against your floor runs there too — when that repo bumps its",
		"   `require github.com/nwylynko/slopball-cli` pin onto the release this entry ships in.",
		"   So a breaking entry here is not deployable on its own, and that is the point: the",
		"   floor is what turns a garbled decode on somebody's laptop into a clear refusal.",
		"",
		"3. REGENERATE THIS SNAPSHOT and commit it with the change and the entry:",
		"",
		"       " + wiresnapshot.RegenerateCommand,
		"",
		"BEFORE YOU WRITE ANY OF IT, read " + wirechanges.Playbook + ".",
		"A breaking change ships in TWO releases — expand (server accepts both shapes, client",
		"sends the new one, `additive` entry), wait for the drain check, then contract (drop the",
		"old shape, `breaking` entry, floor bump). Replacing a shape in place passes every guard",
		"in this repo and still produces a release no live session can upgrade into.",
		"",
		"Entry format, the lifecycle, and what this tripwire cannot see: " + wirechanges.FormatDoc + ".",
	}, "\n")
}

// The scan being broken must not read as a clean surface. Without this, a
// generator that silently stopped finding types would be "fixed" by
// regenerating an empty snapshot, and every wire would go unwatched with the
// suite green — the failure mode `whatisrecorded_test.go` guards the same way.
func TestTheWireScanActuallyFindsEveryWireThisModuleOwns(t *testing.T) {
	surface, err := wiresnapshot.GenerateWireSurface(repoRoot)
	if err != nil {
		t.Fatalf("generating the wire surface: %v", err)
	}
	for _, want := range []struct{ why, needle string }{
		{"the control-plane session document", "type Session struct"},
		{"the member row every cycle upserts", "type Member struct"},
		{"the one uplink per member cycle", "type MemberSync struct"},
		{"the member-id header", `const MemberHeader = "X-Slopball-Member"`},
		{"the session-network record ceiling", "const maxRecord = 16 * 1024"},
		{"the handshake's domain separator", `const handshakeInfo = "slopball-sessionnet-v1"`},
		{"the relay subprotocol", `const Proto = "slopball-relay/1"`},
		{"the record layer's length prefix", "hdr [2]byte"},
		{"the telemetry envelope", "type Envelope struct"},
		{"the relay ticket claims", "type Claims struct"},
		{"the ticket lifetime three parties cache against", "const TicketTTL = time.Hour"},
	} {
		if !strings.Contains(surface, want.needle) {
			t.Errorf("the wire surface does not name %s (looked for %q) — the SCAN is broken, not the wire", want.why, want.needle)
		}
	}

	// And the half that must NOT be here. The route table is service code; it
	// stayed in the private repo with the mux that dispatches it, and a
	// generator that started scanning routes again would be snapshotting a
	// source this module cannot see.
	if strings.Contains(surface, "\nroute ") {
		t.Error("the surface carries route patterns — routes are the private services repo's golden (plan 49 step 4)")
	}
}

// Fields that never reach the wire must not be in the snapshot: pinning them
// would make the tripwire fire on changes no client can observe, and a guard
// that cries wolf gets regenerated without thought.
func TestTheSnapshotHoldsOnlyWhatReachesTheWire(t *testing.T) {
	surface, err := wiresnapshot.GenerateWireSurface(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	// BoxRequest.MemberID / MemberSecret are `json:"-"` — set server-side and
	// never accepted from the wire (plan 44 ticket 05).
	for _, line := range strings.Split(surface, "\n") {
		if strings.Contains(line, `json:"-"`) {
			t.Errorf("the snapshot pins a field that is not on the wire: %q", strings.TrimSpace(line))
		}
	}
	if strings.Contains(surface, "MemberSecret") {
		t.Error("BoxRequest.MemberSecret is json:\"-\" and must not be in the wire surface")
	}
}

// The failure text is the deliverable, so it is asserted like one. An agent
// that has read nothing else gets: what to write, where, how to classify it,
// how the floor pairs with it, the exact regeneration command, and the playbook
// by name.
func TestTheDriftMessageIsEnoughOnItsOwn(t *testing.T) {
	msg := driftInstructions()
	for _, want := range []struct{ why, needle string }{
		{"names the ledger folder", ".wire-changes/<slug>.md"},
		{"names all three classes", "breaking"},
		{"defines additive", "additive   new field / route / constant"},
		{"defines patch", "patch      the shape moved"},
		{"demands the old client's point of view", "OLD CLIENT'S point of view"},
		{"names the floor key", "floor: vX.Y.Z"},
		{"names the floor constant", "ClientVersionFloor"},
		{"says where the floor constant lives", "private services repo"},
		{"says the floor is met when the pin moves", "pin"},
		{"names the regeneration command", wiresnapshot.RegenerateCommand},
		{"points at the playbook by name", "Expand/contract: shipping a wire change"},
		{"says where the playbook lives", wirechanges.FormatDoc},
		{"names the two-release dance", "expand"},
		{"points at the entry-format doc", wirechanges.FormatDoc},
	} {
		if !strings.Contains(msg, want.needle) {
			t.Errorf("the drift message no longer %s (looked for %q) — an agent reading only this failure could not act on it", want.why, want.needle)
		}
	}
}

// The diff has to say WHICH type moved. `+ Version string` on its own names a
// field and not its owner, and the owner is the half that decides whether an
// old client cares — this is the shape of the first real drift this tripwire
// will see (plan 48 step 2 adds Member.Version).
func TestTheDriftDiffNamesTheTypeTheFieldLandedIn(t *testing.T) {
	committed := "## control-plane HTTP\n\ntype Member struct\n" +
		"\tID string `json:\"id\"`\n\tName string `json:\"name\"`\n\tRole string `json:\"role\"`\n" +
		"\tBranch string `json:\"branch,omitempty\"`\n\tMachine string `json:\"machine,omitempty\"`\n" +
		"\tOnline bool `json:\"online\"`\ntype Session struct\n\tPIN string `json:\"pin\"`\n" +
		"\tGeneration int `json:\"generation\"`\n\tStatus string `json:\"status\"`\n\tAccess string `json:\"access,omitempty\"`\n"
	current := strings.Replace(committed, "\tOnline bool", "\tVersion string `json:\"version,omitempty\"`\n\tOnline bool", 1)

	diff := wiresnapshot.WireSurfaceDiff(committed, current)
	if !strings.Contains(diff, "in type Member struct") {
		t.Errorf("the diff does not name the type the field landed in:\n%s", diff)
	}
	if !strings.Contains(diff, "+ \tVersion string") {
		t.Errorf("the diff does not show the added field:\n%s", diff)
	}
	if strings.Contains(diff, "- ") {
		t.Errorf("a pure addition reported a removal:\n%s", diff)
	}
	// And the far side of the file is not dragged along — three lines of
	// context, not the whole surface.
	if strings.Contains(diff, "Access string") {
		t.Errorf("the diff carries unrelated lines:\n%s", diff)
	}
}

// A removal reads as a removal. This is the shape of a CONTRACT step — the
// commit that is actually breaking — so it must not be reported as an edit
// somewhere vague.
func TestTheDriftDiffShowsARemovedFieldAsARemoval(t *testing.T) {
	committed := "## control-plane HTTP\n\ntype ClaimRequest struct\n" +
		"\tPIN string `json:\"pin\"`\n\tHostMachine string `json:\"hostMachine,omitempty\"`\n" +
		"\tHarness string `json:\"harness,omitempty\"`\n\tSeedURL string `json:\"seedUrl,omitempty\"`\n" +
		"\tOverlayAddr string `json:\"overlayAddr,omitempty\"`\n\tTakeover bool `json:\"takeover,omitempty\"`\n"
	current := strings.Replace(committed, "\tOverlayAddr string `json:\"overlayAddr,omitempty\"`\n", "", 1)

	diff := wiresnapshot.WireSurfaceDiff(committed, current)
	if !strings.Contains(diff, "- \tOverlayAddr string") {
		t.Errorf("the diff does not show the removed field:\n%s", diff)
	}
	if !strings.Contains(diff, "in type ClaimRequest struct") {
		t.Errorf("the diff does not name the type the field left:\n%s", diff)
	}
}

// The snapshot on disk is the one this tree generates AND it is where the docs
// say it is. A moved file with a stale pointer is a tripwire nobody can reset.
func TestTheCommittedSnapshotIsWhereEverythingSaysItIs(t *testing.T) {
	if filepath.Base(wiresnapshot.SnapshotPath) != "wire-surface.txt" {
		t.Fatalf("snapshot path %q — the README and the Makefile name wire-surface.txt", wiresnapshot.SnapshotPath)
	}
	if _, err := wiresnapshot.LoadWireSurface(repoRoot); err != nil {
		t.Fatalf("%s is not committed: %v", wiresnapshot.SnapshotPath, err)
	}
}
