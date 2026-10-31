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

// startSystem boots system with g.Cfg unless the guest is running, and
// waits until it is ready; it stops everything it started on failure.
func (g *Guest) startSystem(ctx context.Context, system string) error {
	st, err := g.m.Units.State(ctx, g.vmUnit())
	if err != nil {
		return err
	}
	if st.Running() {
		return g.waitReady(ctx)
	}
	if system == "" {
		return fmt.Errorf("guest %s has no system yet; run \"antic guest apply %s\"", g.Name(), g.Name())
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
	bt := &boot{Invocation: st.Invocation, System: system, Started: time.Now()}
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
	g.m.logf("journal of antic-agent's units in %s:", g.Name())
	g.vsock().Agent(jctx, proto.Request{Op: proto.OpJournal, Lines: 40,
		Units: []string{"antic-agent.service", "antic-ready.target"}},
		func(r proto.Response) {
			if r.Line != "" {
				fmt.Fprintln(g.m.Log, "  "+r.Line)
			}
		})
}

// prepareDirs recreates the runtime directories and rotates the logs.
func (g *Guest) prepareDirs() error {
	os.RemoveAll(g.Inst.RunDir())
	for _, d := range []string{g.Inst.VMDir(), g.Inst.SockDir(), g.Inst.LogDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	os.Rename(g.Inst.Console(), g.Inst.Console()+".1")
	return nil
}

// startVM starts cloud-hypervisor, waiting for its API; the VM itself is
// created and booted through the API afterwards. Helpers bind to this unit,
// so they stop when cloud-hypervisor exits, however it exits.
func (g *Guest) startVM(ctx context.Context) error {
	api := filepath.Join(g.Inst.VMDir(), "api.sock")
	props := append(systemd.Confine(
		[]string{g.Inst.VMDir(), g.Inst.LogDir()},
		[]string{g.Inst.StoreDir(), g.Inst.SockDir(), g.m.Bins.CloudHypervisor},
	), systemd.ProtectProcFS()...)
	props = append(props,
		systemd.Prop("NoNewPrivileges", true),
		// Only unix sockets (vhost-user, hybrid vsock, API) and /dev/kvm.
		systemd.UnixOnly(),
		systemd.Prop("PrivateNetwork", true),
		systemd.Prop("MemoryDenyWriteExecute", true),
	)
	if err := g.m.Units.Start(ctx, systemd.Unit{
		Name: g.vmUnit(), Slice: g.slice(), Description: "antic guest " + g.Name(), Props: props,
		Argv: []string{g.m.Bins.CloudHypervisor, "--api-socket", "path=" + api, "--seccomp", "true"},
	}); err != nil {
		return err
	}
	return g.waitSocket(ctx, g.vmUnit(), api)
}

// helper starts a unit bound to the VM and waits for its socket.
func (g *Guest) helper(ctx context.Context, name, desc string, props []systemd.Property, sock string, argv ...string) error {
	if err := g.m.Units.Start(ctx, systemd.Unit{
		Name: name, Slice: g.slice(), Description: desc, Argv: argv,
		Props: append(props, systemd.BindsTo(g.vmUnit())...),
	}); err != nil {
		return err
	}
	return g.waitSocket(ctx, name, sock)
}

// startShare starts virtiofsd for path. Protected paths inside it are bound
// read-only in virtiofsd's view, so the guest (even guest root) cannot
// change them. Returns the vhost-user socket.
func (g *Guest) startShare(ctx context.Context, tag, path string, ro bool, guestID int) (string, error) {
	dir := g.sockDir(tag)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sock := filepath.Join(dir, "fs.sock")
	rw, roPaths := []string{dir}, []string{}
	if ro {
		roPaths = append(roPaths, path)
	} else {
		rw = append(rw, path)
		protected, err := share.Protected(path, g.m.protectedNames())
		if err != nil {
			return "", err
		}
		roPaths = append(roPaths, protected...)
	}
	args := []string{g.m.Bins.Virtiofsd,
		"--socket-path=" + sock,
		"--shared-dir=" + path,
		"--sandbox=namespace",
		// Only our own UID/GID is mapped, to guestID in the guest.
		fmt.Sprintf("--uid-map=:%d:%d:1:", guestID, os.Getuid()),
		fmt.Sprintf("--gid-map=:%d:%d:1:", guestID, os.Getgid()),
	}
	if ro {
		args = append(args, "--readonly")
	}
	props := append(g.m.confine(rw, append(roPaths, g.m.Bins.Virtiofsd)),
		systemd.Prop("NoNewPrivileges", true), systemd.UnixOnly(), systemd.Prop("PrivateNetwork", true))
	return sock, g.helper(ctx, g.shareUnit(tag), "antic share "+path+" ("+g.Name()+")", props, sock, args...)
}

// Guest IDs our UID/GID appear as: the guest user in shares, root in the
// read-only /nix.
const (
	guestUserID = 1000
	guestRootID = 0
)

// vmConfig starts the store, share and network helpers and returns the VM
// configuration; bt gets the shares.
func (g *Guest) vmConfig(ctx context.Context, system string, bt *boot) (chv.VMConfig, error) {
	store := g.store()
	kernel, err := os.Readlink(store.Real(system + "/kernel"))
	if err != nil {
		return chv.VMConfig{}, err
	}
	initrd, err := os.Readlink(store.Real(system + "/initrd"))
	if err != nil {
		return chv.VMConfig{}, err
	}
	params, err := os.ReadFile(store.Real(system + "/kernel-params"))
	if err != nil {
		return chv.VMConfig{}, err
	}

	storeSock, err := g.startShare(ctx, "store", g.Inst.StoreDir()+"/nix", true, guestRootID)
	if err != nil {
		return chv.VMConfig{}, fmt.Errorf("guest store: %w", err)
	}
	cfg := chv.VMConfig{
		Payload: chv.Payload{
			Kernel:    store.Real(kernel),
			Initramfs: store.Real(initrd),
			Cmdline:   "init=" + system + "/init " + strings.TrimSpace(string(params)),
		},
		CPUs:    chv.CPUs{Boot: g.Cfg.CPUs, Max: g.Cfg.CPUs},
		Memory:  chv.Memory{Size: g.Cfg.MemoryBytes, Shared: true},
		FS:      []chv.FS{{ID: "fs-store", Tag: proto.StoreTag, Socket: storeSock, NumQueues: 1, QueueSize: 1024}},
		Vsock:   &chv.Vsock{ID: "vsock", CID: 3, Socket: filepath.Join(g.Inst.VMDir(), "vsock.sock")},
		Serial:  chv.Console{Mode: "Off"},
		Console: chv.Console{Mode: "File", File: g.Inst.Console()},
		RNG:     &chv.RNG{Src: "/dev/urandom"},
	}

	policy := g.m.policy()
	var fstab strings.Builder
	for _, s := range g.Cfg.Shares {
		path, err := policy.Resolve(s.Path)
		if err != nil {
			return chv.VMConfig{}, fmt.Errorf("guests.%s share: %w", g.Name(), err)
		}
		tag := share.Tag(path)
		sock, err := g.startShare(ctx, tag, path, s.ReadOnly, guestUserID)
		if err != nil {
			return chv.VMConfig{}, fmt.Errorf("share %s: %w", path, err)
		}
		cfg.FS = append(cfg.FS, chv.FS{ID: "fs-" + tag, Tag: tag, Socket: sock, NumQueues: 1, QueueSize: 1024})
		bt.Shares = append(bt.Shares, bootShare{Share: config.Share{Path: path, ReadOnly: s.ReadOnly, Source: s.Source, Mode: s.Mode, Target: s.Target}, Tag: tag})
		// Sessions require antic-ready.target, which requires every share.
		mode := "rw"
		if s.ReadOnly {
			mode = "ro"
		}
		target := path
		if s.Target != "" {
			target = s.Target
		}
		fmt.Fprintf(&fstab, "%s %s virtiofs %s,nosuid,nodev,x-systemd.required-by=antic-ready.target,x-systemd.before=antic-ready.target 0 0\n",
			tag, fstabEscape(target), mode)
	}

	if g.Cfg.Network.Mode != config.NetworkNone {
		sock, err := g.startNet(ctx)
		if err != nil {
			return chv.VMConfig{}, fmt.Errorf("network: %w", err)
		}
		cfg.Net = []chv.Net{{ID: "net", VhostUser: true, VhostSocket: sock, VhostMode: "Client", NumQueues: 2, QueueSize: 256}}
	}

	cred := func(name string, data []byte) string {
		return "io.systemd.credential.binary:" + name + "=" + base64.StdEncoding.EncodeToString(data)
	}
	cfg.Platform = &chv.Platform{OEMStrings: []string{
		cred(proto.CredMounts, []byte(fstab.String())),
		cred(proto.CredProtocol, []byte(strconv.Itoa(proto.Version))),
	}}
	return cfg, nil
}

// fstabEscape escapes a path for fstab (octal escapes for whitespace and
// backslash).
func fstabEscape(p string) string {
	r := strings.NewReplacer(" ", `\040`, "\t", `\011`, "\n", `\012`, `\`, `\134`)
	return r.Replace(p)
}

// waitSocket waits for a unit's socket, failing as soon as the unit stops.
func (g *Guest) waitSocket(ctx context.Context, unit, sock string) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		if fi, err := os.Stat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
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
			return fmt.Errorf("guest %s stopped while starting (see: antic guest logs %s)", g.Name(), g.Name())
		}
		hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		h, err := g.vsock().Hello(hctx)
		cancel()
		switch {
		case err == nil && h.Protocol != proto.Version:
			return fmt.Errorf("guest %s speaks antic protocol %d, this antic speaks %d; update the antic input of its flake and run \"antic guest apply %s\", then restart it",
				g.Name(), h.Protocol, proto.Version, g.Name())
		case err == nil && h.User != g.m.User:
			return fmt.Errorf("guest %s was built for user %q, not %q; set antic.user in its flake", g.Name(), h.User, g.m.User)
		case err == nil && h.Ready:
			return nil
		case err == nil:
			last = errors.New("antic-ready.target not reached")
		default:
			last = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("guest %s did not become ready: %w (see: antic guest logs %s)", g.Name(), last, g.Name())
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
