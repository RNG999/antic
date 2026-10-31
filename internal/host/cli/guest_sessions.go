package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/RNG999/antic/internal/domain"
	"github.com/spf13/cobra"
)

func (a *app) guestPsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:               "ps [NAME]",
		Short:             "List running command sessions in a guest",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
			name, _, err := a.resolveTargetGuest(c.Context(), args, "antic guest ps [NAME]")
			if err != nil {
				return err
			}
			g, err := a.get(c.Context(), name)
			if err != nil {
				return err
			}
			ss, err := g.Sessions(c.Context())
			if err != nil {
				return err
			}
			return a.out(ss, func(w io.Writer) {
				fmt.Fprintln(w, "SESSION\tSTARTED\tCOMMAND")
				for _, s := range ss {
					fmt.Fprintf(w, "%s\t%s ago\t%s\n", domain.Printable(s.Unit), age(time.Since(time.Unix(s.Started, 0))), domain.Printable(s.Command))
				}
			})
		},
	}
	a.jsonFlag(c)
	return c
}

func (a *app) guestKillCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "kill [NAME] SESSION...",
		Short:             "Stop running command sessions in a guest",
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
			name, sessions, err := a.resolveGuestAndItems(c.Context(), args, "antic guest kill [NAME] SESSION...", "SESSION")
			if err != nil {
				return err
			}
			g, err := a.get(c.Context(), name)
			if err != nil {
				return err
			}
			for _, s := range sessions {
				if err := g.Kill(c.Context(), s); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
