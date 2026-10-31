package guest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/host/chv"
	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/share"
	"github.com/RNG999/antic/internal/proto"
)

func (m *Manager) allSharePaths() []string {
	var out []string
	for _, c := range m.Config.Guests {
		for _, s := range c.Shares {
			out = append(out, s.Path)
		}
	}
	return out
}

// ShareInfo describes a share.
type ShareInfo struct {
	Path     string `json:"path"`
	ReadOnly bool   `json:"read_only"`
	Mounted  bool   `json:"mounted"`
}

// Shares lists the configured shares and the running guest's mounts, by
// resolved path: a share removed from the config that stays mounted until
// the guest restarts is listed as mounted.
func (g *Guest) Shares(ctx context.Context) []ShareInfo {
	byPath := map[string]ShareInfo{}
	for _, s := range g.Cfg.Shares {
		p := config.RealPath(s.Path)
		byPath[p] = ShareInfo{Path: p, ReadOnly: s.ReadOnly}
	}
	if g.Created {
		if bt, err := g.readBoot(ctx); err == nil {
			for _, m := range bt.Shares {
				info, ok := byPath[m.Path]
				if !ok {
					info = ShareInfo{Path: m.Path, ReadOnly: m.ReadOnly}
				}
				info.Mounted = true
				byPath[m.Path] = info
			}
		}
	}
	out := make([]ShareInfo, 0, len(byPath))
	for _, info := range byPath {
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b ShareInfo) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// AddShare records path in the guest's config file and, if the guest is
// running, hot-plugs it, atomically. It reports the resolved path and
// whether anything changed (sharing a directory again with the same mode
// changes nothing).
func (g *Guest) AddShare(ctx context.Context, path string, ro bool) (string, bool, error) {
	real, err := g.m.policy().Resolve(path)
	if err != nil {
		return "", false, err
	}
	absPath, err := domain.ParseAbsPath(real)
	if err != nil {
		return "", false, err
	}
	unlock, err := g.lockAndRecover(ctx)
	if err != nil {
		return "", false, err
	}
	defer unlock()

	draft, err := config.ReadDraft(g.Cfg, g.m.Dirs.Home)
	if err != nil {
		return "", false, err
	}
	if draft, err = draft.AddShare(real, ro); err != nil {
		return "", false, err
	}
	if !draft.Changed() {
		return real, false, nil
	}
	tx, bt := g.planChange(ctx, draft)
	tx.modes[absPath.Get()] = ro
	if bt != nil {
		mounted := false
		for _, s := range bt.Shares {
			switch {
			case s.Path == real && s.ReadOnly != ro:
				return "", false, fmt.Errorf("%s is still mounted %s until guest %s restarts", real, modeName(s.ReadOnly), g.Name())
			case s.Path == real:
				mounted = true
			case within(real, s.Path) || within(s.Path, real):
				return "", false, fmt.Errorf("%s overlaps mounted share %s", real, s.Path)
			}
		}
		if !mounted {
			tx.op.Change.Added, _ = pathSet([]string{real})
		}
	}
	if err := g.runChange(ctx, tx); err != nil {
		return "", false, err
	}
	return real, true, nil
}

func modeName(ro bool) string {
	if ro {
		return "read-only"
	}
	return "read-write"
}

func (g *Guest) hotplug(ctx context.Context, bt *boot, path string, ro bool) (err error) {
	tag := share.Tag(path)
	sock, err := g.startShare(ctx, tag, path, ro, guestUserID)
	if err != nil {
		return err
	}
	undo := []func(){func() { g.m.Units.Stop(context.WithoutCancel(ctx), g.shareUnit(tag)) }}
	defer func() {
		if err != nil {
			for i := len(undo) - 1; i >= 0; i-- {
				undo[i]()
			}
		}
	}()
	if err = g.api().AddFS(ctx, chv.FS{ID: "fs-" + tag, Tag: tag, Socket: sock, NumQueues: 1, QueueSize: 1024}); err != nil {
		return err
	}
	// Removing the device must finish before virtiofsd goes away, or
	// cloud-hypervisor treats the broken vhost-user socket as fatal.
	undo = append(undo, func() {
		g.api().RemoveDevice(context.WithoutCancel(ctx), "fs-"+tag)
		time.Sleep(2 * time.Second)
	})
	if err = g.vsock().Agent(ctx, proto.Request{Op: proto.OpMount, Tag: tag, Path: path, ReadOnly: ro}, nil); err != nil {
		return err
	}
	bt.Shares = append(bt.Shares, bootShare{Share: config.Share{Path: path, ReadOnly: ro}, Tag: tag, Runtime: true})
	return g.writeBoot(bt)
}

// RemoveShare removes a share from the guest's config file and, if it was
// hot-plugged on the running guest, unmounts and detaches it, atomically.
// It reports the resolved path, and whether the share stays mounted until
// the guest restarts (it was mounted at boot).
func (g *Guest) RemoveShare(ctx context.Context, path string) (string, bool, error) {
	real := config.RealPath(path)
	unlock, err := g.lockAndRecover(ctx)
	if err != nil {
		return "", false, err
	}
	defer unlock()

	draft, err := config.ReadDraft(g.Cfg, g.m.Dirs.Home)
	if err != nil {
		return "", false, err
	}
	if draft, err = draft.RemoveShare(real); err != nil {
		return "", false, err
	}
	tx, bt := g.planChange(ctx, draft)
	lingers := false
	if bt != nil {
		for _, s := range bt.Shares {
			if s.Path == real && s.Runtime {
				tx.op.Change.Removed, _ = pathSet([]string{real})
			} else if s.Path == real {
				lingers = true
			}
		}
	}
	if err := g.runChange(ctx, tx); err != nil {
		return "", false, err
	}
	return real, lingers, nil
}

func within(path, dir string) bool { return path == dir || strings.HasPrefix(path, dir+"/") }

// ensureMounted hot-plugs path unless the running guest already has it.
func (g *Guest) ensureMounted(ctx context.Context, path string, ro bool) error {
	bt, err := g.readBoot(ctx)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(bt.Shares, func(s bootShare) bool { return s.Path == path }) {
		return nil
	}
	return g.hotplug(ctx, bt, path, ro)
}

// ensureUnmounted detaches the hot-plugged share at path unless it is gone.
// It undoes its own partial work on failure.
func (g *Guest) ensureUnmounted(ctx context.Context, path string) error {
	bt, err := g.readBoot(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(bt.Shares, func(s bootShare) bool { return s.Path == path && s.Runtime })
	if i < 0 {
		return nil
	}
	s := bt.Shares[i]
	if err := g.vsock().Agent(ctx, proto.Request{Op: proto.OpUnmount, Tag: s.Tag}, nil); err != nil {
		return err
	}
	if err := g.api().RemoveDevice(ctx, "fs-"+s.Tag); err != nil {
		remount := g.vsock().Agent(context.WithoutCancel(ctx), proto.Request{Op: proto.OpMount, Tag: s.Tag, Path: s.Path, ReadOnly: s.ReadOnly}, nil)
		return errors.Join(err, remount)
	}
	time.Sleep(2 * time.Second) // see hotplug
	// The device is gone, so the share is unmounted whatever happens to its
	// virtiofsd; record that before stopping it.
	bt.Shares = slices.Delete(bt.Shares, i, i+1)
	if err := g.writeBoot(bt); err != nil {
		return err
	}
	if err := g.m.Units.Stop(ctx, g.shareUnit(s.Tag)); err != nil {
		g.m.logf("warning: %s is unmounted, but stopping its virtiofsd failed: %v", path, err)
	}
	return nil
}
