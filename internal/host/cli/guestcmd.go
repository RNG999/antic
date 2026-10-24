package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/RNG999/antic/internal/host/guest"
)

func (a *app) guestCmd() *cobra.Command {
	c := &cobra.Command{Use: "guest", Short: "Manage guests", Args: cobra.NoArgs}
	c.AddCommand(a.guestLs(), a.guestCreate(), a.guestRm(), a.guestStart(), a.guestStop(), a.guestRestart(),
		a.guestInspect(), a.guestLogs(), a.guestApply(), a.guestPrune())
	return c
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
			if quiet {
				for _, i := range infos {
					fmt.Println(i.Name)
				}
				return nil
			}
			return a.out(infos, func(w io.Writer) {
				fmt.Fprintln(w, "NAME\tID\tSTATE\tOS\tCPUS\tMEMORY\tNETWORK\tSHARES\tDEFAULT")
				for _, i := range infos {
					def := ""
					if i.Default {
						def = "*"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%d\t%s\n",
						i.Name, orDash(i.ID), state(i), tilde(i.OS, a.dirs.Home), i.CPUs, i.Memory, i.Network, len(i.Shares), def)
				}
			})
		},
	}
	c.Flags().BoolVarP(&quiet, "quiet", "q", false, "print names only")
	a.jsonFlag(c)
	return c
}

func (a *app) guestCreate() *cobra.Command {
	var from, flake, attr string
	c := &cobra.Command{
		Use:   "create NAME",
		Short: "Add a guest to antic.toml (if needed) and build it",
		Long: `Add [guests.NAME] to antic.toml, using the OS of another guest (--from,
default: the default guest) or --flake/--attr, and build it. A guest that is
already in antic.toml but not built yet is just built.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := a.load(c.Context()); err != nil {
				return err
			}
			if flake == "" && a.cfg.Guests[args[0]] == nil {
				src, err := a.cfg.Pick(from)
				if err != nil {
					return err
				}
				flake, attr = src.Flake, src.Attr
			}
			if attr == "" {
				attr = "antic"
			}
			_, err := a.mgr.Create(c.Context(), args[0], flake, attr)
			return err
		},
	}
	c.Flags().StringVar(&from, "from", "", "guest whose OS (flake, attr) to use")
	c.Flags().StringVar(&flake, "flake", "", "flake with nixosConfigurations.<attr>")
	c.Flags().StringVar(&attr, "attr", "", "attribute in nixosConfigurations (default antic)")
	return c
}

func (a *app) guestRm() *cobra.Command {
	var yes, keepConfig bool
	c := &cobra.Command{
		Use:               "rm NAME...",
		Short:             "Delete guests (their state, and their table in antic.toml)",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
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
				if err := g.Remove(c.Context(), keepConfig); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "antic: removed guest %s\n", n)
			}
			return nil
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	c.Flags().BoolVar(&keepConfig, "keep-config", false, "keep the guest's table in antic.toml")
	return c
}

// eachGuest runs fn for the named guests (default: the default guest), or
// every guest with all.
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
			args = []string{a.guest}
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
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
			return a.eachGuest(c, args, false, func(g *guest.Guest) error { return g.Start(c.Context()) })
		},
	}
}

func (a *app) guestStop() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:               "stop NAME... | --all",
		Short:             "Shut guests down",
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 && !all {
				return errors.New("name the guests to stop, or use --all")
			}
			return a.eachGuest(c, args, all, func(g *guest.Guest) error { return g.Stop(c.Context()) })
		},
	}
	c.Flags().BoolVar(&all, "all", false, "stop every guest")
	return c
}

func (a *app) guestRestart() *cobra.Command {
	return &cobra.Command{
		Use:               "restart [NAME...]",
		Short:             "Restart guests",
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
			return a.eachGuest(c, args, false, func(g *guest.Guest) error {
				if err := g.Stop(c.Context()); err != nil {
					return err
				}
				return g.Start(c.Context())
			})
		},
	}
}

func (a *app) guestInspect() *cobra.Command {
	c := &cobra.Command{
		Use:               "inspect [NAME]",
		Short:             "Show a guest's configuration and state",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
			g, err := a.pick(c.Context(), args)
			if err != nil {
				return err
			}
			i, err := g.Inspect(c.Context())
			if err != nil {
				return err
			}
			return a.out(i, func(w io.Writer) {
				fmt.Fprintf(w, "Name:\t%s\nID:\t%s\nState:\t%s\nOS:\t%s\nSystem:\t%s\nCPUs:\t%d\nMemory:\t%s\nNetwork:\t%s\n",
					i.Name, orDash(i.ID), state(i), i.OS, orDash(i.System), i.CPUs, i.Memory, i.Network)
				for _, s := range i.Shares {
					fmt.Fprintf(w, "Share:\t%s (%s, %s%s)\n", s.Path, mode(s.ReadOnly), s.Source, map[bool]string{true: ", mounted"}[s.Mounted])
				}
				for _, u := range i.Units {
					fmt.Fprintf(w, "Unit:\t%s\n", u)
				}
			})
		},
	}
	a.jsonFlag(c)
	return c
}

func (a *app) guestLogs() *cobra.Command {
	var follow, jrnl bool
	var units []string
	var lines int
	c := &cobra.Command{
		Use:               "logs [NAME]",
		Short:             "Print a guest's console log (or, with --journal, its journal)",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
			g, err := a.pick(c.Context(), args)
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
			// Console output comes from the guest: never pass terminal
			// control sequences through.
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
		Use:   "apply [NAME]",
		Short: "Build the guest's OS from its flake and switch to it",
		Long: `Build nixosConfigurations.<attr> of the guest's flake on the host, copy it
into the guest's store and, if the guest is running, switch to it there.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
			g, err := a.pick(c.Context(), args)
			if err != nil {
				return err
			}
			return g.Apply(c.Context())
		},
	}
}

func (a *app) guestPrune() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "prune",
		Short: "Delete guest state that no guest name refers to any more",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := a.load(c.Context()); err != nil {
				return err
			}
			o, err := a.mgr.Orphans()
			if err != nil || len(o) == 0 {
				return err
			}
			fmt.Fprintln(os.Stderr, strings.Join(o, "\n"))
			if !yes {
				if err := confirm("Delete these?"); err != nil {
					return err
				}
			}
			return a.mgr.Prune(o)
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	return c
}

func state(i guest.Info) string {
	if i.Since != nil {
		return "up " + age(time.Since(*i.Since))
	}
	return i.State
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

func tilde(p, home string) string {
	if strings.HasPrefix(p, home+"/") {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
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
