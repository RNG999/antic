package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
)

func (a *app) sessionCmd() *cobra.Command {
	c := &cobra.Command{Use: "session", Short: "Sessions (\"antic run\"s) running in a guest", Args: cobra.NoArgs}
	ls := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List sessions",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			g, err := a.pick(c.Context(), nil)
			if err != nil {
				return err
			}
			ss, err := g.Sessions(c.Context())
			if err != nil {
				return err
			}
			return a.out(ss, func(w io.Writer) {
				fmt.Fprintln(w, "SESSION\tNETWORK\tSTARTED\tCOMMAND")
				for _, s := range ss {
					net := "online"
					if s.Offline {
						net = "offline"
					}
					fmt.Fprintf(w, "%s\t%s\t%s ago\t%s\n", s.Unit, net, age(time.Since(time.Unix(s.Started, 0))), s.Command)
				}
			})
		},
	}
	a.jsonFlag(ls)
	kill := &cobra.Command{
		Use:   "kill SESSION...",
		Short: "Stop sessions",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			g, err := a.pick(c.Context(), nil)
			if err != nil {
				return err
			}
			for _, s := range args {
				if err := g.Kill(c.Context(), s); err != nil {
					return err
				}
			}
			return nil
		},
	}
	c.AddCommand(ls, kill)
	return c
}
