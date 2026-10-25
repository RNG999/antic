package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/RNG999/antic/internal/host/config"
)

func (a *app) guestForward() *cobra.Command {
	return &cobra.Command{
		Use:               "forward NAME PORT...",
		Short:             "Forward TCP ports from host to guest NAME (e.g. 3000 or 8080:80)",
		Args:              cobra.MinimumNArgs(2),
		ValidArgsFunction: a.guestArgs,
		RunE:              func(c *cobra.Command, args []string) error { return a.editPorts(c, args[0], args[1:], true) },
	}
}

func (a *app) guestUnforward() *cobra.Command {
	return &cobra.Command{
		Use:   "unforward NAME PORT...",
		Short: "Remove port forwards from guest NAME",
		Args:  cobra.MinimumNArgs(2),
		ValidArgsFunction: a.guestThen(func(g *config.Guest, rest []string) ([]string, cobra.ShellCompDirective) {
			var ports []string
			for _, p := range g.Ports {
				ports = append(ports, fmt.Sprintf("%d\t127.0.0.1:%d -> %d", p.Host, p.Host, p.Guest))
			}
			var out []string
			for _, p := range ports {
				if !slices.Contains(rest, strings.SplitN(p, "\t", 2)[0]) {
					out = append(out, p)
				}
			}
			return out, cobra.ShellCompDirectiveNoFileComp
		}),
		RunE: func(c *cobra.Command, args []string) error { return a.editPorts(c, args[0], args[1:], false) },
	}
}

func (a *app) editPorts(c *cobra.Command, name string, specs []string, add bool) error {
	g, err := a.get(c.Context(), name)
	if err != nil {
		return err
	}
	for _, spec := range specs {
		pt, err := config.ParsePort(spec)
		if err != nil {
			return err
		}
		if add {
			if err := a.cfg.AddPort(name, pt, a.dirs.Home); err != nil {
				return err
			}
		} else {
			if err := a.cfg.RemovePort(name, pt.Host, a.dirs.Home); err != nil {
				return err
			}
		}
	}
	a.mgr, a.cfg = nil, nil
	g, err = a.get(c.Context(), name)
	if err != nil {
		return err
	}
	if up, err := g.Running(c.Context()); err == nil && up {
		fmt.Fprintf(os.Stderr, "antic: restarting %s to apply port forwards...\n", name)
		if err := g.Stop(c.Context()); err != nil {
			return err
		}
		if err := g.Start(c.Context()); err != nil {
			return err
		}
	}
	return nil
}
