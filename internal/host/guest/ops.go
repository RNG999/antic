package guest

import (
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
// store and, if the guest is running, switches it there. A guest without an
// ID gets one (this is how a guest is first built).
func (g *Guest) Apply(ctx context.Context) error {
	unlock, err := g.lock()
	if err != nil {
		return err
	}
	defer unlock()

	unlockBuild, err := g.m.Dirs.LockNixBuild()
	if err != nil {
		return err
	}
	defer unlockBuild()

	nix, err := g.m.nixBuilder()
	if err != nil {
		return err
	}
	var system string
	if g.Cfg.FlakePath != "" {
		if err := nixbuild.CheckLocks(g.Cfg.FlakePath, g.m.allSharePaths()); err != nil {
			return err
		}
		g.m.logf("building %s#nixosConfigurations.%s...", g.Cfg.FlakePath, g.Cfg.FlakeAttr)
		system, err = nix.BuildFlakeCached(ctx, g.m.Dirs.CacheDir(), g.Cfg.FlakePath, g.Cfg.FlakeAttr)
		if err != nil {
			return err
		}
	} else {
		var hit bool
		system, hit, err = nix.BuildBuiltinCached(ctx, g.m.Dirs.CacheDir(), g.m.User, g.Cfg.Packages, func() {
			g.m.logf("building guest %s (%s)...", g.Name(), g.osDesc())
		})
		if err != nil {
			return err
		}
		if hit {
			g.m.logf("using cached guest %s (%s): %s", g.Name(), g.osDesc(), system)
		}
	}

	sysPath, err := domain.ParseStorePath(system)
	if err != nil {
		return err
	}
	m, bt := g.lifecycleModel(ctx)
	m, effects := lifecycle.Update(m, lifecycle.MsgApplyBuilt{
		System:    sysPath,
		KernelSig: g.kernelSignature(system),
	})

	for _, eff := range effects {
		switch e := eff.(type) {
		case lifecycle.EffectAssignInstance:
			inst, err := g.m.Dirs.Assign(e.Guest.Get())
			if err != nil {
				return err
			}
			g.Inst, g.Created = inst, true
			g.m.logf("guest %s has ID %s", g.Name(), inst.ID)
			_ = os.WriteFile(g.Inst.OriginFile(), []byte(g.Cfg.Path+"\n"), 0o600)

		case lifecycle.EffectInstallStore:
			_ = os.WriteFile(g.Inst.OriginFile(), []byte(g.Cfg.Path+"\n"), 0o600)
			g.m.logf("installing %s into the guest store...", e.System.Get().Get())
			if err := nix.Install(ctx, g.store(), e.System.Get().Get()); err != nil {
				return err
			}

		case lifecycle.EffectLog:
			g.m.logf("%s", e.Message.Get())

		case lifecycle.EffectAgentSwitch:
			if err := g.execSwitch(ctx, m, bt, e.Toplevel); err != nil {
				return err
			}
		}
	}
	if g.Created {
		_ = os.WriteFile(g.Inst.OriginFile(), []byte(g.Cfg.Path+"\n"), 0o600)
	}
	return nil
}

func (g *Guest) execSwitch(ctx context.Context, m lifecycle.Model, bt *boot, system domain.StorePath) error {
	var oldSystem string
	if bvm, ok := m.Boot.Get(); ok {
		oldSystem = bvm.System.Get()
	}
	g.m.logf("switching the running guest...")
	err := g.vsock().Agent(ctx, proto.Request{Op: proto.OpApply, Toplevel: system.Get()}, func(r proto.Response) {
		if r.Line != "" {
			fmt.Fprintln(g.m.Log, r.Line)
		}
	})
	if err != nil {
		_, rbEffects := lifecycle.Update(m, lifecycle.MsgSwitchFailed{Reason: domain.NewText(err.Error())})
		for _, eff := range rbEffects {
			sw, ok := eff.(lifecycle.EffectAgentSwitch)
			if !ok || sw.Mode != lifecycle.SwitchRollback || !sw.Toplevel.Valid() || sw.Toplevel == system {
				continue
			}
			g.m.logf("switch failed (%v); rolling back to %s...", err, sw.Toplevel.Get())
			rbCtx, rbCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer rbCancel()
			rbErr := g.vsock().Agent(rbCtx, proto.Request{Op: proto.OpApply, Toplevel: sw.Toplevel.Get()}, func(r proto.Response) {
				if r.Line != "" {
					fmt.Fprintln(g.m.Log, r.Line)
				}
			})
			if rbErr != nil {
				return fmt.Errorf("switch failed: %w (rollback to %s also failed: %v)", err, sw.Toplevel.Get(), rbErr)
			}
			g.m.logf("successfully rolled back to %s", sw.Toplevel.Get())
			return fmt.Errorf("switch to configuration failed (rolled back to %s): %w", sw.Toplevel.Get(), err)
		}
		return err
	}

	_, okEffects := lifecycle.Update(m, lifecycle.MsgSwitchSucceeded{
		KernelSig: g.kernelSignature(system.Get()),
	})
	for _, eff := range okEffects {
		switch e := eff.(type) {
		case lifecycle.EffectWriteBoot:
			if bt != nil {
				bt.System = e.System.Get()
				if err := g.writeBoot(bt); err != nil {
					if oldSystem != "" && oldSystem != system.Get() {
						g.m.logf("recording boot configuration failed (%v); rolling back to %s...", err, oldSystem)
						_ = g.vsock().Agent(context.Background(), proto.Request{Op: proto.OpApply, Toplevel: oldSystem}, nil)
					}
					return fmt.Errorf("failed to record boot configuration: %w", err)
				}
			}
		case lifecycle.EffectLog:
			g.m.logf("%s", e.Message.Get())
		}
	}
	return nil
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

// Create adds the guest config file if needed, builds it, and starts it.
func (m *Manager) Create(ctx context.Context, name string, spec config.Guest) (*Guest, error) {
	if !config.ValidName(name) {
		return nil, fmt.Errorf("invalid guest name %q (a-z, 0-9 and -, up to 32 characters)", name)
	}
	if m.Config.Guests[name] == nil {
		if err := m.Config.CreateGuest(name, spec, m.Dirs.Home); err != nil {
			return nil, err
		}
		c, err := config.Load(m.Config.Dir(), m.Dirs.Home)
		if err != nil {
			return nil, err
		}
		m.Config = c
		m.logf("created %s", c.GuestPath(name))
	}
	g, err := m.Get(name)
	if err != nil {
		return nil, err
	}
	if g.Created {
		if cur, _ := g.store().Current(); cur != "" {
			return nil, fmt.Errorf("guest %s already exists", name)
		}
	}
	if err := g.Apply(ctx); err != nil {
		return nil, err
	}
	m.logf("starting guest %s...", g.Name())
	if err := g.Start(ctx); err != nil {
		return nil, err
	}
	return g, nil
}

// Remove deletes the guest's state, its config file, and any orphaned guest
// state directories.
func (g *Guest) Remove(ctx context.Context) error {
	if g.Created {
		if err := g.Stop(ctx); err != nil {
			return err
		}
		unlock, err := g.lock()
		if err != nil {
			return err
		}
		defer unlock()
		if err := g.m.Dirs.Remove(g.Inst); err != nil {
			return err
		}
	}
	if err := g.m.Config.RemoveGuest(g.Name()); err != nil {
		return err
	}
	if orphans, err := g.m.Dirs.Orphans(); err == nil {
		for _, p := range orphans {
			instance.RemoveDir(p)
		}
	}
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

// AddShare records path in the guest's config file and, if the guest is running, hot-plugs
// it immediately.
func (g *Guest) AddShare(ctx context.Context, path string, ro bool) (string, error) {
	real, err := g.m.policy().Resolve(path)
	if err != nil {
		return "", err
	}
	absPath, err := domain.ParseAbsPath(real)
	if err != nil {
		return "", err
	}
	tag, err := domain.ParseShareTag(share.Tag(real))
	if err != nil {
		return "", err
	}
	unlock, err := g.lock()
	if err != nil {
		return "", err
	}
	defer unlock()

	m, bt := g.lifecycleModel(ctx)
	if bt != nil {
		for _, s := range bt.Shares {
			if s.Path != real && (within(real, s.Path) || within(s.Path, real)) {
				return "", fmt.Errorf("%s overlaps share %s", real, s.Path)
			}
		}
	}
	_, effects := lifecycle.Update(m, lifecycle.MsgMountShare{
		Path:     absPath,
		Tag:      tag,
		ReadOnly: ro,
	})
	for _, eff := range effects {
		if hp, ok := eff.(lifecycle.EffectHotplugShare); ok && bt != nil {
			if err := g.hotplug(ctx, bt, hp.Path.Get(), hp.ReadOnly); err != nil {
				return "", err
			}
		}
	}
	if err := g.m.Config.AddShare(g.Name(), config.Share{Path: real, ReadOnly: ro}, g.m.Dirs.Home); err != nil {
		return real, err
	}
	return real, nil
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

// RemoveShare removes a share from the guest's config file and, if it was hot-plugged on
// the running guest, unmounts and detaches it immediately.
func (g *Guest) RemoveShare(ctx context.Context, path string) error {
	real := path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		real = r
	}
	unlock, err := g.lock()
	if err != nil {
		return err
	}
	defer unlock()

	m, bt := g.lifecycleModel(ctx)
	var effects []lifecycle.Effect
	if absPath, err := domain.ParseAbsPath(real); err == nil {
		_, effects = lifecycle.Update(m, lifecycle.MsgUnmountShare{Path: absPath})
	}
	removedRuntime := false
	for _, eff := range effects {
		switch e := eff.(type) {
		case lifecycle.EffectHotunplugShare:
			if bt == nil {
				continue
			}
			tagStr := e.Tag.Get()
			pathStr := e.Path.Get()
			if err := g.vsock().Agent(ctx, proto.Request{Op: proto.OpUnmount, Tag: tagStr}, nil); err != nil {
				return err
			}
			if err := g.api().RemoveDevice(ctx, "fs-"+tagStr); err != nil {
				return err
			}
			time.Sleep(2 * time.Second) // see hotplug
			if err := g.m.Units.Stop(ctx, g.shareUnit(tagStr)); err != nil {
				return err
			}
			for i, s := range bt.Shares {
				if s.Path == pathStr {
					bt.Shares = slices.Delete(bt.Shares, i, i+1)
					break
				}
			}
			if err := g.writeBoot(bt); err != nil {
				return err
			}
			removedRuntime = true
		case lifecycle.EffectLog:
			defer g.m.logf("%s", e.Message.Get())
		}
	}
	if err := g.m.Config.RemoveShare(g.Name(), real, g.m.Dirs.Home); err != nil && !removedRuntime {
		return err
	}
	return nil
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
