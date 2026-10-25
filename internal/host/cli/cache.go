package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/RNG999/antic/internal/host/instance"
	"github.com/RNG999/antic/internal/host/nixbuild"
)

func (a *app) cacheCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "cache",
		Short: "Manage cached guest OS builds and Nix store garbage",
		Args:  cobra.NoArgs,
	}
	c.AddCommand(
		a.cacheLs(),
		a.cacheRm(),
		a.cacheGC(),
	)
	return c
}

func (a *app) cacheLs() *cobra.Command {
	var quiet bool
	c := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List cached guest OS builds",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			entries, err := nixbuild.ListCache(a.dirs.CacheDir())
			if err != nil {
				return err
			}
			if entries == nil {
				entries = []nixbuild.CacheEntry{}
			}
			if quiet {
				for _, e := range entries {
					fmt.Println(e.Key)
				}
				return nil
			}
			return a.out(entries, func(w io.Writer) {
				fmt.Fprintln(w, "KEY\tOS\tCREATED\tSYSTEM")
				for _, e := range entries {
					created := "-"
					if !e.Created.IsZero() {
						created = age(time.Since(e.Created)) + " ago"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.Key, e.OS, created, e.System)
				}
			})
		},
	}
	c.Flags().BoolVarP(&quiet, "quiet", "q", false, "print cache keys only")
	a.jsonFlag(c)
	return c
}

func (a *app) cacheRm() *cobra.Command {
	var all, runGC, yes bool
	c := &cobra.Command{
		Use:     "rm [KEY...] | --all",
		Aliases: []string{"clean", "prune"},
		Short:   "Remove cached guest OS builds (and optionally garbage-collect the Nix store)",
		Args:    cobra.ArbitraryArgs,
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			entries, err := nixbuild.ListCache(a.dirs.CacheDir())
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			var out []string
			for _, e := range entries {
				if !slices.Contains(args, e.Key) {
					out = append(out, fmt.Sprintf("%s\t%s", e.Key, e.OS))
				}
			}
			return out, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 && !all && !runGC {
				return errors.New("syntax error: specify KEY..., --all, or --gc (usage: antic cache rm [KEY...] [--all] [--gc])")
			}
			unlockBuild, err := a.dirs.LockNixBuild()
			if err != nil {
				return err
			}
			defer unlockBuild()

			if all {
				removed, err := nixbuild.RemoveAllCache(a.dirs.CacheDir())
				if err != nil {
					return err
				}
				for _, k := range removed {
					fmt.Fprintf(os.Stderr, "antic: removed cache %s\n", k)
				}
				if orphans, err := a.dirs.Orphans(); err == nil {
					for _, p := range orphans {
						_ = instance.RemoveDir(p)
					}
				}
			} else {
				for _, k := range args {
					if err := nixbuild.RemoveCache(a.dirs.CacheDir(), k); err != nil {
						return err
					}
					fmt.Fprintf(os.Stderr, "antic: removed cache %s\n", k)
				}
			}

			if runGC {
				nix, err := nixbuild.New()
				if err != nil {
					return err
				}
				fmt.Fprintln(os.Stderr, "antic: running nix store gc...")
				if err := nix.GC(c.Context()); err != nil {
					return err
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all", false, "remove all cached builds")
	c.Flags().BoolVar(&runGC, "gc", false, "also run 'nix store gc' on the host Nix store")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask")
	return c
}

func (a *app) cacheGC() *cobra.Command {
	return &cobra.Command{
		Use:   "gc",
		Short: "Prune orphaned guest state and run 'nix store gc' on the host Nix store",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			unlockBuild, err := a.dirs.LockNixBuild()
			if err != nil {
				return err
			}
			defer unlockBuild()

			if orphans, err := a.dirs.Orphans(); err == nil {
				for _, p := range orphans {
					_ = instance.RemoveDir(p)
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
}
