package host

import (
	sd "github.com/coreos/go-systemd/v22/dbus"

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
	"text/tabwriter"
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

func (g *Guest) unitActive() bool {
	if g.ID == "" {
		return false
	}
	return unitActive(g.unit())
}

// Units started by "hanare guest up" for the guest with ID, all transient
// user services in hanare-ID.slice:
//
//	hanare@ID.service              QEMU itself
//	hanare-net@ID.service          pasta + passt (guest networking, LAN block)
//	hanare-vsock@ID.service        vhost-device-vsock (SSH transport)
//	hanare-share@ID:<tag>.service  one virtiofsd per shared directory
//
// Each runs in its own sandbox with only what it needs. Helpers are PartOf=
// the QEMU unit, so stopping the VM stops them.

// runUnit starts a transient service of the guest (in its slice) running argv.
func (g *Guest) runUnit(name, description string, props []sd.Property, argv ...string) error {
	return startTransient(name, g.slice(), description, props, argv...)
}

// stopAll stops the guest's VM and every helper unit, including leftovers.
func (g *Guest) stopAll() {
	if g.ID != "" {
		stopUnit(g.unit())
		stopUnit(g.slice())
	}
}

// start boots the guest: helpers first, then QEMU.
func (g *Guest) start() error {
	if g.ID == "" {
		return fmt.Errorf("guest %s has no disk yet; run \"hanare guest reset %s\"", g.Name, g.Name)
	}
	if _, err := os.Stat(g.diskPath()); err != nil {
		return fmt.Errorf("guest %s has no disk yet; run \"hanare guest reset %s\"", g.Name, g.Name)
	}
	if err := g.ensureHostKey(false); err != nil {
		return err
	}
	g.stopAll()
	run := g.runDir()
	if err := os.MkdirAll(run, 0o700); err != nil {
		return err
	}
	vars := filepath.Join(g.stateDir(), "OVMF_VARS.fd")
	if _, err := os.Stat(vars); errors.Is(err, os.ErrNotExist) {
		if err := copyFile(ovmfVars, vars, 0o600); err != nil {
			return err
		}
	}
	// Runtime shares do not survive a VM restart.
	os.RemoveAll(g.sharesStateDir())

	args, err := g.qemuArgs()
	if err == nil {
		err = g.startNet()
	}
	if err == nil {
		err = g.startVsock()
	}
	if err == nil {
		// Only this guest's state dir is visible; the shared SSH client key
		// (~/.local/state/hanare/ssh) is not.
		err = g.runUnit(g.unit(), "hanare guest "+g.Name, append(sandboxProps(
			[]string{g.stateDir(), run},
			[]string{g.Flake},
		),
			prop("NoNewPrivileges", true),
			// QEMU only talks over AF_UNIX (networking is passt's).
			unixOnly,
			prop("LockPersonality", true),
			prop("RestrictRealtime", true),
			prop("RestrictSUIDSGID", true),
			prop("SystemCallArchitectures", []string{"native"}),
		), args...)
	}
	if err != nil {
		g.stopAll()
	}
	return err
}

// startShare starts virtiofsd for path as hanare-share-<tag>.service and
// returns its vhost-user socket. hotplugged shares bind to the running VM.
func (g *Guest) startShare(path string, ro, hotplugged bool) (string, error) {
	tag := shareTag(path)
	sock := filepath.Join(g.runDir(), tag+".sock")
	os.Remove(sock)
	rw, roPaths := []string{g.runDir()}, []string{}
	if ro {
		roPaths = append(roPaths, path)
	} else {
		rw = append(rw, path)
	}
	props := append(sandboxProps(rw, roPaths), prop("NoNewPrivileges", true))
	if hotplugged {
		props = append(props, prop("BindsTo", []string{g.unit()}), prop("After", []string{g.unit()}))
	} else {
		props = append(props, partOf(g.unit()))
	}
	if err := g.runUnit(g.shareUnit(tag), "hanare share "+path+" ("+g.Name+")", props,
		append([]string{virtiofsd}, virtiofsdArgs(sock, path, ro)...)...); err != nil {
		return "", err
	}
	if err := waitSocket(sock); err != nil {
		stopUnit(g.shareUnit(tag))
		return "", err
	}
	return sock, nil
}

// startNet runs passt inside a rootless network namespace provided by pasta.
// Unless network.allow_lan is set, nftables in that namespace first blocks
// LAN destinations: the filter sits outside the guest, so guest root cannot
// remove it. Neither maps the host's loopback; the guest's DNS is forwarded
// through pasta to the host resolver.
func (g *Guest) startNet() error {
	sock := filepath.Join(g.runDir(), "passt.sock")
	os.Remove(sock)
	passtCmd := []string{passt, "--foreground", "--quiet", "--one-off",
		"--socket", sock, "--no-map-gw",
		"--dns", guestDNS, "--dns-forward", guestDNS, "--dns-host", nsDNS}
	inner := passtCmd
	if !g.AllowLAN {
		rules := filepath.Join(g.runDir(), "lan-block.nft")
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
	props := append(sandboxProps([]string{g.runDir()}, nil), partOf(g.unit()))
	argv := append([]string{pasta, "--config-net", "--quiet",
		"--no-map-gw", "--map-host-loopback", "none", "--dns-forward", nsDNS, "--"}, inner...)
	if err := g.runUnit(g.netUnit(), "hanare network (pasta + passt) for "+g.Name, props, argv...); err != nil {
		return err
	}
	return waitSocket(sock)
}

// startVsock runs vhost-device-vsock: the host side of the guest's vsock is
// an AF_UNIX socket in our 0700 runtime dir instead of AF_VSOCK, which any
// host user can connect to, and the host kernel never parses guest vsock
// packets.
func (g *Guest) startVsock() error {
	ctl := filepath.Join(g.runDir(), "vhost-vsock.sock")
	os.Remove(ctl)
	os.Remove(g.vsockSock())
	props := append(sandboxProps([]string{g.runDir()}, []string{vhostVsock()}),
		prop("NoNewPrivileges", true), unixOnly, partOf(g.unit()))
	if err := g.runUnit(g.vsockUnit(), "hanare vsock (vhost-device-vsock) for "+g.Name, props,
		vhostVsock(), "--guest-cid", strconv.FormatUint(uint64(g.VsockCID), 10),
		"--socket", ctl, "--uds-path", g.vsockSock()); err != nil {
		return err
	}
	return waitSocket(ctl)
}

// qemuArgs starts the configured shares and returns the QEMU command line.
func (g *Guest) qemuArgs() ([]string, error) {
	run := g.runDir()
	pubKey, err := os.ReadFile(keyPath() + ".pub")
	if err != nil {
		return nil, err
	}
	hostKey, err := os.ReadFile(g.hostKeyPath())
	if err != nil {
		return nil, err
	}

	// Configured shares, plus the guest flake that "hanare guest apply" builds from.
	type mount struct {
		share  Share
		target string
	}
	mounts := []mount{{Share{Path: g.Flake, ReadOnly: true}, "/run/hanare-config"}}
	for _, s := range g.Shares {
		p, err := g.resolveShare(s.Path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", configFile(), err)
		}
		mounts = append(mounts, mount{Share{Path: p, ReadOnly: s.ReadOnly}, p})
	}
	var devArgs []string
	var fstab strings.Builder
	for _, m := range mounts {
		sock, err := g.startShare(m.share.Path, m.share.ReadOnly, false)
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
	// Empty PCIe root ports for "hanare share add" to hot-plug into.
	for i := 0; i < hotplugPorts; i++ {
		devArgs = append(devArgs, "-device", fmt.Sprintf("pcie-root-port,id=hp%d,chassis=%d", i, i+1))
	}

	cred := func(name string, data []byte) []string {
		return []string{"-smbios", "type=11,value=io.systemd.credential.binary:" + name + "=" +
			base64.StdEncoding.EncodeToString(data)}
	}
	args := []string{qemu,
		"-name", "hanare-" + g.Name,
		"-nodefaults",
		"-machine", "q35,accel=kvm,memory-backend=mem",
		// No nested virtualization: keeps the guest away from KVM's nested
		// code paths (e.g. CVE-2026-53359, "Januscape").
		"-cpu", "host,-vmx,-svm",
		"-smp", strconv.Itoa(g.CPUs), "-m", g.Memory,
		"-object", "memory-backend-memfd,id=mem,size=" + g.Memory + ",share=on",
		"-drive", "if=pflash,format=raw,readonly=on,file=" + ovmfCode,
		"-drive", "if=pflash,format=raw,file=" + filepath.Join(g.stateDir(), "OVMF_VARS.fd"),
		"-drive", "if=virtio,format=raw,discard=unmap,file=" + g.diskPath(),
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
	// Which nixosConfigurations.<attr> "hanare guest apply" rebuilds in the guest.
	args = append(args, cred("hanare.flake_attr", []byte(g.Attr))...)
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

func (g *Guest) up() error {
	if !g.unitActive() {
		if err := g.start(); err != nil {
			return err
		}
	}
	client, err := g.waitReady(120 * time.Second)
	if err != nil {
		return err
	}
	client.Close()
	fmt.Printf("hanare: %s is up\n", g.Name)
	return nil
}

func (g *Guest) down() error {
	if !g.unitActive() {
		fmt.Printf("hanare: %s is not running\n", g.Name)
		g.stopAll()
		return nil
	}
	if err := g.qmpOnce("system_powerdown"); err != nil {
		fmt.Fprintf(os.Stderr, "hanare: ACPI shutdown of %s failed (%v), stopping units\n", g.Name, err)
	} else {
		for i := 0; i < 120 && g.unitActive(); i++ {
			time.Sleep(500 * time.Millisecond)
		}
	}
	g.stopAll()
	fmt.Printf("hanare: %s is down\n", g.Name)
	return nil
}

func (g *Guest) status() error {
	fmt.Printf("hanare: %s: %s\n", g.Name, g.state())
	if !g.unitActive() {
		return nil
	}
	start := time.Now()
	client, err := g.dial()
	if err != nil {
		fmt.Printf("hanare: %s: SSH not ready (%v)\n", g.Name, err)
		return nil
	}
	defer client.Close()
	fmt.Printf("hanare: %s: SSH ok (%d ms)\n", g.Name, time.Since(start).Milliseconds())
	if sess, err := client.NewSession(); err == nil {
		out, _ := sess.Output("systemctl show -p ActiveState -p Result --value hanare-verify-store.service | paste -sd ' '")
		sess.Close()
		fmt.Printf("hanare: %s: /nix verification: %s\n", g.Name, verifyState(strings.TrimSpace(string(out))))
	}
	return nil
}

// state describes the guest like "docker ps" does: "Up 5 minutes",
// "Stopped" or "Not created".
func (g *Guest) state() string {
	if !g.unitActive() {
		if g.ID == "" {
			return "Not created"
		}
		if _, err := os.Stat(g.diskPath()); err != nil {
			return "Not created"
		}
		return "Stopped"
	}
	t, err := unitActiveSince(g.unit())
	if err != nil {
		return "Up"
	}
	return "Up " + humanDuration(time.Since(t))
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < 2*time.Minute:
		return "1 minute"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 2*time.Hour:
		return "1 hour"
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

// ps lists all guests in hanare.toml, "docker ps -a" style.
func ps(f *File) error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tOS\tCPUS\tMEMORY\tSHARES\tLAN")
	for _, n := range f.names() {
		g := f.Guests[n]
		name := n
		if n == f.Default {
			name += " (default)"
		}
		lan := "blocked"
		if g.AllowLAN {
			lan = "allowed"
		}
		shares := len(g.Shares)
		if g.unitActive() {
			shares += len(g.runtimeShares())
		}
		id := g.ID
		if id == "" {
			id = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s#%s\t%d\t%s\t%d\t%s\n",
			id, name, g.state(), tildePath(g.Flake), g.Attr, g.CPUs, g.Memory, shares, lan)
	}
	return tw.Flush()
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

func (g *Guest) logs() error {
	f, err := os.Open(filepath.Join(g.runDir(), "console.log"))
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(os.Stdout, f)
	return err
}

// reset builds the guest's image from its flake and replaces its disk,
// discarding /persist (the guest's Nix store).
func (g *Guest) reset(yes bool) error {
	if g.unitActive() {
		return fmt.Errorf("guest %s is running; run \"hanare guest down %s\" first", g.Name, g.Name)
	}
	if _, err := os.Stat(g.diskPath()); g.ID != "" && err == nil && !yes &&
		!confirm(fmt.Sprintf("Replace the disk of %s? The guest's /nix and /persist are discarded.", g.Name)) {
		return errors.New("aborted")
	}
	if err := g.checkFlake(false); err != nil {
		return err
	}
	out, err := nixBuild(g.Flake, "nixosConfigurations."+g.Attr+".config.system.build.images.raw-efi")
	if err != nil {
		return err
	}
	imgs, _ := filepath.Glob(filepath.Join(out, "*.img"))
	if len(imgs) != 1 {
		return fmt.Errorf("expected one .img in %s", out)
	}
	size, err := parseSize(g.DiskSize)
	if err != nil {
		return err
	}
	if err := g.assignID(); err != nil {
		return err
	}
	tmp := g.diskPath() + ".new"
	if err := exec.Command("cp", "--sparse=always", imgs[0], tmp).Run(); err != nil {
		return fmt.Errorf("copy image: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	if err := os.Truncate(tmp, size); err != nil {
		return err
	}
	if err := os.Rename(tmp, g.diskPath()); err != nil {
		return err
	}
	// A fresh guest gets a fresh host key; the host generates and pins it.
	if err := g.ensureHostKey(true); err != nil {
		return err
	}
	fmt.Println("hanare: disk replaced:", g.diskPath())
	return nil
}

// remove deletes the guest's state (disk, UEFI variables, host key).
func (g *Guest) remove(yes bool) error {
	if g.ID == "" {
		return fmt.Errorf("guest %s has no state", g.Name)
	}
	if g.unitActive() {
		return fmt.Errorf("guest %s is running; run \"hanare guest down %s\" first", g.Name, g.Name)
	}
	if !yes && !confirm(fmt.Sprintf("Delete the disk and state of %s (%s)?", g.Name, g.stateDir())) {
		return errors.New("aborted")
	}
	if err := os.RemoveAll(g.stateDir()); err != nil {
		return err
	}
	os.Remove(filepath.Join(namesDir(), g.Name))
	fmt.Printf("hanare: removed %s; its [guests.%s] table in %s is left as is\n", g.stateDir(), g.Name, configFile())
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
func (g *Guest) dialQMP() (*qmpConn, error) {
	c, err := net.DialTimeout("unix", filepath.Join(g.runDir(), "qmp.sock"), 2*time.Second)
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

func (g *Guest) qmpOnce(cmd string) error {
	m, err := g.dialQMP()
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
