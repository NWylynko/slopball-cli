package cli

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/spf13/cobra"
)

func newMembersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "members",
		Short: "List session members and pending join requests",
		RunE:  runMembersList,
	}
	addPinFlag(cmd)

	accept := &cobra.Command{
		Use:   "accept <member-id>",
		Short: "Accept a pending join request",
		Args:  cobra.ExactArgs(1),
		RunE:  runMembersAccept,
	}
	addPinFlag(accept)

	decline := &cobra.Command{
		Use:   "decline <member-id>",
		Short: "Decline a pending join request",
		Args:  cobra.ExactArgs(1),
		RunE:  runMembersDecline,
	}
	addPinFlag(decline)

	cmd.AddCommand(accept, decline)
	return cmd
}

func runMembersList(cmd *cobra.Command, _ []string) error {
	pin := resolvePin(cmd)
	if pin == "" {
		return fmt.Errorf("no session pin — run inside the session work tree, or pass --pin / set SLOPBALL_PIN")
	}
	ctx := context.Background()
	client := controlClient(cmd)
	sess, err := client.Session(ctx, pin)
	if err != nil {
		return err
	}
	pending, err := client.PendingMembers(ctx, pin)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintf(w, "ID\tNAME\tSTATE\tMACHINE\tSEEN\n")
	for _, m := range sess.Members {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", m.ID, m.Name, orMemberState(m), m.Machine, ago(m.LastSeenAt))
	}
	for _, m := range pending {
		fmt.Fprintf(w, "%s\t%s\tpending\t%s\t%s\n", m.ID, m.Name, m.Machine, ago(m.LastSeenAt))
	}
	_ = w.Flush()
	if len(pending) > 0 {
		fmt.Fprintf(out, "\n%d join request(s) waiting — `slopball members accept <id>` or `decline <id>`\n", len(pending))
	}
	return nil
}

func runMembersAccept(cmd *cobra.Command, args []string) error {
	return decideMember(cmd, args[0], controlplane.DecisionAccept)
}

func runMembersDecline(cmd *cobra.Command, args []string) error {
	return decideMember(cmd, args[0], controlplane.DecisionDecline)
}

func decideMember(cmd *cobra.Command, id, decision string) error {
	pin := resolvePin(cmd)
	if pin == "" {
		return fmt.Errorf("no session pin — run inside the session work tree, or pass --pin / set SLOPBALL_PIN")
	}
	m, err := controlClient(cmd).DecideMember(context.Background(), pin, id, decision)
	if err != nil {
		return err
	}
	switch decision {
	case controlplane.DecisionAccept:
		fmt.Fprintf(cmd.OutOrStdout(), "accepted %s (%s)\n", m.ID, m.Name)
	default:
		fmt.Fprintf(cmd.OutOrStdout(), "declined %s (%s)\n", m.ID, m.Name)
	}
	return nil
}

func orMemberState(m controlplane.Member) string {
	if m.State != "" {
		if m.Online {
			return m.State
		}
		return m.State + "/offline"
	}
	if m.Online {
		return "online"
	}
	return "offline"
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t).Round(time.Second)
	if d < time.Minute {
		return d.String() + " ago"
	}
	return d.Round(time.Minute).String() + " ago"
}
