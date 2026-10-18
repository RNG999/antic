// Package guest runs hanare guests: it composes config, instances, systemd
// units, cloud-hypervisor, Nix and the guest transports into the lifecycle
// operations the CLI offers. Mutating operations hold the guest's lock.
package guest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"time"

	"github.com/RNG999/hanare/internal/host/chv"
	"github.com/RNG999/hanare/internal/host/config"
	"github.com/RNG999/hanare/internal/host/instance"
	"github.com/RNG999/hanare/internal/host/nixbuild"
	"github.com/RNG999/hanare/internal/host/share"
	"github.com/RNG999/hanare/internal/host/systemd"
	"github.com/RNG999/hanare/internal/host/transport"
	"github.com/RNG999/hanare/internal/proto"
)

// Binaries are the host programs hanare runs.
type Binaries struct {
	CloudHypervisor, Virtiofsd, Passt, Pasta, Nft string
}

// FindBinaries locates them: cloud-hypervisor from hanare's libexec
// ("hanare upgrade" installs it), the rest from the system.
func FindBinaries(d instance.Dirs) Binaries {
	find := func(name string, extra ...string) string {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
		for _, p := range extra {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		return name
	}
	return Binaries{
		CloudHypervisor: filepath.Join(d.Libexec, "cloud-hypervisor"),
		Virtiofsd:       find("virtiofsd", "/usr/lib/virtiofsd", "/usr/libexec/virtiofsd"),
		Passt:           find("passt"),
		Pasta:           find("pasta"),
		Nft:             find("nft", "/usr/sbin/nft"),
	}
}

// Manager runs guests.
type Manager struct {
	Dirs   instance.Dirs
	Config *config.Config
	Units  systemd.Manager
	Bins   Binaries
	Log    io.Writer // progress messages
	User   string    // host user = guest login user

	nix *nixbuild.Nix
}

// NewManager connects to systemd and returns a manager.
func NewManager(ctx context.Context, d instance.Dirs, cfg *config.Config, log io.Writer) (*Manager, error) {
	u, err := user.Current()
	if err != nil {
		return nil, err
	}
	if d.Home != "/home/"+u.Username {
		return nil, fmt.Errorf("home directory %s is not /home/%s (shares appear at the same path in the guest)", d.Home, u.Username)
	}
	units, err := systemd.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &Manager{Dirs: d, Config: cfg, Units: units, Bins: FindBinaries(d), Log: log, User: u.Username}, nil
}

func (m *Manager) logf(format string, args ...any) {
	fmt.Fprintf(m.Log, "hanare: "+format+"\n", args...)
}

func (m *Manager) nixBuilder() (*nixbuild.Nix, error) {
	if m.nix == nil {
		n, err := nixbuild.New()
		if err != nil {
			return nil, err
		}
		m.nix = n
	}
	return m.nix, nil
}

// Guest is one configured guest.
type Guest struct {
	m    *Manager
	Cfg  *config.Guest
	Inst instance.Instance
	// Created is false until the guest has an ID (its first build).
	Created bool
}

// Get returns the guest name (see config.Pick for the default).
func (m *Manager) Get(name string) (*Guest, error) {
	c, err := m.Config.Pick(name)
	if err != nil {
		return nil, err
	}
	inst, ok, err := m.Dirs.Lookup(c.Name)
	if err != nil {
		return nil, err
	}
	return &Guest{m: m, Cfg: c, Inst: inst, Created: ok}, nil
}

// All returns every configured guest.
func (m *Manager) All() ([]*Guest, error) {
	var out []*Guest
	for _, n := range m.Config.Names() {
		g, err := m.Get(n)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

func (g *Guest) Name() string { return g.Cfg.Name }

// Units and slice, keyed by ID.
func (g *Guest) vmUnit() string  { return "hanare-vm@" + g.Inst.ID + ".service" }
func (g *Guest) netUnit() string { return "hanare-net@" + g.Inst.ID + ".service" }
func (g *Guest) shareUnit(tag string) string {
	return "hanare-share@" + g.Inst.ID + ":" + tag + ".service"
}
func (g *Guest) slice() string              { return "hanare-" + g.Inst.ID + ".slice" }
func (g *Guest) store() nixbuild.GuestStore { return nixbuild.GuestStore(g.Inst.StoreDir()) }
func (g *Guest) api() *chv.Client           { return chv.New(filepath.Join(g.Inst.VMDir(), "api.sock")) }
func (g *Guest) vsock() transport.Vsock {
	return transport.Vsock{Socket: filepath.Join(g.Inst.VMDir(), "vsock.sock")}
}
func (g *Guest) sockDir(helper string) string { return filepath.Join(g.Inst.SockDir(), helper) }
func (g *Guest) ssh() transport.SSH {
	return transport.SSH{Vsock: g.vsock(), User: g.m.User, ClientKey: g.m.Dirs.ClientKey(), HostKey: g.Inst.HostKey()}
}

// requireCreated fails for a guest without a build.
func (g *Guest) requireCreated() error {
	if !g.Created {
		return fmt.Errorf("guest %s has not been built yet; run \"hanare guest create %s\"", g.Name(), g.Name())
	}
	return nil
}

// Running reports whether the guest's VM is up; errors are not "stopped".
func (g *Guest) Running(ctx context.Context) (bool, error) {
	if !g.Created {
		return false, nil
	}
	st, err := g.m.Units.State(ctx, g.vmUnit())
	if err != nil {
		return false, err
	}
	return st.Running(), nil
}

// lock takes the guest's lifecycle lock.
func (g *Guest) lock() (func(), error) { return g.m.Dirs.Lock(g.Name()) }

// boot records what the running VM was started with. It is only trusted
// while the VM unit's InvocationID matches.
type boot struct {
	Invocation string      `json:"invocation"`
	System     string      `json:"system"`
	Shares     []bootShare `json:"shares"`
	Started    time.Time   `json:"started"`
}

type bootShare struct {
	config.Share
	Tag     string `json:"tag"`
	Runtime bool   `json:"runtime,omitempty"`
}

func (g *Guest) readBoot(ctx context.Context) (*boot, error) {
	st, err := g.m.Units.State(ctx, g.vmUnit())
	if err != nil {
		return nil, err
	}
	if !st.Running() {
		return nil, fmt.Errorf("guest %s is not running", g.Name())
	}
	b, err := os.ReadFile(g.Inst.BootFile())
	if err != nil {
		return nil, fmt.Errorf("guest %s: no boot record: %w", g.Name(), err)
	}
	var bt boot
	if err := json.Unmarshal(b, &bt); err != nil {
		return nil, err
	}
	if bt.Invocation != st.Invocation {
		return nil, fmt.Errorf("guest %s: boot record is from an earlier start; restart the guest", g.Name())
	}
	return &bt, nil
}

func (g *Guest) writeBoot(bt *boot) error {
	b, err := json.MarshalIndent(bt, "", "  ")
	if err != nil {
		return err
	}
	tmp := g.Inst.BootFile() + ".new"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, g.Inst.BootFile())
}

// policy is the share policy, protecting hanare's own directories and every
// guest's flake.
func (m *Manager) policy() share.Policy {
	extra := []string{m.Dirs.ConfigDir, m.Dirs.StateRoot, m.Dirs.Libexec, m.Dirs.SrcDir}
	if real, err := filepath.EvalSymlinks(m.Config.Path()); err == nil {
		extra = append(extra, filepath.Dir(real))
	}
	for _, c := range m.Config.Guests {
		extra = append(extra, c.Flake)
	}
	extra = append(extra, m.Config.Policy.Deny...)
	return share.NewPolicy(m.Dirs.Home, extra, m.Config.Policy.ShareRoots)
}

// InShareRoots reports whether path is inside policy.share_roots.
func (m *Manager) InShareRoots(path string) bool {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	return m.policy().InRoots(path)
}

// protectedNames are the names exported read-only inside shares.
func (m *Manager) protectedNames() []string {
	return append(append([]string{}, proto.ProtectedNames...), m.Config.Policy.Protect...)
}

var errNotRunning = errors.New("not running")
