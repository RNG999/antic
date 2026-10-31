package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/RNG999/antic/internal/host/guest"
)

func (a *app) guestCmd() *cobra.Command {
	c := &cobra.Command{Use: "guest", Short: "Manage guests and guest resources", Args: cobra.NoArgs}
	c.AddCommand(
		a.guestLsCmd(),
		a.guestCreateCmd(),
		a.guestRmCmd(),
		a.guestStartCmd(),
		a.guestStopCmd(),
		a.guestRestartCmd(),
		a.guestSetCmd(),
		a.guestEditCmd(),
		a.guestApplyCmd(),
		a.guestPsCmd(),
		a.guestKillCmd(),
		a.guestShowCmd(),
		a.guestLogsCmd(),
		a.guestInstallCmd(),
		a.guestUninstallCmd(),
		a.guestMountCmd(),
		a.guestUnmountCmd(),
		a.guestForwardCmd(),
		a.guestUnforwardCmd(),
		a.guestStatsCmd(),
	)
	return c
}

// eachGuest runs fn for the named guests, or every guest with all.
func (a *app) eachGuest(c *cobra.Command, args []string, all bool, fn func(*guest.Guest) error) error {
	if err := a.load(c.Context()); err != nil {
		return err
	}
	var gs []*guest.Guest
	if all {
		var err error
		if gs, err = a.mgr.All(); err != nil {
			return err
		}
	} else {
		if len(args) == 0 {
			names := a.cfg.Names()
			if len(names) == 1 {
				args = names
			} else if len(names) == 0 {
				return fmt.Errorf("no guests defined (run \"antic guest create <name>\")")
			} else {
				return fmt.Errorf("guest NAME is required\n\nAvailable guests: %s", strings.Join(names, ", "))
			}
		}
		for _, n := range args {
			g, err := a.mgr.Get(n)
			if err != nil {
				return err
			}
			gs = append(gs, g)
		}
	}
	// P2: stop at the first guest that fails; the ones before it stay done.
	for _, g := range gs {
		if err := fn(g); err != nil {
			return fmt.Errorf("%s: %w", g.Name(), err)
		}
	}
	return nil
}

func uptime(i guest.Info) string {
	if i.Since != nil {
		return age(time.Since(*i.Since))
	}
	return "-"
}

func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func shareMode(ro bool) string {
	if ro {
		return "ro"
	}
	return "rw"
}
