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
	var desc string
	var proto string
	cmd := &cobra.Command{
		Use:               "forward [NAME] PORT...",
		Short:             "Forward ports from host to a guest (e.g. 3000, 8080:80, or 53/tcp,udp)",
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
			name, ports, err := a.resolveGuestAndItems(c.Context(), args, "antic guest forward [NAME] PORT... [-p tcp|udp|tcp,udp] [-d DESCRIPTION]", "PORT")
			if err != nil {
				return err
			}
			return a.editPorts(c, name, ports, desc, proto, true)
		},
	}
	cmd.Flags().StringVarP(&desc, "desc", "d", "", "optional explanation of why this port is exposed")
	cmd.Flags().StringVarP(&proto, "proto", "p", "tcp", "protocol: tcp, udp, or tcp,udp")
	cmd.RegisterFlagCompletionFunc("proto", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"tcp", "udp", "tcp,udp"}, cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

func (a *app) guestUnforward() *cobra.Command {
	return &cobra.Command{
		Use:   "unforward [NAME] PORT...",
		Short: "Remove port forwards from a guest",
		Args:  cobra.ArbitraryArgs,
		ValidArgsFunction: a.guestThen(func(g *config.Guest, rest []string) ([]string, cobra.ShellCompDirective) {
			var ports []string
			for _, p := range g.Ports {
				d := ""
				if p.Description != "" {
					d = " (" + p.Description + ")"
				}
				ports = append(ports, fmt.Sprintf("%d\t127.0.0.1:%d -> %d/%s%s", p.Host, p.Host, p.Guest, p.ProtoStr(), d))
			}
			var out []string
			for _, p := range ports {
				if !slices.Contains(rest, strings.SplitN(p, "\t", 2)[0]) {
					out = append(out, p)
				}
			}
			return out, cobra.ShellCompDirectiveNoFileComp
		}),
		RunE: func(c *cobra.Command, args []string) error {
			name, ports, err := a.resolveGuestAndItems(c.Context(), args, "antic guest unforward [NAME] PORT...", "PORT")
			if err != nil {
				return err
			}
			return a.editPorts(c, name, ports, "", "", false)
		},
	}
}

func (a *app) editPorts(c *cobra.Command, name string, specs []string, desc, proto string, add bool) error {
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
			if pt.Proto == "" {
				pt.Proto = proto
			}
			pt.Description = desc
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
