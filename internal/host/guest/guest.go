// Package guest runs antic guests: it composes config, instances, systemd
// units, cloud-hypervisor, Nix and the guest transports into the lifecycle
// operations the CLI offers. Mutating operations hold the guest's lock.
package guest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/host/chv"
	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/instance"
	"github.com/RNG999/antic/internal/host/nixbuild"
	"github.com/RNG999/antic/internal/host/share"
	"github.com/RNG999/antic/internal/host/systemd"
	"github.com/RNG999/antic/internal/host/transport"
)

// Binaries are the host programs antic runs. Self is antic itself (it runs
// as the first process of each guest's network namespace).
type Binaries struct {
	CloudHypervisor, Virtiofsd, Passt, Pasta, NFT, Self domain.AbsPath
}

type hostExecutableKind int

const (
	executableVirtiofsd hostExecutableKind = iota
	executablePasst
	executablePasta
	executableNFT
)

func locateHostExecutable(d instance.Dirs, kind hostExecutableKind) domain.AbsPath {
	var binaryName string
	var systemCandidates []string
	switch kind {
	case executableVirtiofsd:
		binaryName = "virtiofsd"
		systemCandidates = []string{"/usr/lib/virtiofsd", "/usr/libexec/virtiofsd"}
	case executablePasst:
		binaryName = "passt"
	case executablePasta:
		binaryName = "pasta"
	case executableNFT:
		binaryName = "nft"
		systemCandidates = []string{"/usr/sbin/nft"}
	}
	if d.Libexec.Valid() {
		cand := domain.JoinAbsPath(d.Libexec, binaryName)
		if cand.Valid() {
			if _, err := os.Stat(cand.Get()); err == nil {
				return cand
			}
		}
	}
	if p, err := exec.LookPath(binaryName); err == nil {
		if abs, parseErr := domain.ParseAbsPath(filepath.Clean(p)); parseErr == nil {
			return abs
		}
	}
	for _, p := range systemCandidates {
		if _, err := os.Stat(p); err == nil {
			if abs, parseErr := domain.ParseAbsPath(p); parseErr == nil {
				return abs
			}
		}
	}
	return domain.AbsPath{}
}

// FindBinaries locates them: executables installed in antic's libexec
// ("antic update") take precedence, falling back to the system.
func FindBinaries(d instance.Dirs) Binaries {
	var selfPath domain.AbsPath
	if rawSelf, err := os.Executable(); err == nil {
		selfPath, _ = domain.ParseAbsPath(filepath.Clean(config.RealPath(rawSelf)))
	}
	return Binaries{
		CloudHypervisor: domain.JoinAbsPath(d.Libexec, "cloud-hypervisor"),
		Virtiofsd:       locateHostExecutable(d, executableVirtiofsd),
		Passt:           locateHostExecutable(d, executablePasst),
		Pasta:           locateHostExecutable(d, executablePasta),
		NFT:             locateHostExecutable(d, executableNFT),
		Self:            selfPath,
	}
}

// confine returns systemd sandbox properties for a unit, also binding the
// rootless Nix store at /nix when helpers link against /nix/store. The user
// manager cannot create the /nix mount point, so the bind needs an existing
// /nix; without one, the unit would fail to set up its namespace.
func (m *Manager) confine(rwPaths, roPaths []domain.AbsPath) []systemd.Property {
	if fi, err := os.Stat("/nix"); err != nil || !fi.IsDir() {
		return systemd.Confine(rwPaths, roPaths)
	}
	if nix, err := m.nixBuilder(); err == nil && nix.Root() != "" {
		src, err1 := domain.ParseAbsPath(filepath.Clean(filepath.Join(nix.Root(), "nix")))
		dst, err2 := domain.ParseAbsPath("/nix")
		if err1 == nil && err2 == nil {
			return systemd.Confine(rwPaths, roPaths, systemd.BindMount{
				Source:        src,
				Destination:   dst,
				IgnoreMissing: true,
			})
		}
	}
	return systemd.Confine(rwPaths, roPaths)
}

// Manager runs guests.
type Manager struct {
	Dirs   instance.Dirs
	Config *config.Config
	Units  systemd.Manager
	Bins   Binaries
	Log    io.Writer       // progress messages
	User   domain.UserName // host user = guest login user

	nix *nixbuild.Nix
}

// NewManager connects to systemd and returns a manager.
func NewManager(ctx context.Context, d instance.Dirs, cfg *config.Config, log io.Writer) (*Manager, error) {
	u, err := user.Current()
	if err != nil {
		return nil, err
	}
	userName, err := domain.ParseUserName(u.Username)
	if err != nil {
		return nil, err
	}
	expectedHome, err := domain.HomeDir(userName)
	if err != nil || d.Home != expectedHome {
		return nil, fmt.Errorf("home directory %s is not /home/%s (shares appear at the same path in the guest)", d.Home.Get(), userName.Get())
	}
	units, err := systemd.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &Manager{Dirs: d, Config: cfg, Units: units, Bins: FindBinaries(d), Log: log, User: userName}, nil
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

// Get returns the named guest.
func (m *Manager) Get(name domain.GuestName) (*Guest, error) {
	c, err := m.Config.Get(name.Get())
	if err != nil {
		return nil, err
	}
	inst, ok, err := m.Dirs.Lookup(name)
	if err != nil {
		return nil, err
	}
	if ok {
		// Check ownership if an origin file exists
		if origBytes, err := os.ReadFile(inst.OriginFile().Get()); err == nil {
			orig := strings.TrimSpace(string(origBytes))
			if orig != "" && orig != c.Path {
				return nil, fmt.Errorf("guest %q already exists and is owned by %s (cannot use from %s)", c.Name, orig, c.Path)
			}
		}
	}
	return &Guest{m: m, Cfg: c, Inst: inst, Created: ok}, nil
}

// All returns every configured guest whose config loads (the others are
// in m.Config.Errors).
func (m *Manager) All() ([]*Guest, error) {
	var out []*Guest
	for _, rawName := range m.Config.Names() {
		if m.Config.Errors[rawName] != nil {
			continue
		}
		guestName, err := domain.ParseGuestName(rawName)
		if err != nil {
			return nil, err
		}
		g, err := m.Get(guestName)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

// GetUnloadable returns guest name whose config file does not load, so
// that it can still be stopped and removed (D17).
func (m *Manager) GetUnloadable(name domain.GuestName) (*Guest, error) {
	if m.Config.Errors[name.Get()] == nil {
		return nil, fmt.Errorf("no such guest %q", name.Get())
	}
	inst, ok, err := m.Dirs.Lookup(name)
	if err != nil {
		return nil, err
	}
	return &Guest{m: m, Cfg: &config.Guest{Name: name.Get(), Path: m.Config.GuestPath(name.Get())}, Inst: inst, Created: ok}, nil
}

func (g *Guest) GuestName() domain.GuestName {
	gn, _ := domain.ParseGuestName(g.Cfg.Name)
	return gn
}

// Units and slice, keyed by ID.
func (g *Guest) vmUnit() domain.UnitName {
	u, _ := domain.ParseUnitName("antic-vm@" + g.Inst.ID.Get() + ".service")
	return u
}
func (g *Guest) netUnit() domain.UnitName {
	u, _ := domain.ParseUnitName("antic-net@" + g.Inst.ID.Get() + ".service")
	return u
}
func (g *Guest) shareUnit(tag domain.ShareTag) domain.UnitName {
	u, _ := domain.ParseUnitName("antic-share@" + g.Inst.ID.Get() + ":" + tag.Get() + ".service")
	return u
}
func (g *Guest) storeShareUnit() domain.UnitName {
	u, _ := domain.ParseUnitName("antic-share@" + g.Inst.ID.Get() + ":store.service")
	return u
}
func (g *Guest) slice() domain.UnitName {
	u, _ := domain.ParseUnitName("antic-" + g.Inst.ID.Get() + ".slice")
	return u
}
func (g *Guest) CgroupDir() domain.AbsPath {
	uid := os.Getuid()
	base, _ := domain.ParseAbsPath("/sys/fs/cgroup/user.slice")
	return domain.JoinAbsPath(base, fmt.Sprintf("user-%d.slice", uid), fmt.Sprintf("user@%d.service", uid), "antic.slice", g.slice().Get())
}
func (g *Guest) store() nixbuild.GuestStore { return nixbuild.GuestStore(g.Inst.StoreDir().Get()) }
func (g *Guest) api() *chv.Client           { return chv.New(filepath.Join(g.Inst.VMDir().Get(), "api.sock")) }
func (g *Guest) vsock() transport.Vsock {
	return transport.Vsock{Socket: filepath.Join(g.Inst.VMDir().Get(), "vsock.sock")}
}
func (g *Guest) netSockDir() domain.AbsPath {
	return domain.JoinAbsPath(g.Inst.SockDir(), "net")
}
func (g *Guest) storeSockDir() domain.AbsPath {
	return domain.JoinAbsPath(g.Inst.SockDir(), "store")
}
func (g *Guest) shareSockDir(tag domain.ShareTag) domain.AbsPath {
	return domain.JoinAbsPath(g.Inst.SockDir(), tag.Get())
}

// requireCreated fails for a guest without a build.
func (g *Guest) requireCreated() error {
	if !g.Created {
		return fmt.Errorf("guest %s has not been built yet; run \"antic guest create %s\"", g.GuestName(), g.GuestName())
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
func (g *Guest) lock() (func(), error) { return g.m.Dirs.Lock(g.GuestName()) }

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
		return nil, fmt.Errorf("guest %s is not running", g.GuestName())
	}
	b, err := os.ReadFile(g.Inst.BootFile().Get())
	if err != nil {
		return nil, fmt.Errorf("guest %s: no boot record: %w", g.GuestName(), err)
	}
	var bt boot
	if err := json.Unmarshal(b, &bt); err != nil {
		return nil, err
	}
	if bt.Invocation != st.Invocation {
		return nil, fmt.Errorf("guest %s: boot record is from an earlier start; restart the guest", g.GuestName())
	}
	return &bt, nil
}

func (g *Guest) writeBoot(bt *boot) error {
	b, err := json.MarshalIndent(bt, "", "  ")
	if err != nil {
		return err
	}
	tmp := g.Inst.BootFile().Get() + ".new"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, g.Inst.BootFile().Get())
}

// policy is the share policy, protecting antic's own directories and every
// guest's flake.
func (m *Manager) policy() share.Policy {
	var extra []domain.AbsPath
	for _, p := range []domain.AbsPath{m.Dirs.ConfigDir, m.Dirs.StateRoot, m.Dirs.Libexec} {
		if p.Valid() {
			extra = append(extra, p)
		}
	}
	if real := config.RealPath(m.Config.Dir()); real != m.Config.Dir() {
		if abs, err := domain.ParseAbsPath(filepath.Clean(real)); err == nil {
			extra = append(extra, abs)
		}
	}
	for _, c := range m.Config.Guests {
		if c.FlakePath != "" {
			if abs, err := domain.ParseAbsPath(filepath.Clean(c.FlakePath)); err == nil {
				extra = append(extra, abs)
			}
		}
	}
	return share.NewPolicy(m.Dirs.Home, extra, nil)
}
