package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/guest"
)

func (a *app) guestCmd() *cobra.Command {
	c := &cobra.Command{Use: "guest", Short: "Manage guests and guest resources", Args: cobra.NoArgs}
	c.AddCommand(
		a.guestLs(),
		a.guestCreate(),
		a.guestRm(),
		a.guestStart(),
		a.guestStop(),
		a.guestRestart(),
		a.guestSet(),
		a.guestEdit(),
		a.guestApply(),
		a.guestPs(),
		a.guestKill(),
		a.guestShow(),
		a.guestLogs(),
		a.guestInstall(),
		a.guestUninstall(),
		a.guestMount(),
		a.guestUnmount(),
		a.guestForward(),
		a.guestUnforward(),
		a.guestStats(),
	)
	return c
}

func (a *app) guestEdit() *cobra.Command {
	return a.editGuestCmd("edit [NAME]")
}

func (a *app) guestPs() *cobra.Command {
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
					fmt.Fprintf(w, "%s\t%s ago\t%s\n", s.Unit, age(time.Since(time.Unix(s.Started, 0))), s.Command)
				}
			})
		},
	}
	a.jsonFlag(c)
	return c
}

func (a *app) guestKill() *cobra.Command {
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

func (a *app) guestLs() *cobra.Command {
	var quiet bool
	c := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List guests",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := a.load(c.Context()); err != nil {
				return err
			}
			gs, err := a.mgr.All()
			if err != nil {
				return err
			}
			var infos []guest.Info
			for _, g := range gs {
				i, err := g.Inspect(c.Context())
				if err != nil {
					return err
				}
				infos = append(infos, i)
			}
			for name, err := range a.cfg.Errors {
				infos = append(infos, guest.Info{
					Name:        name,
					State:       "error",
					Description: err.Error(),
				})
			}
			sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
			if quiet {
				for _, i := range infos {
					fmt.Println(i.Name)
				}
				return nil
			}
			return a.out(infos, func(w io.Writer) {
				fmt.Fprintln(w, "NAME\tSTATE\tUPTIME\tCPUS\tMEMORY\tNETWORK")
				for _, i := range infos {
					cpus := fmt.Sprintf("%d", i.CPUs)
					mem := i.Memory
					net := i.Network
					upt := uptime(i)
					if i.State == "error" {
						cpus = "-"
						mem = "-"
						net = "-"
						upt = "-"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
						i.Name, state(i), upt, cpus, orDash(mem), orDash(net))
				}
			})
		},
	}
	c.Flags().BoolVarP(&quiet, "quiet", "q", false, "print names only")
	a.jsonFlag(c)
	return c
}

func (a *app) guestCreate() *cobra.Command {
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
			_, err := a.mgr.Create(c.Context(), name, spec)
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

func (a *app) guestSet() *cobra.Command {
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
			name = g.Name()
			if err := a.cfg.SetResources(name, u, a.dirs.Home); err != nil {
				return err
			}
			a.mgr, a.cfg = nil, nil
			g, err = a.get(c.Context(), name)
			if err != nil {
				return err
			}
			if up, err := g.Running(c.Context()); err == nil && up {
				fmt.Fprintf(os.Stderr, "antic: restarting %s to apply updated settings...\n", name)
				if err := g.Stop(c.Context()); err != nil {
					return err
				}
				return g.Start(c.Context())
			}
			return nil
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

func (a *app) guestRm() *cobra.Command {
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
				g, err := a.mgr.Get(n)
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

// eachGuest runs fn for the named guests, or every guest with all.
func (a *app) eachGuest(c *cobra.Command, args []string, all bool, fn func(*guest.Guest) error) error {
	if err := a.load(c.Context()); err != nil {
		return err
	}
	var gs []*guest.Guest
	if all {
		var err error
		if gs, err = a.mgr.All(); err != nil {
			return err
		}
	} else {
		if len(args) == 0 {
			names := a.cfg.Names()
			if len(names) == 1 {
				args = names
			} else if len(names) == 0 {
				return fmt.Errorf("no guests defined (run \"antic guest create <name>\")")
			} else {
				return fmt.Errorf("guest NAME is required\n\nAvailable guests: %s", strings.Join(names, ", "))
			}
		}
		for _, n := range args {
			g, err := a.mgr.Get(n)
			if err != nil {
				return err
			}
			gs = append(gs, g)
		}
	}
	var errs []error
	for _, g := range gs {
		if err := fn(g); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", g.Name(), err))
		}
	}
	return errors.Join(errs...)
}

func (a *app) guestStart() *cobra.Command {
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

func (a *app) guestStop() *cobra.Command {
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

func (a *app) guestRestart() *cobra.Command {
	return &cobra.Command{
		Use:               "restart [NAME...]",
		Short:             "Restart guests",
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: a.guestsArgs,
		RunE: func(c *cobra.Command, args []string) error {
			return a.eachGuest(c, args, false, func(g *guest.Guest) error {
				if err := g.Stop(c.Context()); err != nil {
					return err
				}
				if err := g.Start(c.Context()); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "antic: restarted guest %s\n", g.Name())
				return nil
			})
		},
	}
}

func (a *app) guestShow() *cobra.Command {
	var showPath bool
	var showConfig bool

	c := &cobra.Command{
		Use:               "show [NAME]",
		Short:             "Show a guest's configuration and state",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 && showPath {
				fmt.Println(a.dirs.GuestsConfigDir())
				return nil
			}

			name, _, err := a.resolveTargetGuest(c.Context(), args, "antic guest show [NAME] [flags]")
			if err != nil {
				return err
			}

			if showPath {
				g, err := a.cfg.Get(name)
				if err != nil {
					return err
				}
				fmt.Println(g.Path)
				return nil
			}

			if showConfig {
				g, err := a.cfg.Get(name)
				if err != nil {
					return err
				}
				b, err := os.ReadFile(g.Path)
				if err != nil {
					return err
				}
				os.Stdout.Write(b)
				return nil
			}

			g, err := a.get(c.Context(), name)
			if err != nil {
				return err
			}
			i, err := g.Inspect(c.Context())
			if err != nil {
				return err
			}
			return a.out(i, func(w io.Writer) {
				fmt.Fprintf(w, "Name:\t%s\nID:\t%s\nState:\t%s\nUptime:\t%s\nOS:\t%s\nSystem:\t%s\nCPUs:\t%d\nMemory:\t%s\nNetwork:\t%s\n",
					i.Name, orDash(i.ID), state(i), uptime(i), i.OS, orDash(i.System),
					i.CPUs, i.Memory, i.Network)
				for _, p := range i.Ports {
					desc := ""
					if p.Description != "" {
						desc = fmt.Sprintf(" (%s)", p.Description)
					}
					fmt.Fprintf(w, "Port:\t127.0.0.1:%d -> %d/%s%s\n", p.Host, p.Guest, p.ProtoStr(), desc)
				}
				for _, s := range i.Shares {
					fmt.Fprintf(w, "Share:\t%s (%s%s)\n", s.Path, mode(s.ReadOnly), map[bool]string{true: ", mounted"}[s.Mounted])
				}
				for _, u := range i.Units {
					fmt.Fprintf(w, "Unit:\t%s\n", u)
				}
			})
		},
	}
	c.Flags().BoolVar(&showPath, "path", false, "Print the guest config file path (or config directory if NAME omitted)")
	c.Flags().BoolVar(&showConfig, "config", false, "Print the guest raw TOML configuration")
	a.jsonFlag(c)
	return c
}

func (a *app) guestLogs() *cobra.Command {
	var follow, jrnl bool
	var units []string
	var lines int
	c := &cobra.Command{
		Use:               "logs [NAME]",
		Short:             "Print a guest's console log (or, with --journal, its systemd journal)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
			name, _, err := a.resolveTargetGuest(c.Context(), args, "antic guest logs [NAME] [flags]")
			if err != nil {
				return err
			}
			g, err := a.get(c.Context(), name)
			if err != nil {
				return err
			}
			if jrnl {
				w := &sanitizer{w: os.Stdout}
				return g.Journal(c.Context(), units, lines, func(l string) { w.Write([]byte(l + "\n")) })
			}
			f, err := os.Open(g.Console())
			if err != nil {
				return err
			}
			defer f.Close()
			w := &sanitizer{w: os.Stdout}
			for {
				_, err := io.Copy(w, f)
				if err != nil || !follow {
					return err
				}
				time.Sleep(300 * time.Millisecond)
			}
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing (console log)")
	c.Flags().BoolVar(&jrnl, "journal", false, "show the running guest's journal instead")
	c.Flags().StringSliceVarP(&units, "unit", "u", nil, "journal: only these units")
	c.Flags().IntVarP(&lines, "lines", "n", 200, "journal: number of lines")
	return c
}

func (a *app) guestApply() *cobra.Command {
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

func state(i guest.Info) string {
	return i.State
}

func uptime(i guest.Info) string {
	if i.Since != nil {
		return age(time.Since(*i.Since))
	}
	return "-"
}

func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func mode(ro bool) string {
	if ro {
		return "ro"
	}
	return "rw"
}

// sanitizer drops terminal control sequences (ESC sequences and other
// control characters except newline and tab) from guest output.
type sanitizer struct {
	w   io.Writer
	esc int // 0: text, 1: after ESC, 2: in a CSI sequence
}

func (s *sanitizer) Write(p []byte) (int, error) {
	out := make([]byte, 0, len(p))
	for _, b := range p {
		switch {
		case s.esc == 1:
			s.esc = 0
			if b == '[' {
				s.esc = 2
			}
		case s.esc == 2:
			if b >= 0x40 && b <= 0x7e {
				s.esc = 0
			}
		case b == 0x1b:
			s.esc = 1
		case b >= 0x20 && b != 0x7f, b == '\n', b == '\t':
			out = append(out, b)
		}
	}
	_, err := s.w.Write(out)
	return len(p), err
}
