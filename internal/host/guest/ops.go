package guest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/RNG999/antic/internal/host/chv"
	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/instance"
	"github.com/RNG999/antic/internal/host/nixbuild"
	"github.com/RNG999/antic/internal/host/share"
	"github.com/RNG999/antic/internal/host/transport"
	"github.com/RNG999/antic/internal/proto"
)

// Apply builds the guest's system on the host, installs it into the guest's
// store and, if the guest is running, switches it there. A guest without an
// ID gets one (this is how a guest is first built).
func (g *Guest) Apply(ctx context.Context) error {
	unlock, err := g.lock()
	if err != nil {
		return err
	}
	defer unlock()
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
		system, err = nix.Build(ctx, g.Cfg.FlakePath, "nixosConfigurations."+g.Cfg.FlakeAttr+".config.system.build.toplevel")
		if err != nil {
			return err
		}
	} else {
		dir, cleanup, err := nixbuild.WriteBuiltinFlake(g.m.User, g.Cfg.Packages)
		if err != nil {
			return err
		}
		defer cleanup()
		g.m.logf("building guest %s (%s)...", g.Name(), g.osDesc())
		system, err = nix.Build(ctx, dir, "nixosConfigurations.antic.config.system.build.toplevel")
		if err != nil {
			return err
		}
	}
	if !g.Created {
		inst, err := g.m.Dirs.Assign(g.Name())
		if err != nil {
			return err
		}
		g.Inst, g.Created = inst, true
		g.m.logf("guest %s has ID %s", g.Name(), inst.ID)
	}
	g.m.logf("installing %s into the guest store...", system)
	if err := nix.Install(ctx, g.store(), system); err != nil {
		return err
	}
	bt, err := g.readBoot(ctx)
	if err != nil {
		g.m.logf("%s is ready; it boots on the next \"antic guest start %s\"", system, g.Name())
		return nil
	}
	if bt.System == system {
		return nil
	}
	g.m.logf("switching the running guest...")
	err = g.vsock().Agent(ctx, proto.Request{Op: proto.OpApply, Toplevel: system}, func(r proto.Response) {
		if r.Line != "" {
			fmt.Fprintln(g.m.Log, r.Line)
		}
	})
	if err != nil {
		return err
	}
	if same(g.store(), bt.System, system, "kernel", "initrd", "kernel-params") {
		bt.System = system
		return g.writeBoot(bt)
	}
	g.m.logf("the kernel changed; restart the guest to boot it (antic guest restart %s)", g.Name())
	return nil
}

// same reports whether two systems have the same targets for links.
func same(s nixbuild.GuestStore, a, b string, links ...string) bool {
	for _, l := range links {
		ta, ea := os.Readlink(s.Real(a + "/" + l))
		tb, eb := os.Readlink(s.Real(b + "/" + l))
		if ea != nil || eb != nil || ta != tb {
			if l == "kernel-params" { // a file, not a link
				ca, _ := os.ReadFile(s.Real(a + "/" + l))
				cb, _ := os.ReadFile(s.Real(b + "/" + l))
				if string(ca) == string(cb) {
					continue
				}
			}
			return false
		}
	}
	return true
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
	var out []ShareInfo
	mounted := map[string]bool{}
	seen := map[string]bool{}
	bt, err := g.readBoot(ctx)
	if err == nil {
		for _, s := range bt.Shares {
			mounted[s.Path] = true
		}
	}
	for _, s := range g.Cfg.Shares {
		p := s.Path
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		seen[p] = true
		out = append(out, ShareInfo{p, s.ReadOnly, mounted[p]})
	}
	if err == nil {
		for _, s := range bt.Shares {
			if !seen[s.Path] {
				out = append(out, ShareInfo{s.Path, s.ReadOnly, true})
			}
		}
	}
	slices.SortFunc(out, func(a, b ShareInfo) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// AddShare records path in the guest's config file and, if the guest is running, hot-plugs
// it immediately.
func (g *Guest) AddShare(ctx context.Context, path string, ro bool) (string, error) {
	real, err := g.m.policy().Resolve(path)
	if err != nil {
		return "", err
	}
	unlock, err := g.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	if bt, err := g.readBoot(ctx); err == nil {
		alreadyMounted := false
		for _, s := range bt.Shares {
			if s.Path == real {
				alreadyMounted = true
				break
			}
			if within(real, s.Path) || within(s.Path, real) {
				return "", fmt.Errorf("%s overlaps share %s", real, s.Path)
			}
		}
		if !alreadyMounted {
			if err := g.hotplug(ctx, bt, real, ro); err != nil {
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
	removedRuntime := false
	bootMounted := false
	if bt, err := g.readBoot(ctx); err == nil {
		for i, s := range bt.Shares {
			if s.Path != real {
				continue
			}
			if !s.Runtime {
				bootMounted = true
				break
			}
			if err := g.vsock().Agent(ctx, proto.Request{Op: proto.OpUnmount, Tag: s.Tag}, nil); err != nil {
				return err
			}
			if err := g.api().RemoveDevice(ctx, "fs-"+s.Tag); err != nil {
				return err
			}
			time.Sleep(2 * time.Second) // see hotplug
			if err := g.m.Units.Stop(ctx, g.shareUnit(s.Tag)); err != nil {
				return err
			}
			bt.Shares = slices.Delete(bt.Shares, i, i+1)
			if err := g.writeBoot(bt); err != nil {
				return err
			}
			removedRuntime = true
			break
		}
	}
	if err := g.m.Config.RemoveShare(g.Name(), real, g.m.Dirs.Home); err != nil && !removedRuntime {
		return err
	}
	if bootMounted {
		g.m.logf("removed %s from %s.toml; restart guest %s to unmount it", real, g.Name(), g.Name())
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

// Info describes a guest for "guest ls" and "guest inspect".
type Info struct {
	Name        string        `json:"name"`
	ID          string        `json:"id,omitempty"`
	Description string        `json:"description,omitempty"`
	State       string        `json:"state"` // not-created, stopped, running
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
		Description: g.Cfg.Description, State: "not-created",
		OS: g.osDesc(), Packages: g.Cfg.Packages,
		CPUs: g.Cfg.CPUs, Memory: g.Cfg.Memory, Network: g.Cfg.Network, Ports: g.Cfg.Ports,
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
		i.Units = []string{g.vmUnit()}
		if g.Cfg.Network != config.NetworkNone {
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
