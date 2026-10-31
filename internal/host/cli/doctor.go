package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/guest"
	"github.com/RNG999/antic/internal/host/nixbuild"
	"github.com/spf13/cobra"
)

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
