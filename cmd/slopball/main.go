// Command slopball is the single static binary that hosts and joins slopball
// sessions. It is deliberately thin — all behavior lives in internal packages,
// organized by feature vertical.
package main

import (
	"os"

	"github.com/nwylynko/slopball-cli/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
