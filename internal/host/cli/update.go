package cli

import (
	"debug/elf"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/RNG999/antic/internal/host/nixbuild"
	"github.com/spf13/cobra"
)

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
				if err := runsHere(sp); err != nil {
					return fmt.Errorf("refusing to install %s: %w", name, err)
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

// runsHere checks that the executable at path can run on this host outside
// Nix: a dynamically linked one needs its program interpreter, which a
// helper linked against a Nix store does not find where that store is not
// at /nix (a rootless Nix).
func runsHere(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		b, err := io.ReadAll(p.Open())
		if err != nil {
			return err
		}
		interp := strings.TrimRight(string(b), "\x00")
		if _, err := os.Stat(interp); err != nil {
			return fmt.Errorf("it needs the program interpreter %s, which this host does not have (build it static)", interp)
		}
	}
	return nil
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
