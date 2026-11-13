package guest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/host/chv"
	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/instance"
	"github.com/RNG999/antic/internal/host/nixbuild"
	"github.com/RNG999/antic/internal/host/share"
	"github.com/RNG999/antic/internal/host/transport"
	"github.com/RNG999/antic/internal/proto"
	"github.com/RNG999/antic/internal/tea/operation"
)

type storeDirectory interface {
	Real(string) string
}

type bootIdentity struct {
	valid  bool
	kernel domain.AbsPath
	initrd domain.AbsPath
	params [32]byte
}

// bootSignature returns a deterministic identity of what a NixOS system
// boots with: its kernel, initrd and kernel parameters, read through store
// (the guest's store or the host's). A switch cannot change any of them.
func bootSignature(store storeDirectory, system domain.StorePath) bootIdentity {
	if !system.Valid() {
		return bootIdentity{}
	}
	rawKern, _ := os.Readlink(store.Real(system.Get() + "/kernel"))
	rawInitrd, _ := os.Readlink(store.Real(system.Get() + "/initrd"))
	params, _ := os.ReadFile(store.Real(system.Get() + "/kernel-params"))
	kern, _ := domain.ParseAbsPath(filepath.Clean(rawKern))
	initrd, _ := domain.ParseAbsPath(filepath.Clean(rawInitrd))
	return bootIdentity{
		valid:  true,
		kernel: kern,
		initrd: initrd,
		params: sha256.Sum256(params),
	}
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
	btSystem, _ := domain.ParseStorePath(bt.System)
	if bootSignature(g.store(), btSystem) != bootSignature(nix, tx.newSystem) {
		fmt.Fprintf(g.m.Log, "antic: the new system boots another kernel; guest %s restarts to take it\n", g.GuestName())
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
func (g *Guest) SetPackages(ctx context.Context, pkgs []domain.PackageName) error {
	rawPkgs := make([]string, len(pkgs))
	for i, p := range pkgs {
		rawPkgs[i] = p.Get()
	}
	return g.applyWith(ctx, func(d config.Draft) (config.Draft, error) { return d.SetPackages(rawPkgs) })
}

// applyWith edits the guest's config under its lock and applies it.
func (g *Guest) applyWith(ctx context.Context, edit func(config.Draft) (config.Draft, error)) error {
	unlock, err := g.lockAndRecover(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	draft, err := config.ReadDraft(g.Cfg, g.m.Dirs.Home.Get())
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
		fmt.Fprintf(g.m.Log, "antic: %s is ready; it boots on the next \"antic guest start %s\"\n", tx.newSystem, g.GuestName())
	}
	return nil
}

// build builds the system of guest config c.
func (g *Guest) build(ctx context.Context, c *config.Guest) (domain.StorePath, error) {
	unlockBuild, err := g.m.Dirs.LockNixBuild()
	if err != nil {
		return domain.StorePath{}, err
	}
	defer unlockBuild()
	nix, err := g.m.nixBuilder()
	if err != nil {
		return domain.StorePath{}, err
	}
	if c.FlakePath != "" {
		if err := nixbuild.CheckLocks(c.FlakePath, g.m.allSharePaths()); err != nil {
			return domain.StorePath{}, err
		}
		fmt.Fprintf(g.m.Log, "antic: building %s#nixosConfigurations.%s...\n", c.FlakePath, c.FlakeAttr)
		rawSys, err := nix.BuildFlakeCached(ctx, g.m.Dirs.CacheDir().Get(), c.FlakePath, c.FlakeAttr)
		if err != nil {
			return domain.StorePath{}, err
		}
		return domain.ParseStorePath(rawSys)
	}
	rawSys, hit, err := nix.BuildBuiltinCached(ctx, g.m.Dirs.CacheDir().Get(), g.m.User.Get(), c.Packages, func() {
		fmt.Fprintf(g.m.Log, "antic: building guest %s (%s)...\n", g.GuestName(), g.osDesc())
	})
	if err != nil {
		return domain.StorePath{}, err
	}
	if hit {
		fmt.Fprintf(g.m.Log, "antic: using cached guest %s (%s): %s\n", g.GuestName(), g.osDesc(), rawSys)
	}
	return domain.ParseStorePath(rawSys)
}

// switchTo switches the running guest to system and records it as booted.
// Switching to the system it runs already does nothing.
func (g *Guest) switchTo(ctx context.Context, system domain.StorePath) error {
	bt, err := g.readBoot(ctx)
	if err != nil {
		return err
	}
	if bt.System == system.Get() {
		return nil
	}
	fmt.Fprintf(g.m.Log, "antic: switching the running guest to %s...\n", system)
	err = g.vsock().Agent(ctx, proto.Request{Op: proto.OpApply, Toplevel: system.Get()}, func(r proto.Response) {
		if r.Line != "" {
			fmt.Fprintln(g.m.Log, domain.Printable(r.Line))
		}
	})
	if err != nil {
		return err
	}
	bt.System = system.Get()
	return g.writeBoot(bt)
}

func (m *Manager) allSharePaths() []domain.AbsPath {
	var out []domain.AbsPath
	for _, c := range m.Config.Guests {
		for _, s := range c.Shares {
			if abs, err := domain.ParseAbsPath(config.RealPath(s.Path)); err == nil {
				out = append(out, abs)
			}
		}
	}
	return out
}

// Create writes the guest's config file unless it has one, builds the
// guest, and starts it, atomically. Flags (a spec for the new file) are an
// error when the config exists already: they would be ignored (D3).
func (m *Manager) Create(ctx context.Context, name domain.GuestName, spec config.Guest, flags bool) (*Guest, error) {
	if err := m.RecoverOrphan(ctx, name); err != nil {
		return nil, err
	}
	rawName := name.Get()
	var g *Guest
	var draft config.Draft
	if m.Config.Guests[rawName] != nil {
		if flags {
			return nil, fmt.Errorf("guest %s already has a config (%s); edit it instead of passing flags", name, m.Config.Guests[rawName].Path)
		}
		var err error
		if g, err = m.Get(name); err != nil {
			return nil, err
		}
		if draft, err = config.ReadDraft(g.Cfg, m.Dirs.Home.Get()); err != nil {
			return nil, err
		}
	} else {
		var err error
		if draft, err = m.Config.NewGuestDraft(rawName, spec, m.Dirs.Home.Get()); err != nil {
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
	tx, _ := g.planChange(ctx, draft)
	switch {
	case draft.NewFile && !tx.oldProfile.Valid():
		tx.op.Phase = operation.GuestAbsent
	case !tx.oldProfile.Valid():
		tx.op.Phase = operation.GuestUncreated
	}
	tx.op.Kind, tx.op.Change = operation.OpCreate, operation.Change{}
	if err := g.runChange(ctx, tx); err != nil {
		return nil, err
	}
	if draft.NewFile {
		fmt.Fprintf(m.Log, "antic: created %s\n", draft.Path)
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
	draft, err := config.ReadRawDraft(g.Cfg.Path, g.GuestName().Get(), g.m.Dirs.Home.Get())
	if err != nil {
		return err
	}
	tx, _ := g.planChange(ctx, draft)
	tx.op.Kind, tx.op.Change = operation.OpRemove, operation.Change{}
	if err := g.runSaga(ctx, tx, operation.MsgCommand{Op: tx.op}); err != nil {
		return err
	}
	delete(g.m.Config.Guests, g.GuestName().Get())
	g.Inst, g.Created = instance.Instance{}, false
	return nil
}

// Dir returns the current directory, which must lie in one of the guest's
// shares (the guest sees them at the same path).
func (g *Guest) Dir(ctx context.Context) (domain.AbsPath, error) {
	wd, err := os.Getwd()
	if err != nil {
		return domain.AbsPath{}, err
	}
	wd = config.RealPath(wd)
	var shares []string
	if bt, err := g.readBoot(ctx); err == nil {
		for _, s := range bt.Shares {
			shares = append(shares, s.Path)
		}
	} else {
		for _, s := range g.Cfg.Shares {
			if p, err := filepath.EvalSymlinks(s.Path); err == nil {
				shares = append(shares, p)
			}
		}
	}
	for _, s := range shares {
		if wd == s || strings.HasPrefix(wd, s+"/") {
			return domain.ParseAbsPath(wd)
		}
	}
	return domain.AbsPath{}, fmt.Errorf("%s is not inside a directory shared with guest %s (%s); see \"antic guest mount\"",
		wd, g.GuestName(), strings.Join(shares, ", "))
}

// Exec runs a command or shell in the guest (starting it if needed).
func (g *Guest) Exec(ctx context.Context, c transport.Cmd) (domain.ExitCode, error) {
	if err := g.requireCreated(); err != nil {
		return domain.ExitCode{}, err
	}
	if err := g.EnsureRunning(ctx); err != nil {
		return domain.ExitCode{}, err
	}
	if len(g.Cfg.Env) > 0 {
		var envKeys []string
		for k := range g.Cfg.Env {
			envKeys = append(envKeys, k)
		}
		slices.Sort(envKeys)
		for _, k := range envKeys {
			c.Env = append(c.Env, k+"="+g.Cfg.Env[k])
		}
	}
	rawCode, err := g.vsock().Exec(ctx, c)
	if err != nil {
		return domain.ExitCode{}, err
	}
	return domain.ParseExitCode(rawCode)
}

// Sessions lists the guest's running sessions.
func (g *Guest) Sessions(ctx context.Context) ([]proto.Session, error) {
	if _, err := g.readBoot(ctx); err != nil {
		return nil, err
	}
	var out []proto.Session
	err := g.vsock().Agent(ctx, proto.Request{Op: proto.OpSessions}, func(r proto.Response) {
		if r.Sessions != nil {
			out = r.Sessions
		}
	})
	return out, err
}

// Kill stops a session.
func (g *Guest) Kill(ctx context.Context, session domain.SessionUnit) error {
	if _, err := g.readBoot(ctx); err != nil {
		return err
	}
	return g.vsock().Agent(ctx, proto.Request{Op: proto.OpKill, Session: session.Get()}, nil)
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

// planChange plans the operation that makes draft take effect on g, with
// the concrete old artifacts it needs, and returns the running guest's boot
// record (nil when it is not running). It must run under the guest's lock.
func (g *Guest) planChange(ctx context.Context, draft config.Draft) (*txn, *boot) {
	tx := &txn{draft: draft, modes: map[domain.AbsPath]bool{},
		op: operation.Op{Kind: operation.OpChangeConfig, Phase: operation.GuestUncreated}}
	if g.Created {
		tx.op.Phase = operation.GuestOff
		if rawProf, err := g.store().Current(); err == nil && rawProf != "" {
			tx.oldProfile, _ = domain.ParseStorePath(rawProf)
		}
	}
	if draft.Changed() {
		tx.op.Change.Config = operation.Changed
	}
	bt, err := g.readBoot(ctx)
	if err != nil {
		return tx, nil
	}
	tx.op.Phase = operation.GuestRunning
	tx.oldBooted, _ = domain.ParseStorePath(bt.System)
	var runtime []domain.AbsPath
	for _, s := range bt.Shares {
		if abs, err := domain.ParseAbsPath(s.Path); err == nil {
			tx.modes[abs] = s.ReadOnly
			if s.Runtime {
				runtime = append(runtime, abs)
			}
		}
	}
	tx.op.Runtime = buildPathSet(runtime...)
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
	g.m.Config.Guests[g.GuestName().Get()] = g.Cfg
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
	draft, err := config.ReadDraft(g.Cfg, g.m.Dirs.Home.Get())
	if err != nil {
		return false, err
	}
	if draft, err = edit(draft); err != nil {
		return false, err
	}
	if !draft.Changed() {
		return false, nil
	}
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
	draft, err := config.ReadDraft(g.Cfg, g.m.Dirs.Home.Get())
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
	tx, bt := g.planChange(ctx, draft)
	if tx.op.Phase != operation.GuestUncreated {
		tx.op.Change.System = operation.Changed
	}
	if bt != nil && tx.op.Change.Restart == operation.Unchanged {
		added, removed, live := liveShares(bt, draft.After)
		if live {
			tx.op.Change.Added = buildPathSet(added...)
			tx.op.Change.Removed = buildPathSet(removed...)
			for _, s := range draft.After.Shares {
				if abs, err := domain.ParseAbsPath(config.RealPath(s.Path)); err == nil {
					tx.modes[abs] = s.ReadOnly
				}
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
func liveShares(bt *boot, c *config.Guest) (added, removed []domain.AbsPath, live bool) {
	want := map[domain.AbsPath]bool{}
	for _, s := range c.Shares {
		if abs, err := domain.ParseAbsPath(config.RealPath(s.Path)); err == nil {
			want[abs] = s.ReadOnly
		}
	}
	var mounts []bootShare
	for _, m := range bt.Shares {
		mAbs, err := domain.ParseAbsPath(m.Path)
		if err != nil {
			continue
		}
		if _, keep := want[mAbs]; !keep && m.Runtime {
			removed = append(removed, mAbs)
			continue
		}
		mounts = append(mounts, m)
	}
	for p, ro := range want {
		i := slices.IndexFunc(mounts, func(m bootShare) bool { return m.Path == p.Get() })
		if i >= 0 {
			if mounts[i].ReadOnly != ro {
				return nil, nil, false
			}
			continue
		}
		if slices.ContainsFunc(mounts, func(m bootShare) bool {
			mAbs, err := domain.ParseAbsPath(m.Path)
			return err == nil && domain.PathsOverlap(p, mAbs)
		}) {
			return nil, nil, false
		}
		added = append(added, p)
	}
	return added, removed, true
}

// OtherPorts are the port forwards configured on every other guest.
func (g *Guest) OtherPorts() []config.Port {
	var out []config.Port
	for name, c := range g.m.Config.Guests {
		if name != g.GuestName().Get() {
			out = append(out, c.Ports...)
		}
	}
	return out
}

// AddShare records targetDir in the guest's config file and, if the guest is
// running, hot-plugs it, atomically. It reports the canonical path and
// whether anything changed (sharing a directory again with the same mode
// changes nothing).
func (g *Guest) AddShare(ctx context.Context, targetDir domain.AbsPath, ro bool) (domain.AbsPath, bool, error) {
	absPath, err := g.m.policy().ValidateShareDirectory(targetDir)
	if err != nil {
		return domain.AbsPath{}, false, err
	}
	real := absPath.Get()
	unlock, err := g.lockAndRecover(ctx)
	if err != nil {
		return domain.AbsPath{}, false, err
	}
	defer unlock()

	draft, err := config.ReadDraft(g.Cfg, g.m.Dirs.Home.Get())
	if err != nil {
		return domain.AbsPath{}, false, err
	}
	if draft, err = draft.AddShare(real, ro); err != nil {
		return domain.AbsPath{}, false, err
	}
	if !draft.Changed() {
		return absPath, false, nil
	}
	tx, bt := g.planChange(ctx, draft)
	tx.modes[absPath] = ro
	if bt != nil {
		mounted := false
		for _, s := range bt.Shares {
			sAbs, _ := domain.ParseAbsPath(s.Path)
			switch {
			case s.Path == real && s.ReadOnly != ro:
				modeLabel := "read-write"
				if s.ReadOnly {
					modeLabel = "read-only"
				}
				return domain.AbsPath{}, false, fmt.Errorf("%s is still mounted %s until guest %s restarts", real, modeLabel, g.GuestName())
			case s.Path == real:
				mounted = true
			case domain.PathsOverlap(absPath, sAbs):
				return domain.AbsPath{}, false, fmt.Errorf("%s overlaps mounted share %s", real, s.Path)
			}
		}
		if !mounted {
			tx.op.Change.Added = buildPathSet(absPath)
		}
	}
	if err := g.runChange(ctx, tx); err != nil {
		return domain.AbsPath{}, false, err
	}
	return absPath, true, nil
}

func (g *Guest) hotplug(ctx context.Context, bt *boot, path domain.AbsPath, ro bool) (err error) {
	tag := share.Tag(path)
	sock, err := g.startShare(ctx, g.shareUnit(tag), g.shareSockDir(tag), path, ro, guestUserUID)
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
	if err = g.api().AddFS(ctx, chv.FS{ID: "fs-" + tag.Get(), Tag: tag.Get(), Socket: sock.Get(), NumQueues: 1, QueueSize: 1024}); err != nil {
		return err
	}
	// Removing the device must finish before virtiofsd goes away, or
	// cloud-hypervisor treats the broken vhost-user socket as fatal.
	undo = append(undo, func() {
		g.api().RemoveDevice(context.WithoutCancel(ctx), "fs-"+tag.Get())
		time.Sleep(2 * time.Second)
	})
	if err = g.vsock().Agent(ctx, proto.Request{Op: proto.OpMount, Tag: tag.Get(), Path: path.Get(), ReadOnly: ro}, nil); err != nil {
		return err
	}
	bt.Shares = append(bt.Shares, bootShare{Share: config.Share{Path: path.Get(), ReadOnly: ro}, Tag: tag.Get(), Runtime: true})
	return g.writeBoot(bt)
}

// RemoveShare removes a share from the guest's config file and, if it was
// hot-plugged on the running guest, unmounts and detaches it, atomically.
// It reports the canonical path, and whether the share stays mounted until
// the guest restarts (it was mounted at boot).
func (g *Guest) RemoveShare(ctx context.Context, targetDir domain.AbsPath) (domain.AbsPath, bool, error) {
	real := config.RealPath(targetDir.Get())
	absPath, err := domain.ParseAbsPath(filepath.Clean(real))
	if err != nil {
		return domain.AbsPath{}, false, err
	}
	unlock, err := g.lockAndRecover(ctx)
	if err != nil {
		return domain.AbsPath{}, false, err
	}
	defer unlock()

	draft, err := config.ReadDraft(g.Cfg, g.m.Dirs.Home.Get())
	if err != nil {
		return domain.AbsPath{}, false, err
	}
	if draft, err = draft.RemoveShare(absPath.Get()); err != nil {
		return domain.AbsPath{}, false, err
	}
	tx, bt := g.planChange(ctx, draft)
	lingers := false
	if bt != nil {
		for _, s := range bt.Shares {
			if s.Path == absPath.Get() && s.Runtime {
				tx.op.Change.Removed = buildPathSet(absPath)
			} else if s.Path == absPath.Get() {
				lingers = true
			}
		}
	}
	if err := g.runChange(ctx, tx); err != nil {
		return domain.AbsPath{}, false, err
	}
	return absPath, lingers, nil
}

func (g *Guest) osDesc() domain.PrintableText {
	raw := "builtin"
	if g.Cfg.FlakePath != "" {
		raw = g.Cfg.FlakePath + "#" + g.Cfg.FlakeAttr
	} else if len(g.Cfg.Packages) > 0 {
		raw = "builtin [" + strings.Join(g.Cfg.Packages, " ") + "]"
	}
	desc, _ := domain.ParsePrintableText(domain.Printable(raw))
	return desc
}

// Info describes a guest for "guest ls" and "guest show".
type Info struct {
	Name        string        `json:"name"`
	ID          string        `json:"id,omitempty"`
	Description string        `json:"description,omitempty"`
	State       string        `json:"state"` // -, stopped, running
	Since       *time.Time    `json:"since,omitempty"`
	OS          string        `json:"os"` // builtin [...] or flake#attr
	Packages    []string      `json:"packages,omitempty"`
	System      string        `json:"system,omitempty"`
	CPUs        int           `json:"cpus"`
	Memory      string        `json:"memory"`
	Network     string        `json:"network"`
	Ports       []config.Port `json:"ports,omitempty"`
	Shares      []ShareInfo   `json:"shares"`
	Units       []string      `json:"units,omitempty"`
}

// Inspect returns the guest's state.
func (g *Guest) Inspect(ctx context.Context) (Info, error) {
	i := Info{
		Name: g.GuestName().Get(), ID: g.Inst.ID.Get(),
		Description: g.Cfg.Description, State: "-",
		OS: g.osDesc().Get(), Packages: g.Cfg.Packages,
		CPUs: g.Cfg.CPUs, Memory: g.Cfg.Memory, Network: g.Cfg.Network.Mode, Ports: g.Cfg.Ports,
		Shares: g.Shares(ctx),
	}
	if !g.Created {
		return i, nil
	}
	i.State = "stopped"
	i.System, _ = g.store().Current()
	st, err := g.m.Units.State(ctx, g.vmUnit())
	if err != nil {
		return i, err
	}
	if st.Running() {
		i.State, i.Since = "running", &st.Since
		if bt, err := g.readBoot(ctx); err == nil {
			i.System = bt.System
		}
		i.Units = []string{g.vmUnit().Get()}
		if g.Cfg.Network.Mode != config.NetworkNone {
			i.Units = append(i.Units, g.netUnit().Get())
		}
		i.Units = append(i.Units, g.storeShareUnit().Get())
		for _, s := range i.Shares {
			if s.Mounted {
				if absShare, err := domain.ParseAbsPath(s.Path); err == nil {
					i.Units = append(i.Units, g.shareUnit(share.Tag(absShare)).Get())
				}
			}
		}
	}
	return i, nil
}

// Journal streams the running guest's journal (via antic-agent; root's view).
func (g *Guest) Journal(ctx context.Context, units []domain.UnitName, lines domain.JournalLines, out io.Writer) error {
	if _, err := g.readBoot(ctx); err != nil {
		return err
	}
	rawUnits := make([]string, len(units))
	for i, u := range units {
		rawUnits[i] = u.Get()
	}
	return g.vsock().Agent(ctx, proto.Request{Op: proto.OpJournal, Units: rawUnits, Lines: lines.Get()}, func(r proto.Response) {
		if r.Line != "" {
			fmt.Fprintln(out, domain.Printable(r.Line))
		}
	})
}

// Console is the guest's console log.
func (g *Guest) Console() domain.AbsPath { return g.Inst.Console() }
