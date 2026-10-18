package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Runtime shares are hot-plugged into the running VM: virtiofsd runs in its
// own sandboxed user unit, QEMU gets a vhost-user-fs device on a spare PCIe
// root port, and the guest mounts it through hanare-mount@.service.

const hotplugPorts = 8

func sharesStateDir() string { return filepath.Join(runDir(), "shares") }

func runtimeShares() []Share {
	files, _ := filepath.Glob(filepath.Join(sharesStateDir(), "*.json"))
	var out []Share
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s Share
		if json.Unmarshal(b, &s) == nil {
			out = append(out, s)
		}
	}
	return out
}

func shareUnit(tag string) string { return "hanare-share-" + tag + ".service" }

// mountUnit names the guest unit that mounts tag at path.
func mountUnit(tag, path string, ro bool) (string, error) {
	mode := "rw"
	if ro {
		mode = "ro"
	}
	out, err := exec.Command("systemd-escape", "--template=hanare-mount@.service",
		tag+"|"+mode+"|"+path).Output()
	if err != nil {
		return "", fmt.Errorf("systemd-escape: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func share(args []string) error {
	var ro, save bool
	var paths []string
	for _, a := range args {
		switch a {
		case "--ro":
			ro = true
		case "--save":
			save = true
		default:
			paths = append(paths, a)
		}
	}
	if len(paths) == 0 {
		return listShares()
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if !unitActive() {
		return errors.New("VM is not running (start it with \"hanare up\", or add a [[share]] to hanare.toml)")
	}
	for _, p := range paths {
		real, err := cfg.resolveShare(p)
		if err != nil {
			return err
		}
		for _, s := range cfg.activeShares() {
			if within(real, s.Path) || within(s.Path, real) {
				return fmt.Errorf("%s overlaps existing share %s", real, s.Path)
			}
		}
		if err := hotplug(cfg, real, ro); err != nil {
			return fmt.Errorf("%s: %w", real, err)
		}
		fmt.Println("hanare: shared", real, map[bool]string{true: "(read-only)", false: ""}[ro])
		if save {
			if err := appendShare(Share{Path: real, ReadOnly: ro}); err != nil {
				return err
			}
		}
	}
	return nil
}

func hotplug(cfg *Config, path string, ro bool) error {
	tag := shareTag(path)
	sock, err := startShare(path, ro, true)
	if err != nil {
		return err
	}

	m, err := dialQMP()
	if err != nil {
		stopUnit(shareUnit(tag))
		return err
	}
	defer m.close()
	if _, err := m.do("chardev-add", map[string]any{
		"id": "fs-" + tag,
		"backend": map[string]any{"type": "socket", "data": map[string]any{
			"addr":   map[string]any{"type": "unix", "data": map[string]any{"path": sock}},
			"server": false,
		}},
	}); err != nil {
		stopUnit(shareUnit(tag))
		return err
	}
	plugged := false
	for i := 0; i < hotplugPorts && !plugged; i++ {
		_, err = m.do("device_add", map[string]any{
			"driver": "vhost-user-fs-pci", "id": "dev-" + tag,
			"chardev": "fs-" + tag, "tag": tag, "bus": "hp" + strconv.Itoa(i),
		})
		plugged = err == nil
	}
	if !plugged {
		m.do("chardev-remove", map[string]any{"id": "fs-" + tag})
		stopUnit(shareUnit(tag))
		return fmt.Errorf("no free hotplug slot (max %d runtime shares): %w", hotplugPorts, err)
	}

	if err := os.MkdirAll(sharesStateDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(Share{Path: path, ReadOnly: ro})
	if err := os.WriteFile(filepath.Join(sharesStateDir(), tag+".json"), b, 0o600); err != nil {
		return err
	}

	mu, err := mountUnit(tag, path, ro)
	if err != nil {
		return err
	}
	// The guest user creates the mount point; the root-side unit only mounts
	// on directories it owns below its home.
	if rc := cfg.remote("mkdir -p "+shellQuote(path)+" && systemctl start "+shellQuote(mu), false); rc != 0 {
		unplug(cfg, path)
		return fmt.Errorf("guest mount failed (exit %d)", rc)
	}
	return nil
}

func unshare(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	for _, p := range args {
		real, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		if r, err := filepath.EvalSymlinks(real); err == nil {
			real = r
		}
		if _, err := os.Stat(filepath.Join(sharesStateDir(), shareTag(real)+".json")); err != nil {
			return fmt.Errorf("%s is not a runtime share (configured shares are changed in hanare.toml)", real)
		}
		if err := unplug(cfg, real); err != nil {
			return err
		}
		fmt.Println("hanare: unshared", real)
	}
	return nil
}

func unplug(cfg *Config, path string) error {
	tag := shareTag(path)
	state := filepath.Join(sharesStateDir(), tag+".json")
	var s Share
	if b, err := os.ReadFile(state); err == nil {
		json.Unmarshal(b, &s)
	}
	if mu, err := mountUnit(tag, path, s.ReadOnly); err == nil {
		cfg.remote("systemctl stop "+shellQuote(mu), false)
	}
	if m, err := dialQMP(); err == nil {
		if _, err := m.do("device_del", map[string]any{"id": "dev-" + tag}); err == nil {
			m.waitEvent("DEVICE_DELETED", 10*time.Second)
		}
		m.do("chardev-remove", map[string]any{"id": "fs-" + tag})
		m.close()
	}
	stopUnit(shareUnit(tag))
	os.Remove(state)
	return nil
}

func listShares() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	for _, s := range cfg.Shares {
		p, err := cfg.resolveShare(s.Path)
		if err != nil {
			fmt.Printf("%s\tconfigured\tINVALID: %v\n", s.Path, err)
			continue
		}
		fmt.Printf("%s\tconfigured\t%s\n", p, mode(s.ReadOnly))
	}
	for _, s := range runtimeShares() {
		fmt.Printf("%s\truntime\t%s\n", s.Path, mode(s.ReadOnly))
	}
	return nil
}

func mode(ro bool) string {
	if ro {
		return "ro"
	}
	return "rw"
}

func tildePath(p string) string {
	if within(p, home()) {
		return "~" + strings.TrimPrefix(p, home())
	}
	return p
}

func stopUnit(name string) {
	exec.Command("systemctl", "--user", "stop", name).Run()
}

func virtiofsdArgs(sock, path string, ro bool) []string {
	// Identity-map only our own UID/GID into virtiofsd's namespace (as guest
	// UID/GID 1000): guest writes land on the host as our user.
	args := []string{
		"--socket-path=" + sock,
		"--shared-dir=" + path,
		"--sandbox=namespace",
		"--uid-map=:1000:" + strconv.Itoa(os.Getuid()) + ":1:",
		"--gid-map=:1000:" + strconv.Itoa(os.Getgid()) + ":1:",
	}
	if ro {
		args = append(args, "--readonly")
	}
	return args
}
