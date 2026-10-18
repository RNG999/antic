package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	virtiofsd = "/usr/lib/virtiofsd"
	passt     = "/usr/bin/passt"
	pasta     = "/usr/bin/pasta"
	nft       = "/usr/bin/nft"
	qemu      = "/usr/bin/qemu-system-x86_64"
	ovmfCode  = "/usr/share/edk2/x64/OVMF_CODE.4m.fd"
	ovmfVars  = "/usr/share/edk2/x64/OVMF_VARS.4m.fd"
	netUnit   = "hanare-net.service"
	// Guest-visible DNS address; passt forwards queries sent to it. Taken from
	// TEST-NET-1 so it never clashes with a real host.
	guestDNS = "192.0.2.53"
	// Inside hanare-net's namespace, pasta forwards DNS sent here to the
	// host's resolver.
	nsDNS = "192.0.2.54"
)

// Destinations the guest may not reach unless network.allow_lan is set:
// private/LAN, CGNAT (Tailscale), link-local (incl. cloud metadata) and the
// namespace's own loopback.
const lanBlockRules = `table inet hanare {
  chain output {
    type filter hook output priority filter; policy accept;
    ip daddr ` + nsDNS + ` accept
    ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, 169.254.0.0/16, 127.0.0.0/8 } reject
    ip6 daddr { fc00::/7, fe80::/10, ::1 } reject
  }
}
`

func unitActive() bool {
	return exec.Command("systemctl", "--user", "is-active", "--quiet", unit).Run() == nil
}

// startUnit starts the network unit and then "hanare _vm" as a transient
// user service, so the VM and its helpers live in one cgroup that systemd
// cleans up as a whole.
func startUnit() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := ensureHostKey(false); err != nil {
		return err
	}
	if err := os.MkdirAll(runDir(), 0o700); err != nil {
		return err
	}
	// Even if QEMU or a helper is compromised, it sees only the shared
	// directories and hanare's own files from $HOME, and nothing writable
	// outside them.
	rw := []string{stateDir(), runDir()}
	ro := []string{configDir(), cfg.Guest.Flake, exe, libexecDir()}
	if real, err := filepath.EvalSymlinks(configFile()); err == nil {
		ro = append(ro, real)
	}
	for _, s := range cfg.Shares {
		p, err := cfg.resolveShare(s.Path)
		if err != nil {
			return fmt.Errorf("%s: %w", configFile(), err)
		}
		if s.ReadOnly {
			ro = append(ro, p)
		} else {
			rw = append(rw, p)
		}
	}
	if err := startNet(cfg, exe); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	cmd := exec.Command("systemd-run", "--user", "--quiet", "--collect",
		"--unit="+unit, "--description=hanare VM",
		"-p", "ProtectHome=tmpfs",
		"-p", "ProtectSystem=strict",
		"-p", "PrivateTmp=yes",
		"-p", "NoNewPrivileges=yes",
		// QEMU and its helpers only talk over AF_UNIX (networking is passt's).
		"-p", "RestrictAddressFamilies=AF_UNIX",
		"-p", "LockPersonality=yes",
		"-p", "RestrictRealtime=yes",
		"-p", "RestrictSUIDSGID=yes",
		"-p", "SystemCallArchitectures=native",
		"-p", "BindPaths="+strings.Join(rw, " "),
		"-p", "BindReadOnlyPaths="+strings.Join(ro, " "),
		// The client key logs into the guest; QEMU never needs it.
		"-p", "InaccessiblePaths="+keyPath(),
		"--", exe, "_vm")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		stopUnit(netUnit)
		return err
	}
	return nil
}

// startNet runs passt inside a rootless network namespace provided by pasta
// (hanare-net.service), where "hanare _net" installs the LAN block first:
// the filter sits outside the guest, so guest root cannot remove it.
func startNet(cfg *Config, exe string) error {
	stopUnit(netUnit)
	sock := filepath.Join(runDir(), "passt.sock")
	os.Remove(sock)
	lan := "--block-lan"
	if cfg.Network.AllowLAN {
		lan = "--allow-lan"
	}
	out, err := exec.Command("systemd-run", "--user", "--quiet", "--collect",
		"--unit="+netUnit, "--description=hanare network (pasta + passt)",
		"-p", "PartOf="+unit,
		"-p", "ProtectHome=tmpfs",
		"-p", "ProtectSystem=strict",
		"-p", "PrivateTmp=yes",
		// No NoNewPrivileges here: it stops passt from setting up its own
		// user namespace inside pasta's. passt and pasta drop privileges and
		// apply seccomp themselves.
		"-p", "BindPaths="+runDir(),
		"-p", "BindReadOnlyPaths="+exe,
		"--", pasta, "--config-net", "--quiet",
		"--no-map-gw", "--map-host-loopback", "none", "--dns-forward", nsDNS,
		"--", exe, "_net", lan).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	if err := waitSocket(sock); err != nil {
		stopUnit(netUnit)
		return err
	}
	return nil
}

// execNet runs inside hanare-net's namespace (as its root): block LAN
// destinations, then become passt. passt has no host-loopback mapping and
// forwards the guest's DNS through pasta to the host resolver.
func execNet(args []string) error {
	if len(args) != 1 || (args[0] != "--block-lan" && args[0] != "--allow-lan") {
		return errors.New("usage: hanare _net --block-lan|--allow-lan")
	}
	if args[0] == "--block-lan" {
		cmd := exec.Command(nft, "-f", "-")
		cmd.Stdin = strings.NewReader(lanBlockRules)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("nft: %v: %s", err, out)
		}
	}
	sock := filepath.Join(runDir(), "passt.sock")
	return syscall.Exec(passt, []string{passt, "--foreground", "--quiet", "--one-off",
		"--socket", sock, "--no-map-gw",
		"--dns", guestDNS, "--dns-forward", guestDNS, "--dns-host", nsDNS}, os.Environ())
}

// execVM starts virtiofsd per share and the vsock backend, then execs QEMU,
// which becomes the unit's main process. Runs inside hanare.service.
func execVM() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	run := runDir()
	if err := os.MkdirAll(run, 0o700); err != nil {
		return err
	}
	vars := filepath.Join(stateDir(), "OVMF_VARS.fd")
	if _, err := os.Stat(vars); errors.Is(err, os.ErrNotExist) {
		if err := copyFile(ovmfVars, vars, 0o600); err != nil {
			return err
		}
	}
	pubKey, err := os.ReadFile(keyPath() + ".pub")
	if err != nil {
		return err
	}
	hostKey, err := os.ReadFile(hostKeyPath())
	if err != nil {
		return err
	}

	// Runtime shares do not survive a VM restart.
	os.RemoveAll(sharesStateDir())

	// Configured shares, plus the guest flake that "hanare switch" builds from.
	type mount struct {
		share  Share
		target string
	}
	mounts := []mount{{Share{Path: cfg.Guest.Flake, ReadOnly: true}, "/run/hanare-config"}}
	for _, s := range cfg.Shares {
		p, err := cfg.resolveShare(s.Path)
		if err != nil {
			return err
		}
		mounts = append(mounts, mount{Share{Path: p, ReadOnly: s.ReadOnly}, p})
	}
	var devArgs []string
	var fstab strings.Builder
	for _, m := range mounts {
		tag := shareTag(m.share.Path)
		sock := filepath.Join(run, tag+".sock")
		os.Remove(sock)
		if err := startHelper(sock, virtiofsd, virtiofsdArgs(sock, m.share.Path, m.share.ReadOnly)...); err != nil {
			return fmt.Errorf("virtiofsd for %s: %w", m.share.Path, err)
		}
		devArgs = append(devArgs,
			"-chardev", "socket,id=fs-"+tag+",path="+sock,
			"-device", "vhost-user-fs-pci,chardev=fs-"+tag+",tag="+tag)
		fmt.Fprintf(&fstab, "%s %s virtiofs %s,nosuid,nodev,nofail,x-systemd.before=hanare-guard.service 0 0\n",
			tag, strings.ReplaceAll(m.target, " ", `\040`), mode(m.share.ReadOnly))
	}
	// Empty PCIe root ports for "hanare share" to hot-plug into.
	for i := 0; i < hotplugPorts; i++ {
		devArgs = append(devArgs, "-device", fmt.Sprintf("pcie-root-port,id=hp%d,chassis=%d", i, i+1))
	}

	// vsock through a vhost-user backend: the host side is an AF_UNIX socket
	// in our 0700 runtime dir instead of AF_VSOCK, which any host user can
	// connect to, and the host kernel never parses guest vsock packets.
	vsockCtl := filepath.Join(run, "vhost-vsock.sock")
	os.Remove(vsockCtl)
	os.Remove(vsockSock())
	if err := startHelper(vsockCtl, vhostVsock(),
		"--guest-cid", strconv.FormatUint(uint64(cfg.VM.VsockCID), 10),
		"--socket", vsockCtl, "--uds-path", vsockSock()); err != nil {
		return fmt.Errorf("vhost-device-vsock: %w", err)
	}

	cred := func(name string, data []byte) []string {
		return []string{"-smbios", "type=11,value=io.systemd.credential.binary:" + name + "=" +
			base64.StdEncoding.EncodeToString(data)}
	}
	args := []string{qemu,
		"-name", "hanare",
		"-nodefaults",
		"-machine", "q35,accel=kvm,memory-backend=mem",
		// No nested virtualization: keeps the guest away from KVM's nested
		// code paths (e.g. CVE-2026-53359, "Januscape").
		"-cpu", "host,-vmx,-svm",
		"-smp", strconv.Itoa(cfg.VM.CPUs), "-m", cfg.VM.Memory,
		"-object", "memory-backend-memfd,id=mem,size=" + cfg.VM.Memory + ",share=on",
		"-drive", "if=pflash,format=raw,readonly=on,file=" + ovmfCode,
		"-drive", "if=pflash,format=raw,file=" + vars,
		"-drive", "if=virtio,format=raw,discard=unmap,file=" + diskPath(),
		"-netdev", "stream,id=net0,server=off,addr.type=unix,addr.path=" + filepath.Join(run, "passt.sock"),
		"-device", "virtio-net-pci,netdev=net0",
		"-chardev", "socket,id=vsock0,path=" + vsockCtl,
		"-device", "vhost-user-vsock-pci,chardev=vsock0",
	}
	// Guest systemd reads fstab.extra as extra fstab entries; the SSH keys are
	// installed by hanare-ssh-keys.service, so the guest flake holds no key.
	args = append(args, cred("fstab.extra", []byte(fstab.String()))...)
	args = append(args, cred("hanare.ssh.authorized_keys", pubKey)...)
	args = append(args, cred("hanare.ssh.host_key", hostKey)...)
	args = append(args, devArgs...)
	args = append(args,
		"-device", "virtio-serial-pci",
		"-chardev", "file,id=con0,path="+filepath.Join(run, "console.log"),
		"-device", "virtconsole,chardev=con0",
		"-vga", "none", "-display", "none", "-serial", "none", "-monitor", "none",
		"-qmp", "unix:"+filepath.Join(run, "qmp.sock")+",server=on,wait=off",
		"-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny",
	)
	return syscall.Exec(qemu, args, os.Environ())
}

// startHelper starts a background process and waits for its socket to appear.
func startHelper(sock, path string, args ...string) error {
	cmd := exec.Command(path, args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	return waitSocket(sock)
}

func waitSocket(sock string) error {
	for i := 0; i < 100; i++ {
		if fi, err := os.Stat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("socket %s did not appear", sock)
}

func up() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if !unitActive() {
		if err := startUnit(); err != nil {
			return err
		}
	}
	client, err := cfg.waitReady(120 * time.Second)
	if err != nil {
		return err
	}
	client.Close()
	fmt.Println("hanare: up")
	return nil
}

func down() error {
	if !unitActive() {
		fmt.Println("hanare: not running")
		return nil
	}
	if err := qmpOnce("system_powerdown"); err != nil {
		fmt.Fprintf(os.Stderr, "hanare: ACPI shutdown failed (%v), stopping unit\n", err)
	} else {
		for i := 0; i < 120 && unitActive(); i++ {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if unitActive() {
		return exec.Command("systemctl", "--user", "stop", unit).Run()
	}
	fmt.Println("hanare: down")
	return nil
}

func status() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if !unitActive() {
		fmt.Println("hanare: stopped")
		return nil
	}
	start := time.Now()
	client, err := cfg.dial()
	if err != nil {
		fmt.Printf("hanare: running, SSH not ready (%v)\n", err)
		return nil
	}
	defer client.Close()
	fmt.Printf("hanare: running, SSH ok (%d ms)\n", time.Since(start).Milliseconds())
	if sess, err := client.NewSession(); err == nil {
		out, _ := sess.Output("systemctl show -p ActiveState -p Result --value hanare-verify-store.service | paste -sd ' '")
		sess.Close()
		fmt.Printf("hanare: /nix verification: %s\n", verifyState(strings.TrimSpace(string(out))))
	}
	return nil
}

func verifyState(s string) string {
	switch s {
	case "inactive success":
		return "ok"
	case "activating success":
		return "running"
	case "failed exit-code", "inactive exit-code":
		return "FAILED (see: hanare run journalctl -u hanare-verify-store)"
	}
	return s
}

func logs() error {
	f, err := os.Open(filepath.Join(runDir(), "console.log"))
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(os.Stdout, f)
	return err
}

// reinstall builds .#image from ~/.config/hanare and replaces the VM disk,
// discarding /persist (the guest's Nix store and host keys).
func reinstall(yes bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if unitActive() {
		return errors.New("VM is running; run \"hanare down\" first")
	}
	if _, err := os.Stat(diskPath()); err == nil && !yes &&
		!confirm("Replace the VM disk? The guest's /nix and /persist are discarded.") {
		return errors.New("aborted")
	}
	if err := checkGuestFlake(cfg, false); err != nil {
		return err
	}
	out, err := nixBuild(cfg.Guest.Flake, "nixosConfigurations.hanare.config.system.build.images.raw-efi")
	if err != nil {
		return err
	}
	imgs, _ := filepath.Glob(filepath.Join(out, "*.img"))
	if len(imgs) != 1 {
		return fmt.Errorf("expected one .img in %s", out)
	}
	size, err := parseSize(cfg.VM.DiskSize)
	if err != nil {
		return err
	}
	tmp := diskPath() + ".new"
	if err := exec.Command("cp", "--sparse=always", imgs[0], tmp).Run(); err != nil {
		return fmt.Errorf("copy image: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	if err := os.Truncate(tmp, size); err != nil {
		return err
	}
	if err := os.Rename(tmp, diskPath()); err != nil {
		return err
	}
	// A fresh guest gets a fresh host key; the host generates and pins it.
	if err := ensureHostKey(true); err != nil {
		return err
	}
	fmt.Println("hanare: disk replaced:", diskPath())
	return nil
}

// selfUpdate builds ~/.local/src/hanare and installs hanare to ~/.local/bin
// and the vsock backend to ~/.local/libexec/hanare.
func selfUpdate() error {
	for _, t := range []struct{ attr, bin, dst string }{
		{"default", "hanare", filepath.Join(binDir(), "hanare")},
		{"vhost-device-vsock", "vhost-device-vsock", vhostVsock()},
	} {
		out, err := nixBuild(srcDir(), t.attr)
		if err != nil {
			return err
		}
		if err := installFile(filepath.Join(out, "bin", t.bin), t.dst, 0o755); err != nil {
			return err
		}
		fmt.Println("hanare: installed", t.dst)
	}
	return nil
}

func installFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".new")
	os.Remove(tmp)
	if err := copyFile(src, tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// nixBuild builds flake#attr and returns its output path as seen from the host.
func nixBuild(flake, attr string) (string, error) {
	// "^out": only the main output (packages may also have man/doc outputs).
	build := exec.Command("nix", "build", "--no-link", "--print-out-paths", "path:"+flake+"#"+attr+"^out")
	build.Stderr = os.Stderr
	out, err := build.Output()
	if err != nil {
		return "", fmt.Errorf("nix build: %w", err)
	}
	root, err := nixStoreRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, strings.TrimSpace(string(out))), nil
}

// nixStoreRoot returns the host directory backing /nix/store for a
// "local?root=..." store (the rootless setup), or "" for a real /nix.
func nixStoreRoot() (string, error) {
	out, err := exec.Command("nix", "config", "show", "store").Output()
	if err != nil {
		return "", fmt.Errorf("nix config show store: %w", err)
	}
	s := strings.TrimSpace(string(out))
	if i := strings.IndexByte(s, '?'); i >= 0 {
		q, err := url.ParseQuery(s[i+1:])
		if err != nil {
			return "", err
		}
		return q.Get("root"), nil
	}
	return "", nil
}

type qmpConn struct {
	c   net.Conn
	dec *json.Decoder
}

// dialQMP connects to QEMU's monitor socket and negotiates capabilities.
func dialQMP() (*qmpConn, error) {
	c, err := net.DialTimeout("unix", filepath.Join(runDir(), "qmp.sock"), 2*time.Second)
	if err != nil {
		return nil, err
	}
	m := &qmpConn{c: c, dec: json.NewDecoder(c)}
	var greeting map[string]any
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := m.dec.Decode(&greeting); err != nil {
		c.Close()
		return nil, err
	}
	if _, err := m.do("qmp_capabilities", nil); err != nil {
		c.Close()
		return nil, err
	}
	return m, nil
}

func (m *qmpConn) close() { m.c.Close() }

// do runs one command, skipping asynchronous events.
func (m *qmpConn) do(cmd string, args any) (json.RawMessage, error) {
	m.c.SetDeadline(time.Now().Add(10 * time.Second))
	req := map[string]any{"execute": cmd}
	if args != nil {
		req["arguments"] = args
	}
	if err := json.NewEncoder(m.c).Encode(req); err != nil {
		return nil, err
	}
	for {
		var msg struct {
			Event  string          `json:"event"`
			Return json.RawMessage `json:"return"`
			Error  *struct {
				Desc string `json:"desc"`
			} `json:"error"`
		}
		if err := m.dec.Decode(&msg); err != nil {
			return nil, err
		}
		switch {
		case msg.Event != "":
			continue
		case msg.Error != nil:
			return nil, fmt.Errorf("%s: %s", cmd, msg.Error.Desc)
		default:
			return msg.Return, nil
		}
	}
}

func (m *qmpConn) waitEvent(name string, timeout time.Duration) error {
	m.c.SetDeadline(time.Now().Add(timeout))
	for {
		var msg struct {
			Event string `json:"event"`
		}
		if err := m.dec.Decode(&msg); err != nil {
			return err
		}
		if msg.Event == name {
			return nil
		}
	}
}

func qmpOnce(cmd string) error {
	m, err := dialQMP()
	if err != nil {
		return err
	}
	defer m.close()
	_, err = m.do(cmd, nil)
	return err
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
