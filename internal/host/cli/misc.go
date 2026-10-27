package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/guest"
	"github.com/RNG999/antic/internal/host/nixbuild"
	"github.com/RNG999/antic/internal/proto"
)

func (a *app) editGuestCmd(use string) *cobra.Command {
	return &cobra.Command{
		Use:               use,
		Short:             "Edit a guest's config file with $EDITOR, then check it",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.guestArgs,
		RunE: func(c *cobra.Command, args []string) error {
			name, _, err := a.resolveTargetGuest(c.Context(), args, "antic guest edit [NAME]")
			if err != nil {
				return err
			}
			g, err := a.cfg.Get(name)
			if err != nil {
				return err
			}
			editor := os.Getenv("EDITOR")
			if editor == "" {
				editor = "vi"
			}
			cmd := exec.Command(editor, g.Path)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := cmd.Run(); err != nil {
				return err
			}
			cwd, _ := os.Getwd()
			_, err = config.LoadWithProject(a.dirs.GuestsConfigDir(), a.dirs.Home, cwd)
			return err
		},
	}
}

func (a *app) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use: "version", Short: "Print antic's version and guest protocol", Args: cobra.NoArgs,
		Run: func(*cobra.Command, []string) { fmt.Printf("antic %s (guest protocol %d)\n", version, proto.Version) },
	}
}

// doctorCmd checks the host's prerequisites.
func (a *app) doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use: "doctor", Short: "Check the host's prerequisites", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			bad := 0
			check := func(what string, err error) {
				if err != nil {
					bad++
					fmt.Printf("FAIL  %s: %v\n", what, err)
				} else {
					fmt.Printf("ok    %s\n", what)
				}
			}
			kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
			if err == nil {
				kvm.Close()
			}
			check("/dev/kvm is usable", err)
			b := guest.FindBinaries(a.dirs)
			for _, p := range []string{b.CloudHypervisor, b.Virtiofsd, b.Passt, b.Pasta, b.NFT} {
				_, err := os.Stat(p)
				if err == nil && !filepath.IsAbs(p) {
					err = fmt.Errorf("not found")
				}
				check(p, err)
			}
			_, err = nixbuild.New()
			check("nix", err)
			cfg, err := config.Load(a.dirs.GuestsConfigDir(), a.dirs.Home)
			if err == nil && cfg != nil && len(cfg.Errors) > 0 {
				var errMsgs []string
				for n, e := range cfg.Errors {
					errMsgs = append(errMsgs, fmt.Sprintf("%s.toml: %v", n, e))
				}
				err = errors.New(strings.Join(errMsgs, "; "))
			}
			check("config "+a.dirs.GuestsConfigDir(), err)
			if bad > 0 {
				return fmt.Errorf("%d check(s) failed", bad)
			}
			return nil
		},
	}
}

// updateCmd builds antic and cloud-hypervisor from GitHub (or --source) and
// installs them.
func (a *app) updateCmd() *cobra.Command {
	var source string
	c := &cobra.Command{
		Use:   "update",
		Short: "Build antic (and cloud-hypervisor) and install them",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			for _, dir := range []string{a.dirs.GuestsConfigDir(), a.dirs.StateRoot} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return err
				}
			}
			unlockUpdate, err := a.dirs.LockWithReason("update", "antic update")
			if err != nil {
				return err
			}
			defer unlockUpdate()

			unlockBuild, err := a.dirs.LockNixBuild()
			if err != nil {
				return err
			}
			defer unlockBuild()

			src := source
			if abs, err := filepath.Abs(src); err == nil {
				if _, serr := os.Stat(abs); serr == nil {
					src = abs
				}
			}
			nix, err := nixbuild.New()
			if err != nil {
				return err
			}
			anticOut, err := nix.Build(c.Context(), src, "antic")
			if err != nil {
				return err
			}
			anticDst := filepath.Join(a.dirs.BinDir, "antic")
			if err := install(nix.Real(anticOut)+"/bin/antic", anticDst); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "antic: installed %s\n", anticDst)

			gcRoot := filepath.Join(a.dirs.StateRoot, "libexec")
			libexecOut, err := nix.BuildLink(c.Context(), src, "libexec", gcRoot)
			if err != nil {
				return err
			}
			binDir := filepath.Join(nix.Real(libexecOut), "bin")
			for _, name := range []string{"cloud-hypervisor", "virtiofsd", "passt", "pasta", "passt.avx2", "pasta.avx2", "nft"} {
				sp := filepath.Join(binDir, name)
				if _, err := os.Stat(sp); err != nil {
					if strings.HasSuffix(name, ".avx2") {
						continue
					}
					return err
				}
				dp := filepath.Join(a.dirs.Libexec, name)
				if err := install(sp, dp); err != nil {
					return err
				}
				if !strings.HasSuffix(name, ".avx2") {
					fmt.Fprintf(os.Stderr, "antic: installed %s\n", dp)
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&source, "source", "github:RNG999/antic", "flake URI or local checkout path to build from")
	return c
}

func install(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".new")
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
