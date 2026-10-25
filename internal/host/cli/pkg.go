package cli

import (
	"fmt"
	"os"
	"slices"

	"github.com/spf13/cobra"

	"github.com/RNG999/antic/internal/host/config"
)

func (a *app) guestInstall() *cobra.Command {
	return &cobra.Command{
		Use:               "install NAME PKG...",
		Short:             "Install packages in guest NAME and apply",
		Args:              cobra.MinimumNArgs(2),
		ValidArgsFunction: a.guestArgs,
		RunE:              func(c *cobra.Command, args []string) error { return a.editPkgs(c, args[0], args[1:], true) },
	}
}

func (a *app) guestUninstall() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall NAME PKG...",
		Short: "Uninstall packages from guest NAME and apply",
		Args:  cobra.MinimumNArgs(2),
		ValidArgsFunction: a.guestThen(func(g *config.Guest, rest []string) ([]string, cobra.ShellCompDirective) {
			return without(g.Packages, rest), cobra.ShellCompDirectiveNoFileComp
		}),
		RunE: func(c *cobra.Command, args []string) error { return a.editPkgs(c, args[0], args[1:], false) },
	}
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
