package guest

import (
	"bytes"
	"context"
	"fmt"
	"slices"

	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/tea/operation"
)

// planChange plans the operation that makes draft take effect on g, with
// the concrete old artifacts it needs, and returns the running guest's boot
// record (nil when it is not running). It must run under the guest's lock.
func (g *Guest) planChange(ctx context.Context, draft config.Draft) (*txn, *boot) {
	tx := &txn{draft: draft, modes: map[string]bool{},
		op: operation.Op{Kind: operation.OpChangeConfig, Phase: operation.GuestUncreated}}
	if g.Created {
		tx.op.Phase = operation.GuestOff
		tx.oldProfile, _ = g.store().Current()
	}
	if draft.Changed() {
		tx.op.Change.Config = operation.Changed
	}
	bt, err := g.readBoot(ctx)
	if err != nil {
		return tx, nil
	}
	tx.op.Phase, tx.oldBooted = operation.GuestRunning, bt.System
	var runtime []string
	for _, s := range bt.Shares {
		tx.modes[s.Path] = s.ReadOnly
		if s.Runtime {
			runtime = append(runtime, s.Path)
		}
	}
	tx.op.Runtime, _ = pathSet(runtime)
	if draft.Before != nil && draft.After != nil && config.NeedsRestart(draft.Before, draft.After) {
		tx.op.Change.Restart = operation.Changed
	}
	return tx, bt
}

// runChange runs the saga of tx and adopts the config it leaves.
func (g *Guest) runChange(ctx context.Context, tx *txn) error {
	err := g.runSaga(ctx, tx, operation.MsgCommand{Op: tx.op})
	g.Cfg = tx.draft.After
	if err != nil {
		g.Cfg = tx.draft.Before
	}
	g.m.Config.Guests[g.Name()] = g.Cfg
	return err
}

// Reconfigure edits the guest's config under its lock and makes it take
// effect, restarting a running guest when the change needs it, atomically.
// It reports whether anything changed.
func (g *Guest) Reconfigure(ctx context.Context, edit func(config.Draft) (config.Draft, error)) (bool, error) {
	unlock, err := g.lockAndRecover(ctx)
	if err != nil {
		return false, err
	}
	defer unlock()
	draft, err := config.ReadDraft(g.Cfg, g.m.Dirs.Home)
	if err != nil {
		return false, err
	}
	if draft, err = edit(draft); err != nil {
		return false, err
	}
	if !draft.Changed() {
		return false, nil
	}
	release, err := g.claimPorts(draft)
	if err != nil {
		return false, err
	}
	defer release()
	tx, _ := g.planChange(ctx, draft)
	return true, g.runChange(ctx, tx)
}

// Edit replaces the guest's config file with content, provided it still
// holds old (what the user started editing from), and makes it take effect
// as the specification's edit does: an unbuilt guest only changes its
// config, a stopped one is rebuilt, and a running one is rebuilt and
// switched with its shares hot-plugged, or restarted when the change
// cannot be made live. A content that does not load changes nothing.
func (g *Guest) Edit(ctx context.Context, old, content []byte) (bool, error) {
	unlock, err := g.lockAndRecover(ctx)
	if err != nil {
		return false, err
	}
	defer unlock()
	draft, err := config.ReadDraft(g.Cfg, g.m.Dirs.Home)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(draft.Old, old) {
		return false, fmt.Errorf("%s changed while it was being edited; edit it again", draft.Path)
	}
	if draft, err = draft.Replace(content); err != nil {
		return false, err
	}
	if !draft.Changed() {
		return false, nil
	}
	release, err := g.claimPorts(draft)
	if err != nil {
		return false, err
	}
	defer release()
	tx, bt := g.planChange(ctx, draft)
	if tx.op.Phase != operation.GuestUncreated {
		tx.op.Change.System = operation.Changed
	}
	if bt != nil && tx.op.Change.Restart == operation.Unchanged {
		added, removed, live := liveShares(bt, draft.After)
		if live {
			tx.op.Change.Added, _ = pathSet(added)
			tx.op.Change.Removed, _ = pathSet(removed)
			for _, s := range draft.After.Shares {
				tx.modes[config.RealPath(s.Path)] = s.ReadOnly
			}
		} else {
			tx.op.Change.Restart = operation.Changed
		}
	}
	if bt != nil && tx.op.Change.Restart == operation.Unchanged {
		if err := g.prebuild(ctx, tx, bt); err != nil {
			return false, err
		}
	}
	return true, g.runChange(ctx, tx)
}

// liveShares returns the shares a running guest with boot record bt must
// hot-plug and detach to take the shares of c, or false if that needs a
// restart (a mounted share changes mode, or a new one overlaps a mount).
func liveShares(bt *boot, c *config.Guest) (added, removed []string, live bool) {
	want := map[string]bool{}
	for _, s := range c.Shares {
		want[config.RealPath(s.Path)] = s.ReadOnly
	}
	var mounts []bootShare
	for _, m := range bt.Shares {
		if _, keep := want[m.Path]; !keep && m.Runtime {
			removed = append(removed, m.Path)
			continue
		}
		mounts = append(mounts, m)
	}
	for p, ro := range want {
		i := slices.IndexFunc(mounts, func(m bootShare) bool { return m.Path == p })
		if i >= 0 {
			if mounts[i].ReadOnly != ro {
				return nil, nil, false
			}
			continue
		}
		if slices.ContainsFunc(mounts, func(m bootShare) bool { return within(p, m.Path) || within(m.Path, p) }) {
			return nil, nil, false
		}
		added = append(added, p)
	}
	return added, removed, true
}

// claimPorts checks the host ports draft gives g that it did not have
// against every other guest's config as it is on disk now (D25), and holds
// the host-wide ports lock until the returned release, so that two guests
// cannot claim the same port at once (D26). It takes the lock only for a
// change that adds ports, and always after the guest's own lock.
func (g *Guest) claimPorts(draft config.Draft) (func(), error) {
	added := draft.NewPorts()
	if len(added) == 0 {
		return func() {}, nil
	}
	release, err := g.m.Dirs.LockWithReason("ports", "host port allocation")
	if err != nil {
		return nil, err
	}
	taken := map[string][]config.Port{}
	for name, c := range g.m.Config.Guests {
		taken["guest "+name] = c.Ports
	}
	now, err := config.Load(g.m.Config.Dir(), g.m.Dirs.Home)
	if err != nil {
		release()
		return nil, err
	}
	for name, c := range now.Guests {
		taken["guest "+name] = c.Ports
	}
	delete(taken, "guest "+g.Name())
	if err := config.PortsConflict(added, taken); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// OtherPorts are the port forwards configured on every other guest.
func (g *Guest) OtherPorts() []config.Port {
	var out []config.Port
	for name, c := range g.m.Config.Guests {
		if name != g.Name() {
			out = append(out, c.Ports...)
		}
	}
	return out
}
