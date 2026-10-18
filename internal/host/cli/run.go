package cli

import (
	"context"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/RNG999/hanare/internal/host/guest"
	"github.com/RNG999/hanare/internal/host/transport"
)

func (a *app) runCmd() *cobra.Command {
	var offline, tty, noTTY bool
	c := &cobra.Command{
		Use:   "run [flags] [--] CMD [ARG...]",
		Short: "Run CMD in the guest, in the current directory",
		Long: `Run CMD in the guest, in the current directory (which must be inside a
share). The guest is started if needed. The exit status is CMD's; hanare's
own errors exit with 125.

--offline (or the guest's commands.offline) runs CMD without any network.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			g, err := a.pick(c.Context(), nil)
			if err != nil {
				return err
			}
			pty := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
			if tty {
				pty = true
			} else if noTTY {
				pty = false
			}
			return a.exec(c.Context(), g, args, offline || g.IsOffline(args[0]), pty, false)
		},
	}
	c.Flags().SetInterspersed(false)
	c.Flags().BoolVar(&offline, "offline", false, "run without network")
	c.Flags().BoolVarP(&tty, "tty", "t", false, "always allocate a terminal")
	c.Flags().BoolVarP(&noTTY, "no-tty", "T", false, "never allocate a terminal")
	return c
}

func (a *app) shellCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shell",
		Short: "Interactive shell in the guest",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			g, err := a.pick(c.Context(), nil)
			if err != nil {
				return err
			}
			return a.exec(c.Context(), g, nil, false, true, true)
		},
	}
}

// exec runs argv (or a login shell) in the guest and returns its status.
func (a *app) exec(ctx context.Context, g *guest.Guest, argv []string, offline, pty, shell bool) error {
	dir, err := g.Dir(ctx)
	if err != nil {
		if !shell {
			return err
		}
		dir = "~"
	}
	line := "cd " + shellQuote(dir) + ` && exec "$SHELL" -l`
	if dir == "~" {
		line = `cd && exec "$SHELL" -l`
	}
	if !shell {
		quoted := make([]string, len(argv))
		for i, s := range argv {
			quoted[i] = shellQuote(s)
		}
		line = "cd " + shellQuote(dir) + " && exec " + strings.Join(quoted, " ")
	}
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	cmd := transport.Cmd{Line: line, Offline: offline, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Signals: sigs}
	if pty {
		cmd.PTY = os.Stdin
	}
	code, err := g.Exec(ctx, cmd)
	if err != nil {
		return err
	}
	if code != 0 {
		return exitCode(code)
	}
	return nil
}

// runLinked runs a tool invoked through a ~/.local/bin link, in the guest
// whose commands.link lists it (or the default guest).
func (a *app) runLinked(ctx context.Context, tool string, args []string) error {
	if err := a.load(ctx); err != nil {
		return err
	}
	name := ""
	for _, n := range a.cfg.Names() {
		if slices.Contains(a.cfg.Guests[n].Commands.Link, tool) {
			name = n
			break
		}
	}
	g, err := a.mgr.Get(name)
	if err != nil {
		return err
	}
	pty := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	return a.exec(ctx, g, append([]string{tool}, args...), g.IsOffline(tool), pty, false)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
