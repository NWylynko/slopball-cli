package cli

import (
	"context"
	"fmt"

	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/spf13/cobra"
)

func newAccessCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "access",
		Short: "Show or set whether the session accepts join requests",
		RunE:  runAccessShow,
	}
	addPinFlag(cmd)

	open := &cobra.Command{
		Use:   "open",
		Short: "Allow people to knock",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runAccessSet(cmd, controlplane.AccessOpen) },
	}
	addPinFlag(open)

	closed := &cobra.Command{
		Use:   "closed",
		Short: "Refuse new knocks and clear the queue",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runAccessSet(cmd, controlplane.AccessClosed) },
	}
	addPinFlag(closed)

	cmd.AddCommand(open, closed)
	return cmd
}

func runAccessShow(cmd *cobra.Command, _ []string) error {
	pin := resolvePin(cmd)
	if pin == "" {
		return fmt.Errorf("no session pin — run inside the session work tree, or pass --pin / set SLOPBALL_PIN")
	}
	sess, err := controlClient(cmd).Session(context.Background(), pin)
	if err != nil {
		return err
	}
	access := sess.Access
	if access == "" {
		access = controlplane.AccessOpen
	}
	fmt.Fprintln(cmd.OutOrStdout(), access)
	return nil
}

func runAccessSet(cmd *cobra.Command, access string) error {
	pin := resolvePin(cmd)
	if pin == "" {
		return fmt.Errorf("no session pin — run inside the session work tree, or pass --pin / set SLOPBALL_PIN")
	}
	if err := controlClient(cmd).SetAccess(context.Background(), pin, access); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), access)
	return nil
}
