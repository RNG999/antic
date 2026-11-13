package guest

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/host/chv"
	"github.com/RNG999/antic/internal/host/config"
	"github.com/RNG999/antic/internal/host/share"
	"github.com/RNG999/antic/internal/host/systemd"
	"github.com/RNG999/antic/internal/proto"
	"github.com/RNG999/antic/internal/tea/operation"
)

// Start boots the guest unless it is running, and waits until it is ready.
func (g *Guest) Start(ctx context.Context) error {
	if err := g.requireCreated(); err != nil {
		return err
	}
	return g.power(ctx, operation.OpStart)
}

// Restart reboots the guest with its current config (or boots it).
func (g *Guest) Restart(ctx context.Context) error {
	if err := g.requireCreated(); err != nil {
		return err
	}
	return g.power(ctx, operation.OpRestart)
}

// power runs a start, stop or restart saga under the guest's lock.
func (g *Guest) power(ctx context.Context, kind operation.OpKind) error {
	unlock, err := g.lockAndRecover(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	draft, err := config.ReadDraft(g.Cfg, g.m.Dirs.Home.Get())
	if err != nil {
		return err
	}
	tx, _ := g.planChange(ctx, draft)
	if tx.op.Phase == operation.GuestOff && !tx.oldProfile.Valid() {
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
	fmt.Fprintf(g.m.Log, "antic: starting guest %s...\n", g.GuestName())
	return g.Start(ctx)
}

// startSystem boots system with g.Cfg unless the guest is running, and
// waits until it is ready; it stops everything it started on failure.
func (g *Guest) startSystem(ctx context.Context, system domain.StorePath) error {
	st, err := g.m.Units.State(ctx, g.vmUnit())
	if err != nil {
		return err
	}
	if st.Running() {
		return g.waitReady(ctx)
	}
	if !system.Valid() {
		return fmt.Errorf("guest %s has no system yet; run \"antic guest apply %s\"", g.GuestName(), g.GuestName())
	}
	// Leftovers of an earlier run (the VM is down and we hold the lock).
	if err := g.m.Units.Stop(ctx, g.slice()); err != nil {
		return err
	}
	if err := g.prepareDirs(); err != nil {
		return err
	}
	if err := g.startVM(ctx); err != nil {
		g.m.Units.Stop(context.WithoutCancel(ctx), g.slice())
		return err
	}
	st, err = g.m.Units.State(ctx, g.vmUnit())
	if err != nil {
		return err
	}
	bt := &boot{Invocation: st.Invocation, System: system.Get(), Started: time.Now()}
	cfg, err := g.vmConfig(ctx, system, bt)
	if err == nil {
		err = g.writeBoot(bt)
	}
	if err == nil {
		err = g.api().Create(ctx, cfg)
	}
	if err == nil {
		err = g.api().Boot(ctx)
	}
	if err == nil {
		if err = g.waitReady(ctx); err != nil {
			g.showAgentLog(ctx)
		}
	}
	if err != nil {
		g.m.Units.Stop(context.WithoutCancel(ctx), g.slice())
		return err
	}
	return nil
}

// showAgentLog prints the journal of antic-agent's units, to explain why a
// guest did not become ready (it is torn down right after).
func (g *Guest) showAgentLog(ctx context.Context) {
	jctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	fmt.Fprintf(g.m.Log, "antic: journal of antic-agent's units in %s:\n", g.GuestName())
	g.vsock().Agent(jctx, proto.Request{Op: proto.OpJournal, Lines: 40,
		Units: []string{"antic-agent.service", "antic-ready.target"}},
		func(r proto.Response) {
			if r.Line != "" {
				fmt.Fprintln(g.m.Log, "  "+domain.Printable(r.Line))
			}
		})
}

// prepareDirs recreates the runtime directories and rotates the logs.
func (g *Guest) prepareDirs() error {
	os.RemoveAll(g.Inst.RunDir().Get())
	for _, d := range []domain.AbsPath{g.Inst.VMDir(), g.Inst.SockDir(), g.Inst.LogDir()} {
		if err := os.MkdirAll(d.Get(), 0o700); err != nil {
			return err
		}
	}
	os.Rename(g.Inst.Console().Get(), g.Inst.Console().Get()+".1")
	return nil
}

// startVM starts cloud-hypervisor, waiting for its API; the VM itself is
// created and booted through the API afterwards. Helpers bind to this unit,
// so they stop when cloud-hypervisor exits, however it exits.
func (g *Guest) startVM(ctx context.Context) error {
	api := domain.JoinAbsPath(g.Inst.VMDir(), "api.sock")
	props := append(systemd.Confine(
		[]domain.AbsPath{g.Inst.VMDir(), g.Inst.LogDir()},
		[]domain.AbsPath{g.Inst.StoreDir(), g.Inst.SockDir(), g.m.Bins.CloudHypervisor},
	), systemd.ProtectProcFS()...)
	props = append(props,
		systemd.Prop("NoNewPrivileges", true),
		// Only unix sockets (vhost-user, hybrid vsock, API) and /dev/kvm.
		systemd.UnixOnly(),
		systemd.Prop("PrivateNetwork", true),
		systemd.Prop("MemoryDenyWriteExecute", true),
	)
	desc, _ := domain.ParsePrintableText("antic guest " + g.GuestName().Get())
	if err := g.m.Units.Start(ctx, systemd.Unit{
		Name: g.vmUnit(), Slice: g.slice(), Description: desc, Props: props,
		Argv: []string{g.m.Bins.CloudHypervisor.Get(), "--api-socket", "path=" + api.Get(), "--seccomp", "true"},
	}); err != nil {
		return err
	}
	return g.waitSocket(ctx, g.vmUnit(), api)
}

// startBoundUnitAndWaitSocket starts a unit bound to the VM and waits for its socket.
func (g *Guest) startBoundUnitAndWaitSocket(ctx context.Context, unit systemd.Unit, sock domain.AbsPath) error {
	unit.Slice = g.slice()
	unit.Props = append(unit.Props, systemd.BindsTo(g.vmUnit())...)
	if err := g.m.Units.Start(ctx, unit); err != nil {
		return err
	}
	return g.waitSocket(ctx, unit.Name, sock)
}

// startShare starts virtiofsd for path. Protected paths inside it are bound
// read-only in virtiofsd's view, so the guest (even guest root) cannot
// change them. Returns the vhost-user socket.
func (g *Guest) startShare(ctx context.Context, unit domain.UnitName, sockDir, path domain.AbsPath, ro bool, uid guestUID) (domain.AbsPath, error) {
	if err := os.MkdirAll(sockDir.Get(), 0o700); err != nil {
		return domain.AbsPath{}, err
	}
	sock := domain.JoinAbsPath(sockDir, "fs.sock")
	rwPaths := []domain.AbsPath{sockDir}
	var roPaths []domain.AbsPath
	if ro {
		roPaths = append(roPaths, path)
	} else {
		rwPaths = append(rwPaths, path)
		protected, err := share.Protected(path)
		if err != nil {
			return domain.AbsPath{}, err
		}
		roPaths = append(roPaths, protected...)
	}
	if g.m.Bins.Virtiofsd.Valid() {
		roPaths = append(roPaths, g.m.Bins.Virtiofsd)
	}
	args := []string{g.m.Bins.Virtiofsd.Get(),
		"--socket-path=" + sock.Get(),
		"--shared-dir=" + path.Get(),
		"--sandbox=namespace",
		// Only our own UID/GID is mapped, to uid in the guest.
		fmt.Sprintf("--uid-map=:%d:%d:1:", uid, os.Getuid()),
		fmt.Sprintf("--gid-map=:%d:%d:1:", uid, os.Getgid()),
	}
	if ro {
		args = append(args, "--readonly")
	}
	props := append(g.m.confine(rwPaths, roPaths),
		systemd.Prop("NoNewPrivileges", true), systemd.UnixOnly(), systemd.Prop("PrivateNetwork", true))
	desc, _ := domain.ParsePrintableText(fmt.Sprintf("antic share %s (%s)", path.Get(), g.GuestName()))
	return sock, g.startBoundUnitAndWaitSocket(ctx, systemd.Unit{
		Name:        unit,
		Description: desc,
		Props:       props,
		Argv:        args,
	}, sock)
}

// guestUID represents the UID/GID inside the guest that our host UID/GID maps to:
// the guest user in shares, root in the read-only /nix.
type guestUID int

const (
	guestUserUID guestUID = 1000
	guestRootUID guestUID = 0
)

type fstabMount struct {
	tag      domain.ShareTag
	target   domain.AbsPath
	readOnly bool
}

func oemPlatform(mounts []fstabMount) *chv.Platform {
	esc := strings.NewReplacer(" ", `\040`, "\t", `\011`, "\n", `\012`, `\`, `\134`)
	var fstab strings.Builder
	for _, m := range mounts {
		mode := "rw"
		if m.readOnly {
			mode = "ro"
		}
		fmt.Fprintf(&fstab, "%s %s virtiofs %s,nosuid,nodev,x-systemd.required-by=antic-ready.target,x-systemd.before=antic-ready.target 0 0\n",
			m.tag.Get(), esc.Replace(m.target.Get()), mode)
	}
	mountsCred := "io.systemd.credential.binary:" + proto.CredMounts + "=" + base64.StdEncoding.EncodeToString([]byte(fstab.String()))
	protoCred := "io.systemd.credential.binary:" + proto.CredProtocol + "=" + base64.StdEncoding.EncodeToString([]byte(strconv.Itoa(proto.Version)))
	return &chv.Platform{OEMStrings: []string{mountsCred, protoCred}}
}

// vmConfig starts the store, share and network helpers and returns the VM
// configuration; bt gets the shares.
func (g *Guest) vmConfig(ctx context.Context, system domain.StorePath, bt *boot) (chv.VMConfig, error) {
	store := g.store()
	sysPath := system.Get()
	kernel, err := os.Readlink(store.Real(sysPath + "/kernel"))
	if err != nil {
		return chv.VMConfig{}, err
	}
	initrd, err := os.Readlink(store.Real(sysPath + "/initrd"))
	if err != nil {
		return chv.VMConfig{}, err
	}
	params, err := os.ReadFile(store.Real(sysPath + "/kernel-params"))
	if err != nil {
		return chv.VMConfig{}, err
	}

	storeSock, err := g.startShare(ctx, g.storeShareUnit(), g.storeSockDir(), domain.JoinAbsPath(g.Inst.StoreDir(), "nix"), true, guestRootUID)
	if err != nil {
		return chv.VMConfig{}, fmt.Errorf("guest store: %w", err)
	}
	cfg := chv.VMConfig{
		Payload: chv.Payload{
			Kernel:    store.Real(kernel),
			Initramfs: store.Real(initrd),
			Cmdline:   "init=" + sysPath + "/init " + strings.TrimSpace(string(params)),
		},
		CPUs:    chv.CPUs{Boot: g.Cfg.CPUs, Max: g.Cfg.CPUs},
		Memory:  chv.Memory{Size: g.Cfg.MemoryBytes, Shared: true},
		FS:      []chv.FS{{ID: "fs-store", Tag: proto.StoreTag, Socket: storeSock.Get(), NumQueues: 1, QueueSize: 1024}},
		Vsock:   &chv.Vsock{ID: "vsock", CID: 3, Socket: filepath.Join(g.Inst.VMDir().Get(), "vsock.sock")},
		Serial:  chv.Console{Mode: "Off"},
		Console: chv.Console{Mode: "File", File: g.Inst.Console().Get()},
		RNG:     &chv.RNG{Src: "/dev/urandom"},
	}

	policy := g.m.policy()
	var mounts []fstabMount
	for _, s := range g.Cfg.Shares {
		candidate, err := domain.ParseAbsPath(filepath.Clean(s.Path))
		if err != nil {
			return chv.VMConfig{}, fmt.Errorf("guests.%s share: %w", g.GuestName(), err)
		}
		absPath, err := policy.ValidateShareDirectory(candidate)
		if err != nil {
			return chv.VMConfig{}, fmt.Errorf("guests.%s share: %w", g.GuestName(), err)
		}
		path := absPath.Get()
		tag := share.Tag(absPath)
		sock, err := g.startShare(ctx, g.shareUnit(tag), g.shareSockDir(tag), absPath, s.ReadOnly, guestUserUID)
		if err != nil {
			return chv.VMConfig{}, fmt.Errorf("share %s: %w", path, err)
		}
		cfg.FS = append(cfg.FS, chv.FS{ID: "fs-" + tag.Get(), Tag: tag.Get(), Socket: sock.Get(), NumQueues: 1, QueueSize: 1024})
		bt.Shares = append(bt.Shares, bootShare{Share: config.Share{Path: path, ReadOnly: s.ReadOnly, Source: s.Source, Mode: s.Mode, Target: s.Target}, Tag: tag.Get()})
		target := absPath
		if s.Target != "" {
			if parsedTarget, err := domain.ParseAbsPath(s.Target); err == nil {
				target = parsedTarget
			}
		}
		mounts = append(mounts, fstabMount{tag: tag, target: target, readOnly: s.ReadOnly})
	}

	if g.Cfg.Network.Mode != config.NetworkNone {
		sock, err := g.startNet(ctx)
		if err != nil {
			return chv.VMConfig{}, fmt.Errorf("network: %w", err)
		}
		cfg.Net = []chv.Net{{ID: "net", VhostUser: true, VhostSocket: sock.Get(), VhostMode: "Client", NumQueues: 2, QueueSize: 256}}
	}

	cfg.Platform = oemPlatform(mounts)
	return cfg, nil
}

// waitSocket waits for a unit's socket, failing as soon as the unit stops.
func (g *Guest) waitSocket(ctx context.Context, unit domain.UnitName, sock domain.AbsPath) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		if fi, err := os.Stat(sock.Get()); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return nil
		}
		st, err := g.m.Units.State(ctx, unit)
		if err != nil {
			return err
		}
		if !st.Running() {
			return fmt.Errorf("%s exited (see: journalctl --user -u %s)", unit, unit)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: %s did not appear", unit, sock)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// waitReady waits until antic-agent reports the guest ready, and checks that it
// speaks our protocol for our user.
func (g *Guest) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(120 * time.Second)
	var last error
	for {
		st, err := g.m.Units.State(ctx, g.vmUnit())
		if err != nil {
			return err
		}
		if !st.Running() {
			return fmt.Errorf("guest %s stopped while starting (see: antic guest logs %s)", g.GuestName(), g.GuestName())
		}
		hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		h, err := g.vsock().Hello(hctx)
		cancel()
		switch {
		case err == nil && h.Protocol != proto.Version:
			return fmt.Errorf("guest %s speaks antic protocol %d, this antic speaks %d; update the antic input of its flake and run \"antic guest apply %s\", then restart it",
				g.GuestName(), h.Protocol, proto.Version, g.GuestName())
		case err == nil && h.User != g.m.User.Get():
			return fmt.Errorf("guest %s was built for user %q, not %q; set antic.user in its flake", g.GuestName(), h.User, g.m.User.Get())
		case err == nil && h.Ready:
			return nil
		case err == nil:
			last = errors.New("antic-ready.target not reached")
		default:
			last = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("guest %s did not become ready: %w (see: antic guest logs %s)", g.GuestName(), last, g.GuestName())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Stop shuts the guest down (ACPI power button, then the units).
func (g *Guest) Stop(ctx context.Context) error {
	if !g.Created {
		return nil
	}
	return g.power(ctx, operation.OpStop)
}

// stopLocked shuts the guest down, if it runs, and stops its units.
func (g *Guest) stopLocked(ctx context.Context) error {
	up, err := g.Running(ctx)
	if err != nil {
		return err
	}
	if up {
		if err := g.api().PowerButton(ctx); err != nil {
			fmt.Fprintf(g.m.Log, "antic: power button for %s failed (%v); stopping its units\n", g.GuestName(), err)
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
