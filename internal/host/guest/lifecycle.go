package guest

import (
	"context"
	"fmt"
	"time"

	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/instance"
	"github.com/RNG999/antic/internal/tea/operation"
)

// Create writes the guest's config file unless it has one, builds the
// guest, and starts it, atomically. Flags (a spec for the new file) are an
// error when the config exists already: they would be ignored (D3).
func (m *Manager) Create(ctx context.Context, name string, spec config.Guest, flags bool) (*Guest, error) {
	if !config.ValidName(name) {
		return nil, fmt.Errorf("invalid guest name %q (a-z, 0-9 and -, up to 32 characters)", name)
	}
	if err := m.RecoverOrphan(ctx, name); err != nil {
		return nil, err
	}
	var g *Guest
	var draft config.Draft
	if m.Config.Guests[name] != nil {
		if flags {
			return nil, fmt.Errorf("guest %s already has a config (%s); edit it instead of passing flags", name, m.Config.Guests[name].Path)
		}
		var err error
		if g, err = m.Get(name); err != nil {
			return nil, err
		}
		if draft, err = config.ReadDraft(g.Cfg, m.Dirs.Home); err != nil {
			return nil, err
		}
	} else {
		var err error
		if draft, err = m.Config.NewGuestDraft(name, spec, m.Dirs.Home); err != nil {
			return nil, err
		}
		inst, ok, err := m.Dirs.Lookup(name)
		if err != nil {
			return nil, err
		}
		g = &Guest{m: m, Cfg: draft.After, Inst: inst, Created: ok}
	}
	unlock, err := g.lockAndRecover(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	release, err := g.claimPorts(draft)
	if err != nil {
		return nil, err
	}
	defer release()
	tx, _ := g.planChange(ctx, draft)
	switch {
	case draft.NewFile && tx.oldProfile == "":
		tx.op.Phase = operation.GuestAbsent
	case tx.oldProfile == "":
		tx.op.Phase = operation.GuestUncreated
	}
	tx.op.Kind, tx.op.Change = operation.OpCreate, operation.Change{}
	if err := g.runChange(ctx, tx); err != nil {
		return nil, err
	}
	if draft.NewFile {
		m.logf("created %s", draft.Path)
	}
	return g, nil
}

// Remove deletes the guest: it stops it, moves its config file and state
// to the trash, and empties the trash once both moved, atomically.
func (g *Guest) Remove(ctx context.Context) error {
	unlock, err := g.lockAndRecover(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	// Removing needs the file's bytes (for the journal), not its meaning:
	// a guest whose config does not load can be removed too (D17).
	draft, err := config.ReadRawDraft(g.Cfg.Path, g.Name(), g.m.Dirs.Home)
	if err != nil {
		return err
	}
	tx, _ := g.planChange(ctx, draft)
	tx.op.Kind, tx.op.Change = operation.OpRemove, operation.Change{}
	if err := g.runSaga(ctx, tx, operation.MsgCommand{Op: tx.op}); err != nil {
		return err
	}
	delete(g.m.Config.Guests, g.Name())
	g.Inst, g.Created = instance.Instance{}, false
	return nil
}

// Start boots the guest unless it is running, and waits until it is ready.
func (g *Guest) Start(ctx context.Context) error {
	if err := g.requireCreated(); err != nil {
		return err
	}
	return g.runPower(ctx, operation.OpStart)
}

// Restart reboots the guest with its current config (or boots it).
func (g *Guest) Restart(ctx context.Context) error {
	if err := g.requireCreated(); err != nil {
		return err
	}
	return g.runPower(ctx, operation.OpRestart)
}

// runPower runs a start, stop or restart saga under the guest's lock.
func (g *Guest) runPower(ctx context.Context, kind operation.OpKind) error {
	unlock, err := g.lockAndRecover(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	draft, err := config.ReadDraft(g.Cfg, g.m.Dirs.Home)
	if err != nil {
		return err
	}
	tx, _ := g.planChange(ctx, draft)
	if tx.op.Phase == operation.GuestOff && tx.oldProfile == "" {
		tx.op.Phase = operation.GuestUncreated
	}
	tx.op.Kind, tx.op.Change = kind, operation.Change{}
	return g.runSaga(ctx, tx, operation.MsgCommand{Op: tx.op})
}

// EnsureRunning starts the guest if needed (for "run" and "shell").
func (g *Guest) EnsureRunning(ctx context.Context) error {
	if up, err := g.Running(ctx); err != nil || up {
		return err
	}
	g.m.logf("starting guest %s...", g.Name())
	return g.Start(ctx)
}

// Stop shuts the guest down (ACPI power button, then the units).
func (g *Guest) Stop(ctx context.Context) error {
	if !g.Created {
		return nil
	}
	return g.runPower(ctx, operation.OpStop)
}

// stopLocked shuts the guest down, if it runs, and stops its units.
func (g *Guest) stopLocked(ctx context.Context) error {
	up, err := g.Running(ctx)
	if err != nil {
		return err
	}
	if up {
		if err := g.api().PowerButton(ctx); err != nil {
			g.m.logf("power button for %s failed (%v); stopping its units", g.Name(), err)
		} else {
			for i := 0; i < 120; i++ {
				if up, err := g.Running(ctx); err != nil || !up {
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
		}
	}
	return g.m.Units.Stop(ctx, g.slice())
}
