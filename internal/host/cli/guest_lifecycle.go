package cli

import (
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/guest"
	"github.com/spf13/cobra"
)

func (a *app) guestCreateCmd() *cobra.Command {
	var pkgs, portSpecs []string
	var cpus int
	var memory, network string
	c := &cobra.Command{
		Use:               "create NAME",
		Short:             "Create, build, and start a guest",
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: cobra.NoFileCompletions, // a new name
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 {
				return errors.New("syntax error: guest NAME is required (usage: antic guest create NAME [flags])")
			}
			if len(args) > 1 {
				return errors.New("syntax error: unexpected arguments (usage: antic guest create NAME [flags])")
			}
			if err := a.load(c.Context()); err != nil {
				return err
			}
			name := args[0]
			var ports []config.Port
			for _, ps := range portSpecs {
				pt, err := config.ParsePort(ps)
				if err != nil {
					return err
				}
				ports = append(ports, pt)
			}
			if len(pkgs) == 0 && a.cfg.Guests[name] == nil {
				pkgs = []string{"git"}
			}
			spec := config.Guest{
				Packages: pkgs,
				CPUs:     cpus,
				Memory:   memory,
				Network:  config.NetworkConfig{Mode: network},
				Ports:    ports,
			}
			flags := slices.ContainsFunc([]string{"pkg", "cpus", "memory", "network", "port"}, c.Flags().Changed)
			_, err := a.mgr.Create(c.Context(), name, spec, flags)
			return err
		},
	}
	c.Flags().StringSliceVarP(&pkgs, "pkg", "p", nil, "nixpkgs packages to install in the guest")
	c.Flags().IntVar(&cpus, "cpus", 0, "number of vCPUs (default 2)")
	c.Flags().StringVar(&memory, "memory", "", "memory size (default 4G)")
	c.Flags().StringVar(&network, "network", "", "network mode: internet, lan, or none")
	c.Flags().StringSliceVar(&portSpecs, "port", nil, "host-to-guest TCP port forwards (e.g. 3000 or 8080:80)")
	c.RegisterFlagCompletionFunc("network", networkModes)
	for _, f := range []string{"pkg", "cpus", "memory", "port"} {
		c.RegisterFlagCompletionFunc(f, cobra.NoFileCompletions)
	}
	return c
}

func (a *app) guestSetCmd() *cobra.Command {
	var cpus int
	var memory, network string
	c := &cobra.Command{
		Use:               "set [NAME]",
		Short:             "Update a guest's CPUs, memory, or network mode",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
			name, _, err := a.resolveTargetGuest(c.Context(), args, "antic guest set [NAME] [flags]")
			if err != nil {
				return err
			}
			g, err := a.get(c.Context(), name)
			if err != nil {
				return err
			}
			var u config.ResourceUpdates
			changed := false
			if c.Flags().Changed("cpus") {
				u.CPUs = &cpus
				changed = true
			}
			if c.Flags().Changed("memory") {
				u.Memory = &memory
				changed = true
			}
			if c.Flags().Changed("network") {
				u.Network = &network
				changed = true
			}
			if !changed {
				return errors.New("specify at least one of --cpus, --memory, or --network")
			}
			changed, err = g.Reconfigure(c.Context(), func(d config.Draft) (config.Draft, error) { return d.SetResources(u) })
			if err == nil && !changed {
				fmt.Fprintf(os.Stderr, "antic: %s already has these settings\n", g.Name())
			}
			return err
		},
	}
	c.Flags().IntVar(&cpus, "cpus", 0, "number of vCPUs")
	c.Flags().StringVar(&memory, "memory", "", "memory size (e.g. 4G or 8G)")
	c.Flags().StringVar(&network, "network", "", "network mode: internet, lan, or none")
	c.RegisterFlagCompletionFunc("network", networkModes)
	for _, f := range []string{"cpus", "memory"} {
		c.RegisterFlagCompletionFunc(f, cobra.NoFileCompletions)
	}
	return c
}

func (a *app) guestRmCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:               "rm NAME...",
		Short:             "Delete guests (their state and their config file)",
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: a.guestsArgs,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 {
				return errors.New("syntax error: guest NAME is required (usage: antic guest rm NAME...)")
			}
			if err := a.load(c.Context()); err != nil {
				return err
			}
			for _, n := range args {
				if err := a.mgr.RecoverOrphan(c.Context(), n); err != nil {
					return err
				}
				g, err := a.mgr.Get(n)
				if err != nil && a.cfg.Errors[n] != nil {
					g, err = a.mgr.GetUnloadable(n)
				}
				if err != nil {
					return err
				}
				if !yes {
					if err := confirm(fmt.Sprintf("Delete guest %s?", n)); err != nil {
						return err
					}
				}
				if err := g.Remove(c.Context()); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "antic: removed guest %s\n", n)
			}
			return nil
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	return c
}

func (a *app) guestStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "start [NAME...]",
		Short:             "Start guests and wait until they are ready",
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: a.guestsArgs,
		RunE: func(c *cobra.Command, args []string) error {
			return a.eachGuest(c, args, false, func(g *guest.Guest) error {
				if err := g.Start(c.Context()); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "antic: started guest %s\n", g.Name())
				return nil
			})
		},
	}
}

func (a *app) guestStopCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:               "stop [NAME...] | --all",
		Short:             "Shut guests down",
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: a.guestsArgs,
		RunE: func(c *cobra.Command, args []string) error {
			return a.eachGuest(c, args, all, func(g *guest.Guest) error {
				if err := g.Stop(c.Context()); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "antic: stopped guest %s\n", g.Name())
				return nil
			})
		},
	}
	c.Flags().BoolVar(&all, "all", false, "stop every guest")
	return c
}

func (a *app) guestRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "restart [NAME...]",
		Short:             "Restart guests",
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: a.guestsArgs,
		RunE: func(c *cobra.Command, args []string) error {
			return a.eachGuest(c, args, false, func(g *guest.Guest) error {
				if err := g.Restart(c.Context()); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "antic: restarted guest %s\n", g.Name())
				return nil
			})
		},
	}
}

func (a *app) guestApplyCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "apply [NAME]",
		Short:             "Build the guest's OS and switch to it",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
			name, _, err := a.resolveTargetGuest(c.Context(), args, "antic guest apply [NAME]")
			if err != nil {
				return err
			}
			g, err := a.get(c.Context(), name)
			if err != nil {
				return err
			}
			return g.Apply(c.Context())
		},
	}
}
