package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/guest"
	"github.com/RNG999/antic/internal/host/transport"
)

func (a *app) runCmd() *cobra.Command {
	var tty, noTTY bool
	var workdir string
	c := &cobra.Command{
		Use:   "run [NAME] [flags] [--] CMD [ARG...]",
		Short: "Run CMD in a guest",
		Long: `Run CMD in a guest (in the current directory if shared, otherwise in the
guest user's home directory). The guest is started if needed. The exit status
is CMD's; antic's own errors exit with 125.`,
		Args: cobra.ArbitraryArgs,
		// NAME, then the guest's commands are unknown here: default completion.
		ValidArgsFunction: a.guestThen(func(*config.Guest, []string) ([]string, cobra.ShellCompDirective) {
			return nil, cobra.ShellCompDirectiveDefault
		}),
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 {
				return errors.New("syntax error: CMD is required (usage: antic run [NAME] [flags] [--] CMD [ARG...])")
			}
			if err := a.load(c.Context()); err != nil {
				return err
			}
			var guestName string
			var rest []string
			if a.cfg.Guests[args[0]] != nil || a.cfg.Errors[args[0]] != nil {
				guestName = args[0]
				rest = args[1:]
			} else {
				names := a.cfg.Names()
				if len(names) == 1 {
					guestName = names[0]
					rest = args
				} else if len(names) == 0 {
					return fmt.Errorf("no guests defined (run \"antic guest create <name>\")")
				} else {
					return fmt.Errorf("guest NAME is required\n\nUsage: antic run NAME [flags] [--] CMD [ARG...]\n\nAvailable guests: %s", strings.Join(names, ", "))
				}
			}
			if err := c.Flags().Parse(rest); err != nil {
				return err
			}
			cmdArgs := c.Flags().Args()
			if len(cmdArgs) == 0 {
				return errors.New("syntax error: CMD is required (usage: antic run [NAME] [flags] [--] CMD [ARG...])")
			}
			g, err := a.get(c.Context(), guestName)
			if err != nil {
				return err
			}
			pty := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
			if tty {
				pty = true
			} else if noTTY {
				pty = false
			}
			return a.execIn(c.Context(), g, workdir, cmdArgs, pty)
		},
	}
	c.Flags().SetInterspersed(false)
	c.Flags().BoolVarP(&tty, "tty", "t", false, "always allocate a terminal")
	c.Flags().BoolVarP(&noTTY, "no-tty", "T", false, "never allocate a terminal")
	c.Flags().StringVarP(&workdir, "workdir", "w", "", "working directory inside the guest")
	return c
}

func (a *app) shellCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "shell [NAME]",
		Short:             "Interactive shell in guest NAME",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
			name, _, err := a.resolveTargetGuest(c.Context(), args, "antic shell [NAME]")
			if err != nil {
				return err
			}
			g, err := a.get(c.Context(), name)
			if err != nil {
				return err
			}
			return a.execIn(c.Context(), g, "", nil, true)
		},
	}
}

// execIn runs argv (or a login shell when argv is empty) in the guest and returns its status.
func (a *app) execIn(ctx context.Context, g *guest.Guest, workdir string, argv []string, pty bool) error {
	dir := workdir
	if dir == "" {
		if d, err := g.Dir(ctx); err == nil {
			dir = d
		} else {
			dir = "~"
		}
	}
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	cmd := transport.Cmd{Argv: argv, Dir: dir, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Signals: sigs}
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
