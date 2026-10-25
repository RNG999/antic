package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/RNG999/antic/internal/host/config"
)

func (a *app) guestMount() *cobra.Command {
	var ro bool
	cmd := &cobra.Command{
		Use:   "mount NAME DIR...",
		Short: "Share and mount directories in guest NAME",
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
	cmd.Flags().BoolVar(&ro, "ro", false, "read-only")
	return cmd
}

func (a *app) guestUnmount() *cobra.Command {
	return &cobra.Command{
		Use:   "unmount NAME DIR...",
		Short: "Unmount and stop sharing directories with guest NAME",
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
}
