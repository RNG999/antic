package guest

import (
	"bytes"
	"context"
	"fmt"
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
	"github.com/RNG999/antic/internal/tea/lifecycle"
	"github.com/RNG999/antic/internal/tea/operation"
	"github.com/RNG999/antic/internal/types"
)

// lifecycleModel reconstructs the pure TEA lifecycle.Model from the host's
// current configuration, instance store, and boot state.
func (g *Guest) lifecycleModel(ctx context.Context) (lifecycle.Model, *boot) {
	name, _ := domain.ParseGuestName(g.Name())
	m := lifecycle.Init(name)
	var bt *boot
	if g.Created {
		installed, _ := g.store().Current()
		if instPath, err := domain.ParseStorePath(installed); err == nil {
			m.Installed = types.Some(types.NewIndexed[domain.StorePath, domain.InstalledState](instPath))
			if b, err := g.readBoot(ctx); err == nil {
				if bootSys, err := domain.ParseStorePath(b.System); err == nil {
					bt = b
					m.Boot = types.Some(lifecycle.BootedVM{
						System:    bootSys,
						KernelSig: g.kernelSignature(b.System),
					})
				}
			}
		}
	}

	byPath := map[string]lifecycle.Share{}
	for _, s := range g.Cfg.Shares {
		p := s.Path
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		absPath, err := domain.ParseAbsPath(p)
		if err != nil {
			continue
		}
		tag, err := domain.ParseShareTag(share.Tag(p))
		if err != nil {
			continue
		}
		byPath[p] = lifecycle.Share{
			Path:     absPath,
			Tag:      tag,
			Mount:    lifecycle.MountNone,
			ReadOnly: s.ReadOnly,
		}
	}
	if bt != nil {
		for _, bs := range bt.Shares {
			absPath, err := domain.ParseAbsPath(bs.Path)
			if err != nil {
				continue
			}
			rawTag := bs.Tag
			if rawTag == "" {
				rawTag = share.Tag(bs.Path)
			}
			tag, err := domain.ParseShareTag(rawTag)
			if err != nil {
				continue
			}
			sh, ok := byPath[bs.Path]
			if !ok {
				sh = lifecycle.Share{
					Path:     absPath,
					Tag:      tag,
					ReadOnly: bs.ReadOnly,
				}
			}
			if bs.Runtime {
				sh.Mount = lifecycle.MountRuntime
			} else {
				sh.Mount = lifecycle.MountBoot
			}
			byPath[bs.Path] = sh
		}
	}
	var shares []lifecycle.Share
	for _, sh := range byPath {
		shares = append(shares, sh)
	}
	m.Shares = types.BuildSet[lifecycle.Share, domain.AbsPath, lifecycle.ShareByPath](shares)
	return m, bt
}

// kernelSignature returns a deterministic signature of a NixOS system's kernel,
// initrd, and kernel-params so the pure TEA model can compare kernel identity.
func (g *Guest) kernelSignature(system string) domain.KernelSig {
	if system == "" {
		return domain.NewKernelSig("")
	}
	s := g.store()
	kern, _ := os.Readlink(s.Real(system + "/kernel"))
	initrd, _ := os.Readlink(s.Real(system + "/initrd"))
	params, _ := os.ReadFile(s.Real(system + "/kernel-params"))
	return domain.NewKernelSig(kern + "\x00" + initrd + "\x00" + string(params))
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
	tx, _ := g.planChange(ctx, draft)
	tx.op.Change.System = operation.Changed
	if err := g.commit(ctx, tx); err != nil {
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
			fmt.Fprintln(g.m.Log, r.Line)
		}
	})
	if err != nil {
		return err
	}
	if g.kernelSignature(bt.System) != g.kernelSignature(system) {
		g.m.logf("the kernel changed; restart the guest to boot it (antic guest restart %s)", g.Name())
	}
	bt.System = system
	return g.writeBoot(bt)
}

func (m *Manager) allSharePaths() []string {
	var out []string
	for _, c := range m.Config.Guests {
		for _, s := range c.Shares {
			out = append(out, s.Path)
		}
	}
	return out
}

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
		if draft, err = m.Config.DraftCreate(name, spec, m.Dirs.Home); err != nil {
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
	case draft.Absent && tx.oldProfile == "":
		tx.op.Phase = operation.GuestAbsent
	case tx.oldProfile == "":
		tx.op.Phase = operation.GuestUncreated
	}
	tx.op.Kind, tx.op.Change = operation.OpCreate, operation.Change{}
	if err := g.commit(ctx, tx); err != nil {
		return nil, err
	}
	if draft.Absent {
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
	draft, err := config.ReadDraft(g.Cfg, g.m.Dirs.Home)
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

// Dir returns the current directory, which must lie in one of the guest's
// shares (the guest sees them at the same path).
func (g *Guest) Dir(ctx context.Context) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(wd); err == nil {
		wd = real
	}
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
			return wd, nil
		}
	}
	return "", fmt.Errorf("%s is not inside a directory shared with guest %s (%s); see \"antic share add\"",
		wd, g.Name(), strings.Join(shares, ", "))
}

// Exec runs a command or shell in the guest (starting it if needed).
func (g *Guest) Exec(ctx context.Context, c transport.Cmd) (int, error) {
	if err := g.requireCreated(); err != nil {
		return 0, err
	}
	if err := g.EnsureRunning(ctx); err != nil {
		return 0, err
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
	return g.vsock().Exec(ctx, c)
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
func (g *Guest) Kill(ctx context.Context, session string) error {
	if _, err := g.readBoot(ctx); err != nil {
		return err
	}
	return g.vsock().Agent(ctx, proto.Request{Op: proto.OpKill, Session: session}, nil)
}

// ShareInfo describes a share.
type ShareInfo struct {
	Path     string `json:"path"`
	ReadOnly bool   `json:"read_only"`
	Mounted  bool   `json:"mounted"`
}

// Shares lists configured and runtime shares.
func (g *Guest) Shares(ctx context.Context) []ShareInfo {
	m, _ := g.lifecycleModel(ctx)
	var out []ShareInfo
	for _, s := range m.Shares.All() {
		out = append(out, ShareInfo{
			Path:     s.Path.Get(),
			ReadOnly: s.ReadOnly,
			Mounted:  s.Mounted(),
		})
	}
	return out
}

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
	if config.NeedsRestart(draft.Before, draft.After) {
		tx.op.Change.Restart = operation.Changed
	}
	return tx, bt
}

// commit runs the saga of tx and adopts the config it leaves.
func (g *Guest) commit(ctx context.Context, tx *txn) error {
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
	tx, _ := g.planChange(ctx, draft)
	return true, g.commit(ctx, tx)
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
				tx.modes[realPath(s.Path)] = s.ReadOnly
			}
		} else {
			tx.op.Change.Restart = operation.Changed
		}
	}
	return true, g.commit(ctx, tx)
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// liveShares returns the shares a running guest with boot record bt must
// hot-plug and detach to take the shares of c, or false if that needs a
// restart (a mounted share changes mode, or a new one overlaps a mount).
func liveShares(bt *boot, c *config.Guest) (added, removed []string, live bool) {
	want := map[string]bool{}
	for _, s := range c.Shares {
		want[realPath(s.Path)] = s.ReadOnly
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
	if err := g.commit(ctx, tx); err != nil {
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
	real := path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		real = r
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
	if err := g.commit(ctx, tx); err != nil {
		return "", false, err
	}
	return real, lingers, nil
}

func within(path, dir string) bool { return path == dir || strings.HasPrefix(path, dir+"/") }

func (g *Guest) osDesc() string {
	if g.Cfg.FlakePath != "" {
		return g.Cfg.FlakePath + "#" + g.Cfg.FlakeAttr
	}
	if len(g.Cfg.Packages) == 0 {
		return "builtin"
	}
	return "builtin [" + strings.Join(g.Cfg.Packages, " ") + "]"
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
		Name: g.Name(), ID: g.Inst.ID,
		Description: g.Cfg.Description, State: lifecycle.PhaseUncreated.String(),
		OS: g.osDesc(), Packages: g.Cfg.Packages,
		CPUs: g.Cfg.CPUs, Memory: g.Cfg.Memory, Network: g.Cfg.Network.Mode, Ports: g.Cfg.Ports,
		Shares: g.Shares(ctx),
	}
	if !g.Created {
		return i, nil
	}
	i.State = lifecycle.PhaseStopped.String()
	i.System, _ = g.store().Current()
	st, err := g.m.Units.State(ctx, g.vmUnit())
	if err != nil {
		return i, err
	}
	if st.Running() {
		i.State, i.Since = lifecycle.PhaseRunning.String(), &st.Since
		if bt, err := g.readBoot(ctx); err == nil {
			i.System = bt.System
		}
		i.Units = []string{g.vmUnit()}
		if g.Cfg.Network.Mode != config.NetworkNone {
			i.Units = append(i.Units, g.netUnit())
		}
		i.Units = append(i.Units, g.shareUnit("store"))
		for _, s := range i.Shares {
			if s.Mounted {
				i.Units = append(i.Units, g.shareUnit(share.Tag(s.Path)))
			}
		}
	}
	return i, nil
}

// Journal streams the running guest's journal (via antic-agent; root's view).
func (g *Guest) Journal(ctx context.Context, units []string, lines int, line func(string)) error {
	if _, err := g.readBoot(ctx); err != nil {
		return err
	}
	return g.vsock().Agent(ctx, proto.Request{Op: proto.OpJournal, Units: units, Lines: lines}, func(r proto.Response) {
		if r.Line != "" {
			line(r.Line)
		}
	})
}

// Console is the guest's console log.
func (g *Guest) Console() string { return g.Inst.Console() }
