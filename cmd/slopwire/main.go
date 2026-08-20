// Command slopwire regenerates the committed wire-surface snapshot.
//
// It reads the source of the four packages that carry slopball's wires and
// writes internal/wiresnapshot/wire-surface.txt. It dials nothing, holds no
// credential, and never writes a `.wire-changes/` entry for you — classifying
// the change is a judgment about somebody else's already-installed client, and
// that is the half a tool cannot do.
//
//	make wire-snapshot
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/nwylynko/slopball-cli/internal/wiresnapshot"
)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	surface, err := wiresnapshot.GenerateWireSurface(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wire snapshot:", err)
		os.Exit(1)
	}
	out := filepath.Join(root, wiresnapshot.SnapshotPath)
	if err := os.WriteFile(out, []byte(surface), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "wire snapshot:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", out)
	fmt.Println("now classify the change: .wire-changes/<slug>.md (see .wire-changes/README.md)")
}
