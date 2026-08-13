package cli

import (
	"github.com/nwylynko/slopball-cli/update"
	"github.com/spf13/cobra"
)

// runUpdate is `slopball update`: replace this binary with the latest release.
//
// It resolves the running executable and hands that directory to the updater,
// rather than letting the script pick one. A slopball invoked by an absolute
// path is not on PATH at all, and one that shadows another would otherwise be
// left in place while a different copy was replaced — an update that reports
// success and changes nothing the user can see.
//
// Everything else — which asset, which release, how to replace a running binary
// on darwin without AMFI killing it — lives in the shell scripts the site
// serves, and deliberately not here. See slopball-cli/update for why.
func runUpdate(cmd *cobra.Command, _ []string) error {
	dir, err := update.BinaryDir()
	if err != nil {
		return err
	}
	return update.Apply(cmd.Context(), cmd.OutOrStdout(), dir)
}
