package cli

import (
	"fmt"
	"io"
	"os"
	"os/user"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/RNG999/antic/internal/host/instance"
	"github.com/RNG999/antic/internal/host/nixbuild"
)

func (a *app) cacheCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "cache",
		Short: "Manage cached guest builds",
		Args:  cobra.NoArgs,
	}
	c.AddCommand(
		a.cacheLs(),
		a.cacheRm(),
	)
	return c
}

// cacheEntries returns all cached guest builds annotated with the guests that
// currently use each build (either via their installed store path or config).
func (a *app) cacheEntries() ([]nixbuild.CacheEntry, error) {
	entries, err := nixbuild.ListCache(a.dirs.CacheDir())
	if err != nil {
		return nil, err
	}
	_ = a.loadConfig()

	var username string
	if u, err := user.Current(); err == nil {
		username = u.Username
	}

	byKey := map[string][]string{}
	bySystem := map[string][]string{}
	if a.cfg != nil {
		for _, name := range a.cfg.Names() {
			if g := a.cfg.Guests[name]; g != nil {
				if g.FlakePath != "" {
					k := nixbuild.FlakeKey(g.FlakePath, g.FlakeAttr)
					byKey[k] = append(byKey[k], name)
				} else if username != "" {
					if k, _, err := nixbuild.BuiltinKey(username, g.Packages); err == nil {
						byKey[k] = append(byKey[k], name)
					}
				}
			}
			if inst, ok, err := a.dirs.Lookup(name); err == nil && ok {
				if cur, err := nixbuild.GuestStore(inst.StoreDir()).Current(); err == nil && cur != "" {
					bySystem[cur] = append(bySystem[cur], name)
				}
			}
		}
	}

	for i := range entries {
		var gs []string
		gs = append(gs, byKey[entries[i].Key]...)
		gs = append(gs, bySystem[entries[i].System]...)
		slices.Sort(gs)
		entries[i].Guests = slices.Compact(gs)
	}
	return entries, nil
}

func cachePackagesDesc(e nixbuild.CacheEntry) string {
	if e.Flake != "" {
		return e.Flake
	}
	if len(e.Packages) > 0 {
		return strings.Join(e.Packages, ", ")
	}
	return "-"
}

func (a *app) cacheLs() *cobra.Command {
	c := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List cached guest builds",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			entries, err := a.cacheEntries()
			if err != nil {
				return err
			}
			if entries == nil {
				entries = []nixbuild.CacheEntry{}
			}
			return a.out(entries, func(w io.Writer) {
				fmt.Fprintln(w, "GUESTS\tAGE\tPACKAGES")
				for _, e := range entries {
					guests := "-"
					if len(e.Guests) > 0 {
						guests = strings.Join(e.Guests, ", ")
					}
					entryAge := "-"
					if !e.Created.IsZero() {
						entryAge = age(time.Since(e.Created))
					}
					fmt.Fprintf(w, "%s\t%s\t%s\n", guests, entryAge, cachePackagesDesc(e))
				}
			})
		},
	}
	a.jsonFlag(c)
	return c
}

func (a *app) cacheRm() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:               "rm [GUEST...]",
		Aliases:           []string{"clean", "prune"},
		Short:             "Remove unused (or specified) cached builds and garbage-collect the Nix store",
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: a.guestsArgs,
		RunE: func(c *cobra.Command, args []string) error {
			unlockBuild, err := a.dirs.LockNixBuild()
			if err != nil {
				return err
			}
			defer unlockBuild()

			entries, err := a.cacheEntries()
			if err != nil {
				return err
			}

			type target struct {
				key  string
				desc string
			}
			var toRemove []target
			seen := map[string]bool{}
			add := func(e nixbuild.CacheEntry) {
				if !seen[e.Key] {
					seen[e.Key] = true
					toRemove = append(toRemove, target{key: e.Key, desc: cachePackagesDesc(e)})
				}
			}

			switch {
			case all:
				for _, e := range entries {
					add(e)
				}
			case len(args) == 0:
				for _, e := range entries {
					if len(e.Guests) == 0 {
						add(e)
					}
				}
			default:
				for _, arg := range args {
					matched := false
					for _, e := range entries {
						if slices.Contains(e.Guests, arg) || e.Key == arg {
							add(e)
							matched = true
						}
					}
					if !matched {
						return fmt.Errorf("no cached build found for guest %q", arg)
					}
				}
			}

			for _, t := range toRemove {
				if err := nixbuild.RemoveCache(a.dirs.CacheDir(), t.key); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "antic: removed cache (%s)\n", t.desc)
			}

			if len(args) == 0 || all {
				if orphans, err := a.dirs.Orphans(); err == nil {
					for _, p := range orphans {
						_ = instance.RemoveDir(p)
					}
				}
			}

			nix, err := nixbuild.New()
			if err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "antic: running nix store gc...")
			return nix.GC(c.Context())
		},
	}
	c.Flags().BoolVar(&all, "all", false, "remove all cached builds")
	return c
}
