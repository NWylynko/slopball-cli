package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/nwylynko/slopball-cli/boxexec"
	"github.com/nwylynko/slopball-cli/conductor"
	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/spf13/cobra"
)

// requireSessionBox is the shared gate for session-scoped box verbs.
func requireSessionBox(ctx context.Context, client *controlplane.Client, pin string) (controlplane.BoxFacts, error) {
	if pin == "" {
		return controlplane.BoxFacts{}, fmt.Errorf("no session pin — run inside the session work tree, or pass --pin / set SLOPBALL_PIN")
	}
	sess, err := client.Session(ctx, pin)
	if err != nil {
		return controlplane.BoxFacts{}, err
	}
	if sess.Box == nil {
		return controlplane.BoxFacts{}, fmt.Errorf("session %s has no box — provision one with `slopball --box` or `slopball box add`", pin)
	}
	b := *sess.Box
	switch b.State {
	case controlplane.BoxRequested, controlplane.BoxProvisioning:
		return controlplane.BoxFacts{}, fmt.Errorf("session %s box is %s — wait for it to be ready", pin, b.State)
	case controlplane.BoxFailed:
		return controlplane.BoxFacts{}, fmt.Errorf("session %s box failed: %s", pin, strings.TrimSpace(b.Error))
	case controlplane.BoxReady, "":
		return b, nil
	default:
		return controlplane.BoxFacts{}, fmt.Errorf("session %s box is not ready (state %q)", pin, b.State)
	}
}

func sessionExecClient(ctx context.Context, client *controlplane.Client, pin string) (*boxexec.Client, error) {
	d, err := client.SessionDialer(ctx, pin)
	if err != nil {
		return nil, err
	}
	return &boxexec.Client{Dialer: d}, nil
}

func remoteBoxArgv(cmd *cobra.Command, args []string) ([]string, error) {
	if dash := cmd.ArgsLenAtDash(); dash >= 0 {
		remote := args[dash:]
		if len(remote) == 0 {
			return nil, fmt.Errorf("nothing after --")
		}
		return remote, nil
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("usage: slopball box run <command>... (optional `--` before flagged commands)")
	}
	return args, nil
}

func runSessionBoxLogs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("slopball box logs takes no host argument")
	}
	pin := resolvePin(cmd)
	ctx := context.Background()
	client := controlClient(cmd)
	if _, err := requireSessionBox(ctx, client, pin); err != nil {
		return err
	}
	url, err := logsEndpoint(ctx, client, pin)
	if err != nil {
		return fmt.Errorf("session %s logs: %w", pin, err)
	}
	if url == "" {
		return fmt.Errorf("session %s has no published logs endpoint yet", pin)
	}
	src := &conductor.RemoteLogSource{URL: url}
	if err := src.Probe(); err != nil {
		return fmt.Errorf("session %s logs at %s are unreachable: %w", pin, url, err)
	}
	resp, err := httpGet(ctx, url)
	if err != nil {
		return fmt.Errorf("session %s logs at %s: %w", pin, url, err)
	}
	if resp == "" {
		return fmt.Errorf("session %s logs at %s returned no output", pin, url)
	}
	fmt.Fprint(cmd.OutOrStdout(), resp)
	return nil
}

func runSessionBoxRun(cmd *cobra.Command, args []string) error {
	argv, err := remoteBoxArgv(cmd, args)
	if err != nil {
		return err
	}
	pin := resolvePin(cmd)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client := controlClient(cmd)
	if _, err := requireSessionBox(ctx, client, pin); err != nil {
		return err
	}
	execClient, err := sessionExecClient(ctx, client, pin)
	if err != nil {
		return err
	}
	_, err = execClient.Run(ctx, argv, os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr())
	return err
}

func runSessionBoxRm(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("slopball box rm takes no host argument")
	}
	yes, _ := cmd.Flags().GetBool("yes")
	if !yes {
		if !isCharDevice(os.Stdin) || !isCharDevice(os.Stdout) {
			return fmt.Errorf("refusing to remove the box without a TTY — pass --yes to confirm")
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Remove the session's box? The session keeps running. [y/N] ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
		default:
			return fmt.Errorf("cancelled")
		}
	}
	pin := resolvePin(cmd)
	ctx := context.Background()
	client := controlClient(cmd)
	box, err := requireSessionBox(ctx, client, pin)
	if err != nil {
		return err
	}
	execClient, err := sessionExecClient(ctx, client, pin)
	if err == nil {
		if err := execClient.Shutdown(ctx); err != nil {
			if box.Provider == controlplane.BoxProviderBYO {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not signal the box to shut down: %v\n", err)
			}
		}
	} else if box.Provider == controlplane.BoxProviderBYO {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not reach the box to shut down: %v\n", err)
	}
	if err := client.DestroyBox(ctx, pin); err != nil {
		return fmt.Errorf("destroy the session box: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "removed the box from session %s\n", pin)
	return nil
}

func httpGet(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("answered %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	return string(b), nil
}
