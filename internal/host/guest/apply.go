package guest

import (
	"context"
	"fmt"
	"os"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/nixbuild"
	"github.com/RNG999/antic/internal/proto"
	"github.com/RNG999/antic/internal/tea/operation"
)

// bootSignature returns a deterministic signature of what a NixOS system
// boots with: its kernel, initrd and kernel parameters, read through real
// (the guest's store or the host's). A switch cannot change any of them.
func bootSignature(real func(string) string, system string) domain.KernelSig {
	if system == "" {
		return domain.NewKernelSig("")
	}
	kern, _ := os.Readlink(real(system + "/kernel"))
	initrd, _ := os.Readlink(real(system + "/initrd"))
	params, _ := os.ReadFile(real(system + "/kernel-params"))
	return domain.NewKernelSig(kern + "\x00" + initrd + "\x00" + string(params))
}

// prebuild builds the new system of a change to running guest g ahead of
// its saga (whose Build step then has nothing to do) and plans a restart
// instead of a switch when the new system boots differently.
func (g *Guest) prebuild(ctx context.Context, tx *txn, bt *boot) error {
	nix, err := g.m.nixBuilder()
	if err != nil {
		return err
	}
	if tx.newSystem, err = g.build(ctx, tx.draft.After); err != nil {
		return err
	}
	if bootSignature(g.store().Real, bt.System) != bootSignature(nix.Real, tx.newSystem) {
		g.m.logf("the new system boots another kernel; guest %s restarts to take it", g.Name())
		tx.op.Change.Restart = operation.Changed
		tx.op.Change.Added, tx.op.Change.Removed = operation.PathSet{}, operation.PathSet{}
	}
	return nil
}

// Apply builds the guest's system on the host, installs it into the guest's
// store and, if the guest is running, switches it there, atomically. A guest
// without an ID gets one (this is how a guest is first built).
func (g *Guest) Apply(ctx context.Context) error {
	return g.applyWith(ctx, func(d config.Draft) (config.Draft, error) { return d, nil })
}

// SetPackages changes the guest's packages and applies them, atomically.
func (g *Guest) SetPackages(ctx context.Context, pkgs []string) error {
	return g.applyWith(ctx, func(d config.Draft) (config.Draft, error) { return d.SetPackages(pkgs) })
}

// applyWith edits the guest's config under its lock and applies it.
func (g *Guest) applyWith(ctx context.Context, edit func(config.Draft) (config.Draft, error)) error {
	unlock, err := g.lockAndRecover(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	draft, err := config.ReadDraft(g.Cfg, g.m.Dirs.Home)
	if err != nil {
		return err
	}
	if draft, err = edit(draft); err != nil {
		return err
	}
	tx, bt := g.planChange(ctx, draft)
	tx.op.Change.System = operation.Changed
	if bt != nil && tx.op.Change.Restart == operation.Unchanged {
		if err := g.prebuild(ctx, tx, bt); err != nil {
			return err
		}
	}
	if err := g.runChange(ctx, tx); err != nil {
		return err
	}
	switch tx.op.Phase {
	case operation.GuestRunning:
	case operation.GuestAbsent, operation.GuestUncreated, operation.GuestOff:
		g.m.logf("%s is ready; it boots on the next \"antic guest start %s\"", tx.newSystem, g.Name())
	}
	return nil
}

// build builds the system of guest config c.
func (g *Guest) build(ctx context.Context, c *config.Guest) (string, error) {
	unlockBuild, err := g.m.Dirs.LockNixBuild()
	if err != nil {
		return "", err
	}
	defer unlockBuild()
	nix, err := g.m.nixBuilder()
	if err != nil {
		return "", err
	}
	if c.FlakePath != "" {
		if err := nixbuild.CheckLocks(c.FlakePath, g.m.allSharePaths()); err != nil {
			return "", err
		}
		g.m.logf("building %s#nixosConfigurations.%s...", c.FlakePath, c.FlakeAttr)
		return nix.BuildFlakeCached(ctx, g.m.Dirs.CacheDir(), c.FlakePath, c.FlakeAttr)
	}
	system, hit, err := nix.BuildBuiltinCached(ctx, g.m.Dirs.CacheDir(), g.m.User, c.Packages, func() {
		g.m.logf("building guest %s (%s)...", g.Name(), g.osDesc())
	})
	if err != nil {
		return "", err
	}
	if hit {
		g.m.logf("using cached guest %s (%s): %s", g.Name(), g.osDesc(), system)
	}
	return system, nil
}

// switchTo switches the running guest to system and records it as booted.
// Switching to the system it runs already does nothing.
func (g *Guest) switchTo(ctx context.Context, system string) error {
	bt, err := g.readBoot(ctx)
	if err != nil {
		return err
	}
	if bt.System == system {
		return nil
	}
	g.m.logf("switching the running guest to %s...", system)
	err = g.vsock().Agent(ctx, proto.Request{Op: proto.OpApply, Toplevel: system}, func(r proto.Response) {
		if r.Line != "" {
			fmt.Fprintln(g.m.Log, domain.Printable(r.Line))
		}
	})
	if err != nil {
		return err
	}
	bt.System = system
	return g.writeBoot(bt)
}
