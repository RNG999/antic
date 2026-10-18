// Command banken ("watchdog") runs inside a hanare guest as root, started by
// the systemd units of nix/module.nix: the agent that serves the host, and
// the BPF programs that restrict and audit the guest user.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/RNG999/hanare/internal/guest/agent"
	"github.com/RNG999/hanare/internal/guest/audit"
	"github.com/RNG999/hanare/internal/guest/sd"
	"github.com/RNG999/hanare/internal/guest/userns"
	"github.com/RNG999/hanare/internal/proto"
)

func main() {
	root := &cobra.Command{
		Use:           "banken",
		Short:         "hanare's guest-side agent and BPF programs (run by systemd units in the guest)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(agentCmd(), restrictUsernsCmd(), auditCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "banken: %v\n", err)
		os.Exit(1)
	}
}

func agentCmd() *cobra.Command {
	var user string
	c := &cobra.Command{
		Use:   "agent --user NAME",
		Short: fmt.Sprintf("serve the host on vsock port %d (mounts, apply, sessions)", proto.PortAgent),
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			a, err := agent.New(user)
			if err != nil {
				return err
			}
			return a.Serve()
		},
	}
	c.Flags().StringVar(&user, "user", "", "guest login user")
	c.MarkFlagRequired("user")
	return c
}

func restrictUsernsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restrict-userns",
		Short: "deny creating user namespaces to everyone but root (pinned BPF LSM program)",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return userns.Restrict() },
	}
}

func auditCmd() *cobra.Command {
	var uid uint32
	var out string
	c := &cobra.Command{
		Use:   "audit [--uid N] [--out PATH]",
		Short: "log the user's executions, connections and write-opens as JSON lines",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			f, err := audit.OpenSerial(out)
			if err != nil {
				return err
			}
			defer f.Close()
			return audit.Run(uid, f, sd.NotifyReady)
		},
	}
	c.Flags().Uint32Var(&uid, "uid", 1000, "user to audit")
	c.Flags().StringVar(&out, "out", proto.AuditPort, "serial port the host records")
	return c
}
