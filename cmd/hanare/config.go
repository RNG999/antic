package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config mirrors ~/.config/hanare/hanare.toml (host side). Guest-side
// settings live in the guest flake (see nix/guest.nix).
type Config struct {
	Guest struct {
		Flake string `toml:"flake"`
	} `toml:"guest"`
	VM struct {
		CPUs     int    `toml:"cpus"`
		Memory   string `toml:"memory"`
		DiskSize string `toml:"disk_size"`
		VsockCID uint32 `toml:"vsock_cid"`
	} `toml:"vm"`
	Shares   []Share `toml:"share"`
	Commands struct {
		Expose []string `toml:"expose"`
	} `toml:"commands"`
}

// Share is a host directory exposed to the guest at the same path.
type Share struct {
	Path     string `toml:"path" json:"path"`
	ReadOnly bool   `toml:"read_only" json:"read_only,omitempty"`
}

const unit = "hanare.service"

func home() string        { h, _ := os.UserHomeDir(); return h }
func configDir() string   { return filepath.Join(home(), ".config/hanare") }
func configFile() string  { return filepath.Join(configDir(), "hanare.toml") }
func srcDir() string      { return filepath.Join(home(), ".local/src/hanare") }
func stateDir() string    { return filepath.Join(home(), ".local/state/hanare") }
func binDir() string      { return filepath.Join(home(), ".local/bin") }
func diskPath() string    { return filepath.Join(stateDir(), "hanare.raw") }
func keyPath() string     { return filepath.Join(stateDir(), "ssh/id_ed25519") }
func hostKeyPath() string { return filepath.Join(stateDir(), "host_key.pub") }

// guestUser is the login user in the guest: the same name as the host user
// (hanare.user in the guest flake), which owns the shares as UID 1000.
func guestUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

func runDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "hanare")
	}
	return filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "hanare")
}

func loadConfig() (*Config, error) {
	var c Config
	if _, err := toml.DecodeFile(configFile(), &c); err != nil {
		return nil, fmt.Errorf("%s: %w", configFile(), err)
	}
	if c.Guest.Flake == "" {
		c.Guest.Flake = filepath.Join(configDir(), "guest")
	}
	c.Guest.Flake = expandHome(c.Guest.Flake)
	if c.VM.CPUs == 0 {
		c.VM.CPUs = 2
	}
	if c.VM.Memory == "" {
		c.VM.Memory = "4G"
	}
	if c.VM.DiskSize == "" {
		c.VM.DiskSize = "20G"
	}
	if c.VM.VsockCID == 0 {
		c.VM.VsockCID = 4242
	}
	if c.VM.VsockCID < 3 {
		return nil, fmt.Errorf("%s: vm.vsock_cid must be >= 3", configFile())
	}
	// Shares appear in the guest at their host path, under the guest user's home.
	if home() != "/home/"+guestUser() {
		return nil, fmt.Errorf("home directory %s is not /home/%s", home(), guestUser())
	}
	return &c, nil
}

// appendShare records a share in hanare.toml, keeping the file's comments.
func appendShare(s Share) error {
	f, err := os.OpenFile(configFile(), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	block := fmt.Sprintf("\n[[share]]\npath = %q\n", tildePath(s.Path))
	if s.ReadOnly {
		block += "read_only = true\n"
	}
	if _, err := f.WriteString(block); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return filepath.Join(home(), strings.TrimPrefix(p, "~"))
	}
	return p
}

// sensitive lists directories that must never be shared, nor any directory
// containing them: secrets, and files the host executes or trusts (a guest
// that could write these would escape to the host).
func sensitive() []string {
	h := home()
	return []string{
		filepath.Join(h, ".ssh"),
		filepath.Join(h, ".gnupg"),
		filepath.Join(h, ".claude"),
		filepath.Join(h, ".config"),
		filepath.Join(h, ".local"),
	}
}

// resolveShare validates a share path and returns its real absolute path.
func resolveShare(p string) (string, error) {
	abs, err := filepath.Abs(expandHome(p))
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", real)
	}
	if !within(real, home()) || real == home() {
		return "", fmt.Errorf("%s: only directories below %s can be shared", real, home())
	}
	for _, s := range sensitive() {
		if within(real, s) || within(s, real) {
			return "", fmt.Errorf("%s: refusing to share (overlaps %s)", real, s)
		}
	}
	return real, nil
}

// within reports whether path equals dir or lies below it.
func within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+"/")
}

// shareTag derives the virtiofs tag (at most 36 bytes) from the host path.
func shareTag(path string) string {
	sum := sha256.Sum256([]byte(path))
	return "s-" + hex.EncodeToString(sum[:6])
}

// activeShares returns the configured shares plus those added at runtime.
func (c *Config) activeShares() []Share {
	var out []Share
	for _, s := range c.Shares {
		if p, err := resolveShare(s.Path); err == nil {
			out = append(out, Share{Path: p, ReadOnly: s.ReadOnly})
		}
	}
	return append(out, runtimeShares()...)
}

// guestDir returns the current directory, which must lie inside a share; the
// guest sees shares at the same path.
func (c *Config) guestDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(wd); err == nil {
		wd = real
	}
	var paths []string
	for _, s := range c.activeShares() {
		if within(wd, s.Path) {
			return wd, nil
		}
		paths = append(paths, s.Path)
	}
	return "", fmt.Errorf("%s is not inside a shared directory (%s); see \"hanare share\"", wd, strings.Join(paths, ", "))
}

// parseSize parses sizes like "20G" or "512M" (base 1024).
func parseSize(s string) (int64, error) {
	mult := int64(1)
	switch strings.ToUpper(s[len(s)-1:]) {
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	case "T":
		mult = 1 << 40
	}
	if mult > 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n * mult, nil
}
