package host

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

// Units started by "hanare up", all transient user services:
//
//	hanare.service              QEMU itself
//	hanare-net.service          pasta + passt (guest networking, LAN block)
//	hanare-vsock.service        vhost-device-vsock (SSH transport)
//	hanare-share-<tag>.service  one virtiofsd per shared directory
//
// Helpers are PartOf=hanare.service, so stopping the VM stops them.
const (
	netUnit   = "hanare-net.service"
	vsockUnit = "hanare-vsock.service"
)

// sandboxProps confines a unit to the given $HOME paths: QEMU or a helper
// that is compromised sees nothing else from $HOME and cannot write outside.
func sandboxProps(rw, ro []string) []string {
	props := []string{
		"-p", "ProtectHome=tmpfs",
		"-p", "ProtectSystem=strict",
		"-p", "PrivateTmp=yes",
	}
	if len(rw) > 0 {
		props = append(props, "-p", "BindPaths="+strings.Join(rw, " "))
	}
	if len(ro) > 0 {
		props = append(props, "-p", "BindReadOnlyPaths="+strings.Join(ro, " "))
	}
	return props
}

// runUnit starts a transient user service running argv.
func runUnit(name, description string, props []string, argv ...string) error {
	args := []string{"--user", "--quiet", "--collect", "--unit=" + name, "--description=" + description}
	args = append(args, props...)
	args = append(append(args, "--"), argv...)
	if out, err := exec.Command("systemd-run", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("start %s: %v: %s", name, err, out)
	}
	return nil
}

// stopAll stops the VM and every helper unit, including leftovers.
func stopAll() {
	exec.Command("systemctl", "--user", "stop", unit, "hanare-*.service").Run()
}

func startUnit() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := ensureHostKey(false); err != nil {
		return err
	}
	stopAll()
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
	// Runtime shares do not survive a VM restart.
	os.RemoveAll(sharesStateDir())

	args, err := qemuArgs(cfg)
	if err == nil {
		err = startNet(cfg)
	}
	if err == nil {
		err = startVsock(cfg)
	}
	if err == nil {
		err = runUnit(unit, "hanare VM", append(sandboxProps(
			[]string{stateDir(), run},
			[]string{cfg.Guest.Flake},
		),
			"-p", "NoNewPrivileges=yes",
			// QEMU only talks over AF_UNIX (networking is passt's).
			"-p", "RestrictAddressFamilies=AF_UNIX",
			"-p", "LockPersonality=yes",
			"-p", "RestrictRealtime=yes",
			"-p", "RestrictSUIDSGID=yes",
			"-p", "SystemCallArchitectures=native",
			// The client key logs into the guest; QEMU never needs it.
			"-p", "InaccessiblePaths="+keyPath(),
		), args...)
	}
	if err != nil {
		stopAll()
	}
	return err
}

// startShare starts virtiofsd for path as hanare-share-<tag>.service and
// returns its vhost-user socket. hotplugged shares bind to the running VM.
func startShare(path string, ro, hotplugged bool) (string, error) {
	tag := shareTag(path)
	sock := filepath.Join(runDir(), tag+".sock")
	os.Remove(sock)
	rw, roPaths := []string{runDir()}, []string{}
	if ro {
		roPaths = append(roPaths, path)
	} else {
		rw = append(rw, path)
	}
	props := append(sandboxProps(rw, roPaths), "-p", "NoNewPrivileges=yes")
	if hotplugged {
		props = append(props, "-p", "BindsTo="+unit, "-p", "After="+unit)
	} else {
		props = append(props, "-p", "PartOf="+unit)
	}
	if err := runUnit(shareUnit(tag), "hanare share "+path, props,
		append([]string{virtiofsd}, virtiofsdArgs(sock, path, ro)...)...); err != nil {
		return "", err
	}
	if err := waitSocket(sock); err != nil {
		stopUnit(shareUnit(tag))
		return "", err
	}
	return sock, nil
}

// startNet runs passt inside a rootless network namespace provided by pasta.
// Unless network.allow_lan is set, nftables in that namespace first blocks
// LAN destinations: the filter sits outside the guest, so guest root cannot
// remove it. Neither maps the host's loopback; the guest's DNS is forwarded
// through pasta to the host resolver.
func startNet(cfg *Config) error {
	sock := filepath.Join(runDir(), "passt.sock")
	os.Remove(sock)
	passtCmd := []string{passt, "--foreground", "--quiet", "--one-off",
		"--socket", sock, "--no-map-gw",
		"--dns", guestDNS, "--dns-forward", guestDNS, "--dns-host", nsDNS}
	inner := passtCmd
	if !cfg.Network.AllowLAN {
		rules := filepath.Join(runDir(), "lan-block.nft")
		if err := os.WriteFile(rules, []byte(lanBlockRules), 0o600); err != nil {
			return err
		}
		quoted := make([]string, len(passtCmd))
		for i, a := range passtCmd {
			quoted[i] = shellQuote(a)
		}
		inner = []string{"/bin/sh", "-c", nft + " -f " + shellQuote(rules) + " && exec " + strings.Join(quoted, " ")}
	}
	// No NoNewPrivileges here: it stops passt from setting up its own user
	// namespace inside pasta's. passt and pasta drop privileges and apply
	// seccomp themselves.
	props := append(sandboxProps([]string{runDir()}, nil), "-p", "PartOf="+unit)
	argv := append([]string{pasta, "--config-net", "--quiet",
		"--no-map-gw", "--map-host-loopback", "none", "--dns-forward", nsDNS, "--"}, inner...)
	if err := runUnit(netUnit, "hanare network (pasta + passt)", props, argv...); err != nil {
		return err
	}
	return waitSocket(sock)
}

// startVsock runs vhost-device-vsock: the host side of the guest's vsock is
// an AF_UNIX socket in our 0700 runtime dir instead of AF_VSOCK, which any
// host user can connect to, and the host kernel never parses guest vsock
// packets.
func startVsock(cfg *Config) error {
	ctl := filepath.Join(runDir(), "vhost-vsock.sock")
	os.Remove(ctl)
	os.Remove(vsockSock())
	props := append(sandboxProps([]string{runDir()}, []string{vhostVsock()}),
		"-p", "NoNewPrivileges=yes", "-p", "RestrictAddressFamilies=AF_UNIX", "-p", "PartOf="+unit)
	if err := runUnit(vsockUnit, "hanare vsock (vhost-device-vsock)", props,
		vhostVsock(), "--guest-cid", strconv.FormatUint(uint64(cfg.VM.VsockCID), 10),
		"--socket", ctl, "--uds-path", vsockSock()); err != nil {
		return err
	}
	return waitSocket(ctl)
}

// qemuArgs starts the configured shares and returns the QEMU command line.
func qemuArgs(cfg *Config) ([]string, error) {
	run := runDir()
	pubKey, err := os.ReadFile(keyPath() + ".pub")
	if err != nil {
		return nil, err
	}
	hostKey, err := os.ReadFile(hostKeyPath())
	if err != nil {
		return nil, err
	}

	// Configured shares, plus the guest flake that "hanare switch" builds from.
	type mount struct {
		share  Share
		target string
	}
	mounts := []mount{{Share{Path: cfg.Guest.Flake, ReadOnly: true}, "/run/hanare-config"}}
	for _, s := range cfg.Shares {
		p, err := cfg.resolveShare(s.Path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", configFile(), err)
		}
		mounts = append(mounts, mount{Share{Path: p, ReadOnly: s.ReadOnly}, p})
	}
	var devArgs []string
	var fstab strings.Builder
	for _, m := range mounts {
		sock, err := startShare(m.share.Path, m.share.ReadOnly, false)
		if err != nil {
			return nil, fmt.Errorf("share %s: %w", m.share.Path, err)
		}
		tag := shareTag(m.share.Path)
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
		"-drive", "if=pflash,format=raw,file=" + filepath.Join(stateDir(), "OVMF_VARS.fd"),
		"-drive", "if=virtio,format=raw,discard=unmap,file=" + diskPath(),
		"-netdev", "stream,id=net0,server=off,addr.type=unix,addr.path=" + filepath.Join(run, "passt.sock"),
		"-device", "virtio-net-pci,netdev=net0",
		"-chardev", "socket,id=vsock0,path=" + filepath.Join(run, "vhost-vsock.sock"),
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
	return args, nil
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
	stopAll()
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
