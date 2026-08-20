// Command slopnextversion derives the next release number from the wire-change
// ledger and prints what cutting it would take.
//
// It reads `.wire-changes/` and the last tag reachable from HEAD, and writes a
// page to stdout: the derived version, the changelog block (the entries' own
// sentences, verbatim), and the consume → commit → tag sequence. It never tags
// and never pushes — the same shape as `make deploy-checklist`, which prints
// wrangler commands nothing in this repo runs.
//
//	make next-version           # derive and print; writes nothing
//	make next-version-consume   # the one writing step, and step one of the sequence
//
// `-consume` deletes the pending entries and appends their sentences to
// CHANGELOG.md under the new heading. With an empty ledger it writes nothing and
// says so, so running the sequence twice is harmless.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/nwylynko/slopball-cli/internal/nextversion"
)

func main() {
	consume := flag.Bool("consume", false, "delete the pending entries and append their sentences to CHANGELOG.md")
	flag.Parse()

	root := "."
	if args := flag.Args(); len(args) > 0 {
		root = args[0]
	}

	ctx := context.Background()
	lastTag, err := nextversion.LastReleaseTag(ctx, root)
	if err != nil {
		fail(err)
	}
	rel, err := nextversion.LoadRelease(ctx, root, lastTag)
	if err != nil {
		// The parser's own sentence, unwrapped: it already names the file and
		// the fix, and "next version: invalid entry" would send the reader to
		// grep this repo instead of to the entry they got wrong.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if !*consume {
		if err := nextversion.RenderNextVersion(os.Stdout, rel); err != nil {
			fail(err)
		}
		return
	}

	result, err := nextversion.ConsumeLedger(root, rel)
	if err != nil {
		fail(err)
	}
	if !result.Appended {
		fmt.Printf("the ledger is empty: nothing consumed, %s unchanged.\n", nextversion.ChangelogPath)
		fmt.Printf("%s is still the derived release — commit and tag it.\n", rel.Next)
		return
	}
	for _, file := range result.Removed {
		fmt.Printf("consumed %s\n", file)
	}
	fmt.Printf("appended %d sentence(s) to %s under %s\n", len(result.Removed), nextversion.ChangelogPath, result.Version)
	fmt.Printf("now commit the tree, then tag it %s — neither of those is this tool's to do.\n", result.Version)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "next version:", err)
	os.Exit(1)
}
