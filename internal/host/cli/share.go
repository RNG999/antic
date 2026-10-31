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
		Use:   "mount [NAME] DIR...",
		Short: "Share and mount directories in a guest",
		Args:  cobra.ArbitraryArgs,
		ValidArgsFunction: a.guestThen(func(*config.Guest, []string) ([]string, cobra.ShellCompDirective) {
			return nil, cobra.ShellCompDirectiveFilterDirs
		}),
		RunE: func(c *cobra.Command, args []string) error {
			name, dirs, err := a.resolveGuestAndItems(c.Context(), args, "antic guest mount [NAME] DIR...", "DIR")
			if err != nil {
				return err
			}
			g, err := a.get(c.Context(), name)
			if err != nil {
				return err
			}
			for _, d := range dirs {
				abs, _ := filepath.Abs(d)
				p, changed, err := g.AddShare(c.Context(), abs, ro)
				if err != nil {
					return err
				}
				if !changed {
					fmt.Fprintf(os.Stderr, "antic: %s is already shared with %s (%s)\n", p, g.Name(), mode(ro))
					continue
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
		Use:   "unmount [NAME] DIR...",
		Short: "Unmount and stop sharing directories with a guest",
		Args:  cobra.ArbitraryArgs,
		ValidArgsFunction: a.guestThen(func(g *config.Guest, rest []string) ([]string, cobra.ShellCompDirective) {
			var paths []string
			for _, s := range g.Shares {
				paths = append(paths, s.Path)
			}
			return without(paths, rest), cobra.ShellCompDirectiveNoFileComp
		}),
		RunE: func(c *cobra.Command, args []string) error {
			name, dirs, err := a.resolveGuestAndItems(c.Context(), args, "antic guest unmount [NAME] DIR...", "DIR")
			if err != nil {
				return err
			}
			g, err := a.get(c.Context(), name)
			if err != nil {
				return err
			}
			for _, d := range dirs {
				abs, _ := filepath.Abs(d)
				p, lingers, err := g.RemoveShare(c.Context(), abs)
				if err != nil {
					return err
				}
				if lingers {
					fmt.Fprintf(os.Stderr, "antic: stopped sharing %s with %s (it stays mounted until the guest restarts)\n", p, g.Name())
					continue
				}
				fmt.Fprintf(os.Stderr, "antic: stopped sharing %s with %s\n", p, g.Name())
			}
			return nil
		},
	}
}
