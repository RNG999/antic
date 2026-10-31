package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/RNG999/antic/internal/host/guest"
	"github.com/spf13/cobra"
)

func (a *app) guestLsCmd() *cobra.Command {
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

func (a *app) guestShowCmd() *cobra.Command {
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
					fmt.Fprintf(w, "Port:\t127.0.0.1:%d -> %d/%s%s\n", p.Host, p.Guest, p.ProtoString(), desc)
				}
				for _, s := range i.Shares {
					fmt.Fprintf(w, "Share:\t%s (%s%s)\n", s.Path, shareMode(s.ReadOnly), map[bool]string{true: ", mounted"}[s.Mounted])
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

func (a *app) guestLogsCmd() *cobra.Command {
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

func state(i guest.Info) string {
	return i.State
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
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
