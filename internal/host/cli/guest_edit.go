package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

func (a *app) guestEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "edit [NAME]",
		Short:             "Edit a copy of a guest's config file with $VISUAL or $EDITOR; it takes effect only if valid",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
			name, _, err := a.resolveTargetGuest(c.Context(), args, "antic guest edit [NAME]")
			if err != nil {
				return err
			}
			g, err := a.get(c.Context(), name)
			if err != nil {
				return err
			}
			old, err := os.ReadFile(g.Cfg.Path)
			if err != nil {
				return err
			}
			// The user edits a copy; the file changes only if the result
			// loads and takes effect (D10).
			tmp, err := os.CreateTemp("", "antic-"+g.Name()+"-*.toml")
			if err != nil {
				return err
			}
			_, werr := tmp.Write(old)
			if err := errors.Join(werr, tmp.Close()); err != nil {
				return err
			}
			if err := runEditor(tmp.Name()); err != nil {
				return err
			}
			edited, err := os.ReadFile(tmp.Name())
			if err != nil {
				return err
			}
			changed, err := g.Edit(c.Context(), old, edited)
			if err != nil {
				return fmt.Errorf("%w\n(your edits are kept in %s)", err, tmp.Name())
			}
			os.Remove(tmp.Name())
			if !changed {
				fmt.Fprintf(os.Stderr, "antic: %s is unchanged\n", g.Cfg.Path)
			}
			return nil
		},
	}
}

// runEditor edits path with $VISUAL or $EDITOR, which may carry arguments
// (as git runs them, D12), or vi.
func runEditor(path string) error {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.Command("sh", "-c", editor+` "$@"`, editor, path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
