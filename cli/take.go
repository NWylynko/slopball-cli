package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/placement"
	"github.com/nwylynko/slopball-cli/session"
	"github.com/spf13/cobra"
)

// runTake implements `slopball take <git|dev|conductor|all>` — the manual half
// of placement (plan 30). Automatic placement covers the case nobody is around
// for; this covers the case where somebody is, and it is also how a returning
// creator picks its services back up.
//
// It cannot steal from a live owner. `take git` against a healthy box is
// refused, which is plan 25's guarantee restated as a lease: nothing is ever
// taken from an owner that is still serving.
func runTake(cmd *cobra.Command, args []string) error {
	s, _, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	services, err := servicesArg(args[0])
	if err != nil {
		return err
	}
	client := controlClient(cmd)
	ctx := context.Background()
	memberID, err := resolveMember(s)
	if err != nil {
		return err
	}
	force, _ := cmd.Flags().GetBool("force")
	hostname, _ := os.Hostname()
	name := strings.TrimPrefix(s.Branch, "client/")
	if name == "" {
		name = hostname
	}

	out := cmd.OutOrStdout()
	var failed []string
	for _, svc := range services {
		lease, err := client.ClaimLease(ctx, s.PIN, controlplane.LeaseRequest{
			Service: svc, MemberID: memberID, Name: name, Machine: hostname,
			TTLSeconds: controlplane.DefaultLeaseTTL, Force: force,
		})
		if err != nil {
			// A live owner refusing is the mechanism working. Say who has it and
			// what the escape hatch is, then keep going: `take all` should place
			// what it can rather than stopping at the first held service.
			failed = append(failed, fmt.Sprintf("%s: %v", svc, err))
			fmt.Fprintf(out, "%-10s refused — %v\n", svc, err)
			continue
		}
		fmt.Fprintf(out, "%-10s taken by %s (%s)\n", svc, lease.OwnerName, lease.Machine)
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not take %s (use --force only for a wedged owner)", strings.Join(failed, "; "))
	}
	fmt.Fprintln(out, "\nthe service starts here on this machine's next tick — leave `slopball` or `slopball join` running")
	return nil
}

// runHandOff implements `slopball hand-off <service> [--to <member>]` — give a
// service up on purpose, without waiting for a lease to expire. With no target
// it simply frees the service and placement re-homes it.
func runHandOff(cmd *cobra.Command, args []string) error {
	s, _, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	services, err := servicesArg(args[0])
	if err != nil {
		return err
	}
	client := controlClient(cmd)
	ctx := context.Background()
	memberID, err := resolveMember(s)
	if err != nil {
		return err
	}
	to, _ := cmd.Flags().GetString("to")
	toID := ""
	if to != "" {
		if toID, err = memberIDByName(ctx, client, s.PIN, to); err != nil {
			return err
		}
	}

	out := cmd.OutOrStdout()
	for _, svc := range services {
		lease, err := client.HandoverLease(ctx, s.PIN, controlplane.LeaseRequest{
			Service: svc, MemberID: memberID, To: toID, TTLSeconds: controlplane.DefaultLeaseTTL,
		})
		if err != nil {
			return fmt.Errorf("hand off %s: %w", svc, err)
		}
		if lease.Owner == "" {
			fmt.Fprintf(out, "%-10s handed back — the best-ranked member picks it up next tick\n", svc)
			continue
		}
		fmt.Fprintf(out, "%-10s handed to %s (%s)\n", svc, lease.OwnerName, lease.Machine)
	}
	return nil
}

// servicesArg expands the argument, including the `all` shorthand a returning
// creator types.
func servicesArg(arg string) ([]string, error) {
	if strings.EqualFold(arg, "all") {
		return controlplane.Services, nil
	}
	for _, s := range controlplane.Services {
		if strings.EqualFold(arg, s) {
			return []string{s}, nil
		}
	}
	return nil, fmt.Errorf("unknown service %q — slopball places %s", arg, strings.Join(controlplane.Services, ", "))
}

// resolveMember finds this machine's member id. A machine with session state
// but no member id is not a member — it must join (or be invited), never
// silently self-register (plan 44).
func resolveMember(s session.Session) (string, error) {
	if s.MemberID != "" {
		return s.MemberID, nil
	}
	return "", fmt.Errorf("this machine is not a member of %s — run `slopball join %s`", s.PIN, s.PIN)
}

func memberIDByName(ctx context.Context, client *controlplane.Client, pin, name string) (string, error) {
	sess, err := client.Session(ctx, pin)
	if err != nil {
		return "", err
	}
	id, err := MemberIDAmong(sess.Members, name)
	if err != nil {
		return "", fmt.Errorf("no member %q in %s — %w", name, pin, err)
	}
	return id, nil
}

// MemberIDAmong resolves a hand-off target. Left members keep their secret's
// authority but are not addressable by name — never resolve to a ghost (plan 44).
//
// Exported because that exclusion is the decision rule and its worst case is
// unreachable from outside: the Session API never returns a left member, so the
// only way to prove a caller who somehow held one cannot address it is to hand
// this the roster directly.
func MemberIDAmong(members []controlplane.Member, name string) (string, error) {
	var known []string
	for _, m := range members {
		if m.State == controlplane.MemberLeft {
			continue
		}
		if strings.EqualFold(m.Name, name) || m.ID == name {
			return m.ID, nil
		}
		known = append(known, m.Name)
	}
	return "", fmt.Errorf("members: %s", strings.Join(known, ", "))
}

// runServices prints the current placement: where each service is, and why an
// unplaced one is unplaced.
func runServices(cmd *cobra.Command, _ []string) error {
	s, _, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	sess, err := controlClient(cmd).Session(context.Background(), s.PIN)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	now := time.Now()
	for _, svc := range controlplane.Services {
		fmt.Fprintf(out, "%-10s %s\n", svc, placement.Describe(sess, svc, now))
	}
	return nil
}
