package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/RNG999/antic/internal/host/config"
)

func (a *app) shareCmd() *cobra.Command {
	c := &cobra.Command{Use: "share", Short: "Directories shared with a guest", Args: cobra.NoArgs}
	ls := &cobra.Command{
		Use:               "ls NAME",
		Aliases:           []string{"list"},
		Short:             "List shared directories of guest NAME",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
			g, err := a.get(c.Context(), args[0])
			if err != nil {
				return err
			}
			ss := g.Shares(c.Context())
			return a.out(ss, func(w io.Writer) {
				fmt.Fprintln(w, "PATH\tMODE\tMOUNTED")
				for _, s := range ss {
					fmt.Fprintf(w, "%s\t%s\t%v\n", s.Path, mode(s.ReadOnly), s.Mounted)
				}
			})
		},
	}
	a.jsonFlag(ls)

	var ro bool
	add := &cobra.Command{
		Use:   "add NAME DIR...",
		Short: "Share directories with guest NAME",
		Args:  cobra.MinimumNArgs(2),
		ValidArgsFunction: a.guestThen(func(*config.Guest, []string) ([]string, cobra.ShellCompDirective) {
			return nil, cobra.ShellCompDirectiveFilterDirs
		}),
		RunE: func(c *cobra.Command, args []string) error {
			g, err := a.get(c.Context(), args[0])
			if err != nil {
				return err
			}
			for _, d := range args[1:] {
				abs, _ := filepath.Abs(d)
				p, err := g.AddShare(c.Context(), abs, ro)
				if err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "antic: shared %s with %s (%s)\n", p, g.Name(), mode(ro))
			}
			return nil
		},
	}
	add.Flags().BoolVar(&ro, "ro", false, "read-only")

	rm := &cobra.Command{
		Use:   "rm NAME DIR...",
		Short: "Stop sharing directories with guest NAME",
		Args:  cobra.MinimumNArgs(2),
		ValidArgsFunction: a.guestThen(func(g *config.Guest, rest []string) ([]string, cobra.ShellCompDirective) {
			var paths []string
			for _, s := range g.Shares {
				paths = append(paths, s.Path)
			}
			return without(paths, rest), cobra.ShellCompDirectiveNoFileComp
		}),
		RunE: func(c *cobra.Command, args []string) error {
			g, err := a.get(c.Context(), args[0])
			if err != nil {
				return err
			}
			for _, d := range args[1:] {
				abs, _ := filepath.Abs(d)
				if err := g.RemoveShare(c.Context(), abs); err != nil {
					return err
				}
			}
			return nil
		},
	}
	c.AddCommand(ls, add, rm)
	return c
}
