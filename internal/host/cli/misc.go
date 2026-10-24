package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/guest"
	"github.com/RNG999/antic/internal/host/nixbuild"
	"github.com/RNG999/antic/internal/proto"
)

func (a *app) configCmd() *cobra.Command {
	c := &cobra.Command{Use: "config", Short: "antic.toml", Args: cobra.NoArgs}
	c.AddCommand(&cobra.Command{
		Use: "path", Short: "Print the config file's path", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { fmt.Println(a.dirs.ConfigFile()); return nil },
	}, &cobra.Command{
		Use: "check", Short: "Check the config file", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if _, err := config.Load(a.dirs.ConfigFile(), a.dirs.Home); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "antic: config ok")
			return nil
		},
	}, &cobra.Command{
		Use: "edit", Short: "Edit the config file with $EDITOR, then check it", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			editor := os.Getenv("EDITOR")
			if editor == "" {
				editor = "vi"
			}
			cmd := exec.Command(editor, a.dirs.ConfigFile())
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := cmd.Run(); err != nil {
				return err
			}
			_, err := config.Load(a.dirs.ConfigFile(), a.dirs.Home)
			return err
		},
	})
	return c
}

func (a *app) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use: "version", Short: "Print antic's version and guest protocol", Args: cobra.NoArgs,
		Run: func(*cobra.Command, []string) { fmt.Printf("antic %s (guest protocol %d)\n", Version, proto.Version) },
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
			for _, p := range []string{b.CloudHypervisor, b.Virtiofsd, b.Passt, b.Pasta, b.Nft} {
				_, err := os.Stat(p)
				if err == nil && !filepath.IsAbs(p) {
					err = fmt.Errorf("not found")
				}
				check(p, err)
			}
			_, err = nixbuild.New()
			check("nix", err)
			_, err = config.Load(a.dirs.ConfigFile(), a.dirs.Home)
			check("config "+a.dirs.ConfigFile(), err)
			if bad > 0 {
				return fmt.Errorf("%d check(s) failed", bad)
			}
			return nil
		},
	}
}

// upgradeCmd builds antic and cloud-hypervisor from a checkout and installs
// them.
func (a *app) upgradeCmd() *cobra.Command {
	var source string
	c := &cobra.Command{
		Use: "upgrade", Short: "Build antic (and cloud-hypervisor) from a checkout and install them", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			nix, err := nixbuild.New()
			if err != nil {
				return err
			}
			for _, t := range []struct{ attr, bin, dst string }{
				{"antic", "antic", filepath.Join(a.dirs.BinDir, "antic")},
				{"cloud-hypervisor", "cloud-hypervisor", filepath.Join(a.dirs.Libexec, "cloud-hypervisor")},
			} {
				out, err := nix.Build(c.Context(), source, t.attr)
				if err != nil {
					return err
				}
				if err := install(nix.Real(out)+"/bin/"+t.bin, t.dst); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "antic: installed %s\n", t.dst)
			}
			return nil
		},
	}
	c.Flags().StringVar(&source, "source", a.dirs.SrcDir, "antic checkout to build from")
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
