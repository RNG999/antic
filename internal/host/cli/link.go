package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/spf13/cobra"
)

// Links are ~/.local/bin/TOOL symlinks to hanare; invoked through one,
// hanare runs TOOL in the guest whose commands.link lists it. hanare.toml is
// the only source of truth: add/rm edit it, then sync.

type linkInfo struct {
	Tool   string `json:"tool"`
	Guest  string `json:"guest"`
	Status string `json:"status"` // ok, missing, shadowed (by a host command earlier in PATH), stray
}

func (a *app) linkCmd() *cobra.Command {
	c := &cobra.Command{Use: "link", Short: "~/.local/bin links that run tools in a guest", Args: cobra.NoArgs}
	ls := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List links",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := a.load(c.Context()); err != nil {
				return err
			}
			infos := a.links()
			return a.out(infos, func(w io.Writer) {
				fmt.Fprintln(w, "TOOL\tGUEST\tSTATUS")
				for _, l := range infos {
					fmt.Fprintf(w, "%s\t%s\t%s\n", l.Tool, orDash(l.Guest), l.Status)
				}
			})
		},
	}
	a.jsonFlag(ls)
	sync := &cobra.Command{
		Use:   "sync",
		Short: "Make the links match commands.link of all guests",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := a.load(c.Context()); err != nil {
				return err
			}
			return a.syncLinks()
		},
	}
	add := &cobra.Command{
		Use:   "add TOOL...",
		Short: "Link tools to the guest (-g) and sync",
		Args:  cobra.MinimumNArgs(1),
		RunE:  func(c *cobra.Command, args []string) error { return a.editLinks(c, args, true) },
	}
	rm := &cobra.Command{
		Use:   "rm TOOL...",
		Short: "Unlink tools and sync",
		Args:  cobra.MinimumNArgs(1),
		RunE:  func(c *cobra.Command, args []string) error { return a.editLinks(c, args, false) },
	}
	c.AddCommand(ls, sync, add, rm)
	return c
}

func (a *app) editLinks(c *cobra.Command, tools []string, add bool) error {
	g, err := a.pick(c.Context(), nil)
	if err != nil {
		return err
	}
	links := slices.Clone(g.Cfg.Commands.Link)
	for _, t := range tools {
		if add && !slices.Contains(links, t) {
			links = append(links, t)
		}
		if !add {
			links = slices.DeleteFunc(links, func(s string) bool { return s == t })
		}
	}
	if err := a.cfg.SetLinks(g.Name(), links, a.dirs.Home); err != nil {
		return err
	}
	a.mgr, a.cfg = nil, nil
	if err := a.load(c.Context()); err != nil {
		return err
	}
	return a.syncLinks()
}

// links reports the desired links and stray ones.
func (a *app) links() []linkInfo {
	var out []linkInfo
	want := map[string]bool{}
	for _, n := range a.cfg.Names() {
		for _, t := range a.cfg.Guests[n].Commands.Link {
			want[t] = true
			st := "missing"
			if dst, err := os.Readlink(filepath.Join(a.dirs.BinDir, t)); err == nil && dst == "hanare" {
				st = "ok"
			}
			if host := a.hostCommand(t); host != "" {
				st = "shadowed by " + host
			}
			out = append(out, linkInfo{t, n, st})
		}
	}
	for _, t := range a.currentLinks() {
		if !want[t] {
			out = append(out, linkInfo{t, "", "stray"})
		}
	}
	return out
}

func (a *app) syncLinks() error {
	for _, l := range a.links() {
		p := filepath.Join(a.dirs.BinDir, l.Tool)
		switch {
		case l.Status == "stray":
			if err := os.Remove(p); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "hanare: removed %s\n", p)
		case l.Status == "missing":
			if _, err := os.Lstat(p); err == nil {
				return fmt.Errorf("%s exists and is not a hanare link", p)
			}
			if err := os.Symlink("hanare", p); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "hanare: linked %s -> %s\n", l.Tool, l.Guest)
		case l.Status != "ok":
			fmt.Fprintf(os.Stderr, "hanare: %s is %s; use \"hanare run %s\"\n", l.Tool, l.Status, l.Tool)
		}
	}
	return nil
}

// hostCommand returns a host command named name found in PATH before
// ~/.local/bin (it would win over the link).
func (a *app) hostCommand(name string) string {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == a.dirs.BinDir {
			return ""
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

func (a *app) currentLinks() []string {
	entries, _ := os.ReadDir(a.dirs.BinDir)
	var out []string
	for _, e := range entries {
		if dst, err := os.Readlink(filepath.Join(a.dirs.BinDir, e.Name())); err == nil && dst == "hanare" {
			out = append(out, e.Name())
		}
	}
	return out
}
