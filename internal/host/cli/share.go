package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func (a *app) shareCmd() *cobra.Command {
	c := &cobra.Command{Use: "share", Short: "Directories shared with a guest", Args: cobra.NoArgs}
	ls := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List shares",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			g, err := a.pick(c.Context(), nil)
			if err != nil {
				return err
			}
			ss := g.Shares(c.Context())
			return a.out(ss, func(w io.Writer) {
				fmt.Fprintln(w, "PATH\tMODE\tSOURCE\tMOUNTED")
				for _, s := range ss {
					fmt.Fprintf(w, "%s\t%s\t%s\t%v\n", s.Path, mode(s.ReadOnly), s.Source, s.Mounted)
				}
			})
		},
	}
	a.jsonFlag(ls)

	var ro, persist, yes bool
	add := &cobra.Command{
		Use:   "add DIR...",
		Short: "Share directories with the guest (at the same path)",
		Long: `Share directories with the running guest; with --persist, also record them
in hanare.toml (then the guest need not be running). Directories outside
policy.share_roots need confirmation on a terminal (or --yes).`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			g, err := a.pick(c.Context(), nil)
			if err != nil {
				return err
			}
			for _, d := range args {
				abs, _ := filepath.Abs(d)
				if !a.mgr.InShareRoots(abs) && !yes {
					if err := confirm(fmt.Sprintf("%s is outside policy.share_roots; share it with %s?", abs, g.Name())); err != nil {
						return err
					}
				}
				p, err := g.AddShare(c.Context(), abs, ro, persist)
				if err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "hanare: shared %s with %s (%s)\n", p, g.Name(), mode(ro))
			}
			return nil
		},
	}
	add.Flags().BoolVar(&ro, "ro", false, "read-only")
	add.Flags().BoolVar(&persist, "persist", false, "also add to hanare.toml")
	add.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask about directories outside policy.share_roots")

	var rmPersist bool
	rm := &cobra.Command{
		Use:   "rm DIR...",
		Short: "Stop sharing directories (--persist: also remove from hanare.toml)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			g, err := a.pick(c.Context(), nil)
			if err != nil {
				return err
			}
			for _, d := range args {
				abs, _ := filepath.Abs(d)
				if err := g.RemoveShare(c.Context(), abs, rmPersist); err != nil {
					return err
				}
			}
			return nil
		},
	}
	rm.Flags().BoolVar(&rmPersist, "persist", false, "also remove from hanare.toml")
	c.AddCommand(ls, add, rm)
	return c
}
