package nextversion

import (
	"fmt"
	"io"
	"strings"

	"github.com/nwylynko/slopball-cli/internal/wirechanges"
)

// RenderNextVersion writes the page Nick reads at tag time.
//
// Every command on it is a command for a HUMAN to run — the same constraint the
// deploy checklist is printed under. The page shows the changelog block FIRST
// and the sequence second, because the block is the part worth arguing with: if
// a sentence reads wrong here, the fix is to edit the entry and re-run, not to
// fix it up in CHANGELOG.md afterwards where nothing keeps it honest.
func RenderNextVersion(w io.Writer, rel Release) error {
	p := func(format string, a ...any) { fmt.Fprintf(w, format+"\n", a...) }

	p("# slopball — next version")
	p("")
	p("DERIVED by `make next-version` from .wire-changes/ and the last tag. Nothing")
	p("here ran: this tool never tags and never pushes, the same way")
	p("`make deploy-checklist` never runs wrangler. Cutting the release is yours.")
	p("")
	p("  last tag       %s", rel.LastTag)
	p("  pending        %s", pendingSummary(rel.Entries))
	p("  next version   %s", rel.Next)
	first, second := whyLines(rel.Entries)
	p("  why            %s", first)
	p("                 %s", second)
	p("")

	p("## The changelog")
	p("")
	if block := ChangelogBlock(rel.Next, rel.Entries); block != "" {
		p("The entries' own sentences, verbatim. `make next-version-consume` appends")
		p("exactly these bytes to CHANGELOG.md and deletes the entries — there is no")
		p("second write-up, so there is nothing to drift.")
		p("")
		p("```markdown")
		fmt.Fprint(w, block)
		p("```")
		p("")
		p("## The sequence")
		p("")
		p("Consume, then commit, then tag — in that order, so the tag points at a tree")
		p("whose CHANGELOG.md already says what shipped and whose ledger is empty.")
		p("")
		p("```sh")
		p("make next-version-consume")
	} else {
		p("Nothing is appended to CHANGELOG.md: no entry was pending, so no sentence was")
		p("written by anybody who knew. The tag is the whole record of this release.")
		p("")
		p("## The sequence")
		p("")
		p("```sh")
	}
	p("git add -A && git commit -m %q", "release "+rel.Next)
	p("git tag -a %s -m %q", rel.Next, rel.Next)
	p("git push origin main --follow-tags")
	p("```")
	p("")
	p("The number is derived, not decided: it is max(class) over the pending entries,")
	p("and an empty ledger is a patch because a release changes more than the wire. It")
	p("is still a suggestion — tag something else if a release needs a number for a")
	p("reason the wire cannot see, and the next derivation reads whatever tag it finds.")
	return nil
}

// pendingSummary counts the ledger by class, in bump order.
func pendingSummary(entries []wirechanges.Entry) string {
	if len(entries) == 0 {
		return "nothing"
	}
	var parts []string
	for _, class := range wirechanges.Classes {
		if n := len(byClass(entries, class)); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, class))
		}
	}
	noun := "entries"
	if len(entries) == 1 {
		noun = "entry"
	}
	return fmt.Sprintf("%d %s — %s", len(entries), noun, strings.Join(parts, ", "))
}

// whyLines explains the bump in the ledger's own terms — two lines, wrapped to
// the column the table sets. The rule nobody remembers is the empty one, so the
// empty case says the whole reason out loud.
func whyLines(entries []wirechanges.Entry) (string, string) {
	if len(entries) == 0 {
		return "the ledger is empty — the wire did not move, and a release",
			"changes more than the wire, so the patch moves."
	}
	switch LargestClass(entries) {
	case wirechanges.ClassBreaking:
		return "a `breaking` entry is pending — an already-installed client",
			"stops working, so the major moves."
	case wirechanges.ClassAdditive:
		return "the largest pending class is `additive` — no already-installed",
			"client breaks, so the minor moves."
	default:
		return "every pending entry is `patch` — nothing an already-installed",
			"client can observe changed, so the patch moves."
	}
}
