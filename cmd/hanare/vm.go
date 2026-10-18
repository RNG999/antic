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
	qemu      = "/usr/bin/qemu-system-x86_64"
	ovmfCode  = "/usr/share/edk2/x64/OVMF_CODE.4m.fd"
	ovmfVars  = "/usr/share/edk2/x64/OVMF_VARS.4m.fd"
	// Guest-visible DNS address; passt forwards queries sent to it to the
	// host's resolver. Taken from TEST-NET-1 so it never clashes with a real host.
	guestDNS = "192.0.2.53"
	// systemd-resolved's stub; passt otherwise sends forwarded queries to 127.0.0.1.
	hostDNS = "127.0.0.53"
)

func unitActive() bool {
	return exec.Command("systemctl", "--user", "is-active", "--quiet", unit).Run() == nil
}

// startUnit runs "hanare _vm" as a transient user service, so the VM and its
// helper processes live in one cgroup that systemd cleans up as a whole.
func startUnit() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// Even if QEMU or a helper is compromised, it sees only the shared
	// directories and hanare's own files from $HOME, and nothing writable
	// outside them.
	rw := []string{stateDir(), runDir()}
	ro := []string{configDir(), exe}
	for _, s := range cfg.Shares {
		p, err := resolveShare(s.Path)
		if err != nil {
			return fmt.Errorf("hanare.json: %w", err)
		}
		if s.RO {
			ro = append(ro, p)
		} else {
			rw = append(rw, p)
		}
	}
	if err := os.MkdirAll(runDir(), 0o700); err != nil {
		return err
	}
	cmd := exec.Command("systemd-run", "--user", "--quiet", "--collect",
		"--unit="+unit, "--description=hanare VM",
		"-p", "ProtectHome=tmpfs",
		"-p", "ProtectSystem=strict",
		"-p", "PrivateTmp=yes",
		"-p", "NoNewPrivileges=yes",
		"-p", "BindPaths="+strings.Join(rw, " "),
		"-p", "BindReadOnlyPaths="+strings.Join(ro, " "),
		"--", exe, "_vm")
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// execVM starts one virtiofsd per share plus passt, then execs QEMU, which
// becomes the unit's main process. Runs inside hanare.service.
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

	// Runtime shares do not survive a VM restart.
	os.RemoveAll(sharesStateDir())

	// Configured shares, plus the config dir that "hanare switch" builds from.
	type mount struct {
		share  Share
		target string
	}
	mounts := []mount{{Share{Path: configDir(), RO: true}, "/run/hanare-config"}}
	for _, s := range cfg.Shares {
		p, err := resolveShare(s.Path)
		if err != nil {
			return err
		}
		mounts = append(mounts, mount{Share{Path: p, RO: s.RO}, p})
	}
	var fsArgs []string
	var fstab strings.Builder
	for _, m := range mounts {
		tag := shareTag(m.share.Path)
		sock := filepath.Join(run, tag+".sock")
		os.Remove(sock)
		if err := startHelper(sock, virtiofsd, virtiofsdArgs(sock, m.share.Path, m.share.RO)...); err != nil {
			return fmt.Errorf("virtiofsd for %s: %w", m.share.Path, err)
		}
		fsArgs = append(fsArgs,
			"-chardev", "socket,id=fs-"+tag+",path="+sock,
			"-device", "vhost-user-fs-pci,chardev=fs-"+tag+",tag="+tag)
		fmt.Fprintf(&fstab, "%s %s virtiofs %s,nosuid,nodev,nofail,x-systemd.before=hanare-gitguard.service 0 0\n",
			tag, strings.ReplaceAll(m.target, " ", `\040`), mode(m.share.RO))
	}
	// Empty PCIe root ports for "hanare share" to hot-plug into.
	for i := 0; i < hotplugPorts; i++ {
		fsArgs = append(fsArgs, "-device", fmt.Sprintf("pcie-root-port,id=hp%d,chassis=%d", i, i+1))
	}

	// passt without --map-host-loopback: the guest cannot reach services
	// bound to the host's loopback; only DNS is forwarded explicitly.
	netSock := filepath.Join(run, "passt.sock")
	os.Remove(netSock)
	if err := startHelper(netSock, passt, "--foreground", "--quiet", "--one-off",
		"--socket", netSock, "--no-map-gw",
		"--dns", guestDNS, "--dns-forward", guestDNS, "--dns-host", hostDNS); err != nil {
		return fmt.Errorf("passt: %w", err)
	}

	args := []string{qemu,
		"-name", "hanare",
		"-machine", "q35,accel=kvm,memory-backend=mem",
		"-cpu", "host", "-smp", strconv.Itoa(cfg.CPUs), "-m", cfg.Memory,
		"-object", "memory-backend-memfd,id=mem,size=" + cfg.Memory + ",share=on",
		"-drive", "if=pflash,format=raw,readonly=on,file=" + ovmfCode,
		"-drive", "if=pflash,format=raw,file=" + vars,
		"-drive", "if=virtio,format=raw,discard=unmap,file=" + diskPath(),
		"-netdev", "stream,id=net0,server=off,addr.type=unix,addr.path=" + netSock,
		"-device", "virtio-net-pci,netdev=net0",
		"-device", fmt.Sprintf("vhost-vsock-pci,guest-cid=%d", cfg.CID),
		// Guest systemd reads this credential as extra fstab entries.
		"-smbios", "type=11,value=io.systemd.credential.binary:fstab.extra=" +
			base64.StdEncoding.EncodeToString([]byte(fstab.String())),
	}
	args = append(args, fsArgs...)
	args = append(args,
		"-device", "virtio-serial-pci",
		"-chardev", "file,id=con0,path="+filepath.Join(run, "console.log"),
		"-device", "virtconsole,chardev=con0",
		"-display", "none", "-serial", "none", "-monitor", "none",
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
	client.Close()
	fmt.Printf("hanare: running, SSH ok (%d ms)\n", time.Since(start).Milliseconds())
	return nil
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
	out, err := nixBuild("image")
	if err != nil {
		return err
	}
	imgs, _ := filepath.Glob(filepath.Join(out, "*.img"))
	if len(imgs) != 1 {
		return fmt.Errorf("expected one .img in %s", out)
	}
	size, err := parseSize(cfg.Disk)
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
	os.Remove(hostKeyPath())
	fmt.Println("hanare: disk replaced:", diskPath())
	return nil
}

// selfUpdate builds .#hanare and installs it as ~/.local/bin/hanare.
func selfUpdate() error {
	out, err := nixBuild("hanare")
	if err != nil {
		return err
	}
	tmp := filepath.Join(binDir(), ".hanare.new")
	os.Remove(tmp)
	if err := copyFile(filepath.Join(out, "bin/hanare"), tmp, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(binDir(), "hanare")); err != nil {
		return err
	}
	fmt.Println("hanare: installed", filepath.Join(binDir(), "hanare"))
	return nil
}

// nixBuild builds a package of the ~/.config/hanare flake and returns its
// output path as seen from the host.
func nixBuild(attr string) (string, error) {
	build := exec.Command("nix", "build", "--no-link", "--print-out-paths", "path:"+configDir()+"#"+attr)
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
