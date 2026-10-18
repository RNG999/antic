package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config mirrors ~/.config/hanare/hanare.json, which the guest's
// configuration.nix reads as well.
type Config struct {
	User   string   `json:"user"`
	SSHKey string   `json:"ssh_authorized_key"`
	CID    uint32   `json:"cid"`
	CPUs   int      `json:"cpus"`
	Memory string   `json:"memory"`
	Disk   string   `json:"disk"`
	LAN    bool     `json:"lan"`
	Shares []Share  `json:"shares"`
	Links  []string `json:"links"`
}

// Share is a host directory exposed to the guest at the same path.
type Share struct {
	Path string `json:"path"`
	RO   bool   `json:"ro,omitempty"`
}

const unit = "hanare.service"

func home() string        { h, _ := os.UserHomeDir(); return h }
func configDir() string   { return filepath.Join(home(), ".config/hanare") }
func configFile() string  { return filepath.Join(configDir(), "hanare.json") }
func stateDir() string    { return filepath.Join(home(), ".local/state/hanare") }
func binDir() string      { return filepath.Join(home(), ".local/bin") }
func diskPath() string    { return filepath.Join(stateDir(), "hanare.raw") }
func keyPath() string     { return filepath.Join(stateDir(), "ssh/id_ed25519") }
func hostKeyPath() string { return filepath.Join(stateDir(), "host_key.pub") }

func runDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "hanare")
	}
	return filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "hanare")
}

func loadConfig() (*Config, error) {
	b, err := os.ReadFile(configFile())
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("hanare.json: %w", err)
	}
	if c.User == "" || c.CID < 3 || c.CPUs < 1 || c.Memory == "" || c.Disk == "" {
		return nil, fmt.Errorf("hanare.json: user, cid, cpus, memory and disk are required")
	}
	// Shares appear in the guest at their host path, under the guest user's home.
	if home() != "/home/"+c.User {
		return nil, fmt.Errorf("hanare.json: user %q does not match home directory %s", c.User, home())
	}
	return &c, nil
}

func (c *Config) save() error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := configFile() + ".new"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, configFile())
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
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = filepath.Join(home(), strings.TrimPrefix(p, "~"))
	}
	abs, err := filepath.Abs(p)
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
			out = append(out, Share{Path: p, RO: s.RO})
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
