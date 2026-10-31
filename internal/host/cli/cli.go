// Package cli is antic's command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/guest"
	"github.com/RNG999/antic/internal/host/instance"
)

// Exit status for antic's own errors (as opposed to a command's status).
const exitError = 125

// version is set at build time.
var version = "dev"

// exitCode carries a command's exit status through cobra.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

type app struct {
	json bool // --json
	dirs instance.Dirs
	cfg  *config.Config
	mgr  *guest.Manager
}

// Main runs antic and returns its exit status.
func Main() int {
	if len(os.Args) > 1 && os.Args[1] == netnsCmd {
		return netnsMain(os.Args[2:])
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	a := &app{}
	var err error
	if a.dirs, err = instance.Default(); err != nil {
		fmt.Fprintf(os.Stderr, "antic: %v\n", err)
		return exitError
	}
	root := a.root()
	root.SetArgs(os.Args[1:])
	err = root.ExecuteContext(ctx)
	var code exitCode
	switch {
	case err == nil:
		return 0
	case errors.As(err, &code):
		return int(code)
	default:
		fmt.Fprintf(os.Stderr, "antic: %v\n", err)
		return exitError
	}
}

func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:   "antic",
		Short: "Run CLI tools in isolated NixOS guests",
		Long: `antic runs CLI tools (for example those an AI agent drives) in NixOS VMs
("guests"), isolated from the host. Guests are defined in
~/.config/antic/guests/<name>.toml; host directories are shared at the same path.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}
	root.AddGroup(&cobra.Group{ID: "run", Title: "Running commands:"}, &cobra.Group{ID: "manage", Title: "Managing:"})
	for _, c := range []*cobra.Command{a.runCmd(), a.shellCmd()} {
		c.GroupID = "run"
		root.AddCommand(c)
	}
	for _, c := range []*cobra.Command{a.guestCmd(), a.cacheCmd(), a.skillCmd()} {
		c.GroupID = "manage"
		root.AddCommand(c)
	}
	root.AddCommand(a.doctorCmd(), a.updateCmd(), a.versionCmd())
	return root
}

// loadConfig reads the config without connecting to systemd.
func (a *app) loadConfig() error {
	if a.cfg != nil {
		return nil
	}
	cwd, _ := os.Getwd()
	cfg, err := config.LoadWithProject(a.dirs.GuestsConfigDir(), a.dirs.Home, cwd)
	if err != nil {
		return err
	}
	a.cfg = cfg
	return nil
}

// load reads the config and connects to systemd, once.
func (a *app) load(ctx context.Context) error {
	if a.mgr != nil {
		return nil
	}
	if err := a.loadConfig(); err != nil {
		return err
	}
	m, err := guest.NewManager(ctx, a.dirs, a.cfg, os.Stderr)
	if err != nil {
		return err
	}
	a.mgr = m
	return nil
}

// get returns the guest named by name.
func (a *app) get(ctx context.Context, name string) (*guest.Guest, error) {
	if err := a.load(ctx); err != nil {
		return nil, err
	}
	return a.mgr.Get(name)
}

// resolveTargetGuest resolves a guest name when optional (or omitted in args).
// If a name is provided in args, it uses it and returns the remaining args.
// If args is empty:
//   - If exactly one guest exists, it selects that guest automatically.
//   - If multiple guests exist, it returns an error listing the available guests.
//   - If no guests exist, it returns an error saying no guests are defined.
func (a *app) resolveTargetGuest(ctx context.Context, args []string, usage string) (string, []string, error) {
	if err := a.loadConfig(); err != nil {
		return "", nil, err
	}
	if len(args) > 0 {
		return args[0], args[1:], nil
	}
	names := a.cfg.Names()
	if len(names) == 0 {
		return "", nil, fmt.Errorf("no guests defined (run \"antic guest create <name>\")")
	}
	if len(names) == 1 {
		return names[0], nil, nil
	}
	return "", nil, fmt.Errorf("guest NAME is required\n\nUsage:\n  %s\n\nAvailable guests:\n  %s", usage, strings.Join(names, ", "))
}

// resolveGuestAndItems resolves a target guest and trailing items (e.g. packages, ports, dirs).
// If args has at least 1 element and matches an existing guest name:
//
//	that guest is selected and args[1:] are items.
//
// Otherwise:
//   - If exactly one guest exists: that guest is selected and all args are items.
//   - If multiple guests exist: returns an error listing the available guests.
//   - If no guests exist: returns an error saying no guests are defined.
//
// Finally, if len(items) == 0: returns a syntax error indicating itemNoun is required.
func (a *app) resolveGuestAndItems(ctx context.Context, args []string, usage, itemNoun string) (string, []string, error) {
	if err := a.loadConfig(); err != nil {
		return "", nil, err
	}
	var guestName string
	var items []string
	if len(args) > 0 && (a.cfg.Guests[args[0]] != nil || a.cfg.Errors[args[0]] != nil) {
		guestName = args[0]
		items = args[1:]
	} else {
		names := a.cfg.Names()
		if len(names) == 1 {
			guestName = names[0]
			items = args
		} else if len(names) == 0 {
			return "", nil, fmt.Errorf("no guests defined (run \"antic guest create <name>\")")
		} else {
			return "", nil, fmt.Errorf("guest NAME is required\n\nUsage:\n  %s\n\nAvailable guests:\n  %s", usage, strings.Join(names, ", "))
		}
	}
	if len(items) == 0 {
		return "", nil, fmt.Errorf("syntax error: %s is required (usage: %s)", itemNoun, usage)
	}
	return guestName, items, nil
}

// out prints v as JSON with --json, or calls table.
func (a *app) out(v any, table func(w io.Writer)) error {
	if a.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	table(w)
	return w.Flush()
}

func (a *app) jsonFlag(c *cobra.Command) {
	c.Flags().BoolVar(&a.json, "json", false, "print JSON")
}

// confirm asks on the terminal; without one it refuses (use --yes).
func confirm(prompt string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("%s: refusing without --yes (stdin is not a terminal)", prompt)
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", prompt)
	var answer string
	fmt.Scanln(&answer)
	if answer != "y" && answer != "Y" {
		return errors.New("aborted")
	}
	return nil
}

// guestArgs completes guest names for the first argument.
func (a *app) guestArgs(c *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	if err := a.loadConfig(); err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return a.cfg.Names(), cobra.ShellCompDirectiveNoFileComp
}

// guestsArgs completes guest names for every argument (commands taking
// NAME...), leaving out names already given.
func (a *app) guestsArgs(c *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if err := a.loadConfig(); err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for _, n := range a.cfg.Names() {
		if !slices.Contains(args, n) {
			out = append(out, n)
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// guestThen completes guest names for the first argument and calls fn with
// that guest's config for the arguments after it.
func (a *app) guestThen(fn func(g *config.Guest, rest []string) ([]string, cobra.ShellCompDirective)) cobra.CompletionFunc {
	return func(c *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return a.guestArgs(c, args, toComplete)
		}
		if err := a.loadConfig(); err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		g, ok := a.cfg.Guests[args[0]]
		if !ok {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return fn(g, args[1:])
	}
}

// without returns items not in given.
func without(items, given []string) []string {
	var out []string
	for _, s := range items {
		if !slices.Contains(given, s) {
			out = append(out, s)
		}
	}
	return out
}

// networkModes completes --network.
func networkModes(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return []string{
		config.NetworkInternet + "\tinternet only (no LAN, host or link-local)",
		config.NetworkLAN + "\tinternet and LAN (no host or link-local)",
		config.NetworkNone + "\tno network device",
	}, cobra.ShellCompDirectiveNoFileComp
}
