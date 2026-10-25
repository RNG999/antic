package cli

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

//go:embed skill/SKILL.md
var skillMarkdown []byte

func (a *app) skillCmd() *cobra.Command {
	var project bool
	targetDir := func() (string, error) {
		if project {
			wd, err := os.Getwd()
			if err != nil {
				return "", err
			}
			return filepath.Join(wd, ".claude/skills/antic"), nil
		}
		return filepath.Join(a.dirs.Home, ".claude/skills/antic"), nil
	}

	c := &cobra.Command{
		Use:   "skill",
		Short: "Install or remove the antic skill for Claude Code (~/.claude/skills/antic)",
		Args:  cobra.NoArgs,
	}
	c.PersistentFlags().BoolVarP(&project, "project", "p", false, "use the current project's .claude/skills/antic instead of ~/.claude/skills/antic")

	install := &cobra.Command{
		Use:   "install",
		Short: "Install the antic skill for Claude Code",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			dir, err := targetDir()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			dst := filepath.Join(dir, "SKILL.md")
			if err := os.WriteFile(dst, skillMarkdown, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "antic: installed %s\n", dst)
			return nil
		},
	}

	rm := &cobra.Command{
		Use:     "rm",
		Aliases: []string{"uninstall"},
		Short:   "Remove the antic skill for Claude Code",
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			dir, err := targetDir()
			if err != nil {
				return err
			}
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "antic: removed %s\n", dir)
			return nil
		},
	}

	path := &cobra.Command{
		Use:   "path",
		Short: "Print the Claude Code skill file path",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			dir, err := targetDir()
			if err != nil {
				return err
			}
			fmt.Println(filepath.Join(dir, "SKILL.md"))
			return nil
		},
	}

	c.AddCommand(install, rm, path)
	return c
}
