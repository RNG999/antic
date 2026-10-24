// Command antic-agent runs inside an antic guest, started by the systemd
// units of nix/module.nix: the root agent that serves the host and the
// socket-activated session runner.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/RNG999/antic/internal/guest/agent"
	"github.com/RNG999/antic/internal/guest/session"
	"github.com/RNG999/antic/internal/proto"
)

func main() {
	root := &cobra.Command{
		Use:           "antic-agent",
		Short:         "antic's guest-side agent and session runner",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(agentCmd(), sessionCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "antic-agent: %v\n", err)
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

func sessionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "session",
		Short: "run one socket-activated command or shell session over vsock",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return session.Run() },
	}
}
