package cli

import (
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/spf13/cobra"

	"github.com/RNG999/antic/internal/host/config"
)

func (a *app) pkgCmd() *cobra.Command {
	c := &cobra.Command{Use: "pkg", Short: "Packages installed in a guest", Args: cobra.NoArgs}
	ls := &cobra.Command{
		Use:               "ls NAME",
		Aliases:           []string{"list"},
		Short:             "List packages installed in guest NAME",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
			g, err := a.get(c.Context(), args[0])
			if err != nil {
				return err
			}
			return a.out(g.Cfg.Packages, func(w io.Writer) {
				for _, p := range g.Cfg.Packages {
					fmt.Fprintln(w, p)
				}
			})
		},
	}
	a.jsonFlag(ls)

	add := &cobra.Command{
		Use:               "add NAME PKG...",
		Short:             "Add packages to guest NAME and apply",
		Args:              cobra.MinimumNArgs(2),
		ValidArgsFunction: a.guestArgs,
		RunE:              func(c *cobra.Command, args []string) error { return a.editPkgs(c, args[0], args[1:], true) },
	}
	rm := &cobra.Command{
		Use:   "rm NAME PKG...",
		Short: "Remove packages from guest NAME and apply",
		Args:  cobra.MinimumNArgs(2),
		ValidArgsFunction: a.guestThen(func(g *config.Guest, rest []string) ([]string, cobra.ShellCompDirective) {
			return without(g.Packages, rest), cobra.ShellCompDirectiveNoFileComp
		}),
		RunE:              func(c *cobra.Command, args []string) error { return a.editPkgs(c, args[0], args[1:], false) },
	}
	c.AddCommand(ls, add, rm)
	return c
}

func (a *app) editPkgs(c *cobra.Command, name string, pkgs []string, add bool) error {
	g, err := a.get(c.Context(), name)
	if err != nil {
		return err
	}
	if g.Cfg.Flake != "" {
		return fmt.Errorf("guest %s uses custom flake %s; edit the flake directly and run \"antic guest apply %s\"", g.Name(), g.Cfg.Flake, g.Name())
	}
	next := slices.Clone(g.Cfg.Packages)
	for _, p := range pkgs {
		if !config.ValidPackage(p) {
			return fmt.Errorf("invalid package attribute %q", p)
		}
		if add {
			if !slices.Contains(next, p) {
				next = append(next, p)
			}
		} else {
			next = slices.DeleteFunc(next, func(s string) bool { return s == p })
		}
	}
	if err := a.cfg.SetPackages(g.Name(), next, a.dirs.Home); err != nil {
		return err
	}
	a.mgr, a.cfg = nil, nil
	g, err = a.get(c.Context(), name)
	if err != nil {
		return err
	}
	if err := g.Apply(c.Context()); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "antic: updated packages of %s: %v\n", name, next)
	return nil
}
