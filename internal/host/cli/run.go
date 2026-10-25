package cli

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/guest"
	"github.com/RNG999/antic/internal/host/transport"
)

func (a *app) runCmd() *cobra.Command {
	var offline, tty, noTTY bool
	var workdir string
	c := &cobra.Command{
		Use:   "run NAME [flags] [--] CMD [ARG...]",
		Short: "Run CMD in guest NAME",
		Long: `Run CMD in guest NAME (in the current directory if shared, otherwise in the
guest user's home directory). The guest is started if needed. The exit status
is CMD's; antic's own errors exit with 125.

--offline runs CMD without any network.`,
		Args: cobra.MinimumNArgs(2),
		// NAME, then the guest's commands are unknown here: default completion.
		ValidArgsFunction: a.guestThen(func(*config.Guest, []string) ([]string, cobra.ShellCompDirective) {
			return nil, cobra.ShellCompDirectiveDefault
		}),
		RunE: func(c *cobra.Command, args []string) error {
			// Flag parsing stops at NAME (so CMD's own flags are left alone);
			// parse antic's flags between NAME and CMD here, up to "--" or
			// the first non-flag.
			if err := c.Flags().Parse(args[1:]); err != nil {
				return err
			}
			cmdArgs := c.Flags().Args()
			if len(cmdArgs) == 0 {
				return errors.New("syntax error: CMD is required (usage: antic run NAME [flags] [--] CMD [ARG...])")
			}
			g, err := a.get(c.Context(), args[0])
			if err != nil {
				return err
			}
			pty := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
			if tty {
				pty = true
			} else if noTTY {
				pty = false
			}
			return a.execIn(c.Context(), g, workdir, cmdArgs, offline, pty)
		},
	}
	c.Flags().SetInterspersed(false)
	c.Flags().BoolVar(&offline, "offline", false, "run without network")
	c.Flags().BoolVarP(&tty, "tty", "t", false, "always allocate a terminal")
	c.Flags().BoolVarP(&noTTY, "no-tty", "T", false, "never allocate a terminal")
	c.Flags().StringVarP(&workdir, "workdir", "w", "", "working directory inside the guest")
	return c
}

func (a *app) shellCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "shell NAME",
		Short:             "Interactive shell in guest NAME",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
			g, err := a.get(c.Context(), args[0])
			if err != nil {
				return err
			}
			return a.execIn(c.Context(), g, "", nil, false, true)
		},
	}
}

// execIn runs argv (or a login shell when argv is empty) in the guest and returns its status.
func (a *app) execIn(ctx context.Context, g *guest.Guest, workdir string, argv []string, offline, pty bool) error {
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
	cmd := transport.Cmd{Argv: argv, Dir: dir, Offline: offline, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Signals: sigs}
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
