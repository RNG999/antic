package host

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// File mirrors ~/.config/hanare/hanare.toml: the guests that can be started,
// and which one commands use by default.
type File struct {
	Default string            `toml:"default"`
	Guests  map[string]*Guest `toml:"guests"`
}

// Guest is one [guests.NAME] table: a VM, its OS (a NixOS configuration in a
// flake) and its host-side settings. Guest-side settings live in that flake
// (see guest/module.nix).
type Guest struct {
	Name string `toml:"-"`
	// ID identifies the guest's state and units; assigned when its disk is
	// first built, so a guest recreated under an old name never picks up the
	// old one's state. Empty until then.
	ID string `toml:"-"`

	Flake string `toml:"flake"` // flake with nixosConfigurations.<attr>
	Attr  string `toml:"attr"`

	CPUs     int    `toml:"cpus"`
	Memory   string `toml:"memory"`
	DiskSize string `toml:"disk_size"`
	VsockCID uint32 `toml:"vsock_cid"`

	AllowLAN bool     `toml:"allow_lan"`
	Expose   []string `toml:"expose"`  // ~/.local/bin links ("hanare link")
	Offline  []string `toml:"offline"` // commands that always run offline
	Shares   []Share  `toml:"share"`

	file *File
	// sshPort selects the guest sshd: 22 (default) or 23 for offline
	// sessions, whose processes cannot reach the network.
	sshPort uint32
}

// Share is a host directory exposed to the guest at the same path.
type Share struct {
	Path     string `toml:"path" json:"path"`
	ReadOnly bool   `toml:"read_only" json:"read_only,omitempty"`
}

var guestName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func home() string       { h, _ := os.UserHomeDir(); return h }
func configDir() string  { return filepath.Join(home(), ".config/hanare") }
func configFile() string { return filepath.Join(configDir(), "hanare.toml") }
func srcDir() string     { return filepath.Join(home(), ".local/src/hanare") }
func stateRoot() string  { return filepath.Join(home(), ".local/state/hanare") }
func binDir() string     { return filepath.Join(home(), ".local/bin") }
func keyPath() string    { return filepath.Join(stateRoot(), "ssh/id_ed25519") } // shared client key
func libexecDir() string { return filepath.Join(home(), ".local/libexec/hanare") }
func vhostVsock() string { return filepath.Join(libexecDir(), "vhost-device-vsock") }

func runRoot() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "hanare")
	}
	return filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "hanare")
}

// State lives in ~/.local/state/hanare/guests/<ID>; names/<NAME> links to it.
func namesDir() string { return filepath.Join(stateRoot(), "names") }

// Per-guest paths, units and slice, all keyed by ID.
func (g *Guest) stateDir() string       { return filepath.Join(stateRoot(), "guests", g.ID) }
func (g *Guest) runDir() string         { return filepath.Join(runRoot(), g.ID) }
func (g *Guest) diskPath() string       { return filepath.Join(g.stateDir(), "disk.raw") }
func (g *Guest) hostKeyPath() string    { return filepath.Join(g.stateDir(), "host_ed25519") }
func (g *Guest) vsockSock() string      { return filepath.Join(g.runDir(), "vsock.sock") }
func (g *Guest) sharesStateDir() string { return filepath.Join(g.runDir(), "shares") }
func (g *Guest) unit() string           { return "hanare@" + g.ID + ".service" }
func (g *Guest) netUnit() string        { return "hanare-net@" + g.ID + ".service" }
func (g *Guest) vsockUnit() string      { return "hanare-vsock@" + g.ID + ".service" }
func (g *Guest) slice() string          { return "hanare-" + g.ID + ".slice" }
func (g *Guest) shareUnit(tag string) string {
	return "hanare-share@" + g.ID + ":" + tag + ".service"
}

// assignID gives a guest without one a new random ID and links its name.
func (g *Guest) assignID() error {
	if g.ID != "" {
		return nil
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	g.ID = hex.EncodeToString(b)
	if err := os.MkdirAll(g.stateDir(), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(namesDir(), 0o700); err != nil {
		return err
	}
	return os.Symlink(filepath.Join("..", "guests", g.ID), filepath.Join(namesDir(), g.Name))
}

// guestUser is the login user in the guest: the same name as the host user
// (hanare.user in the guest flake), which owns the shares as UID 1000.
func guestUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

func loadFile() (*File, error) {
	var f File
	if _, err := toml.DecodeFile(configFile(), &f); err != nil {
		return nil, fmt.Errorf("%s: %w", configFile(), err)
	}
	if len(f.Guests) == 0 {
		return nil, fmt.Errorf("%s: no [guests.NAME] defined", configFile())
	}
	// Shares appear in the guest at their host path, under the guest user's home.
	if home() != "/home/"+guestUser() {
		return nil, fmt.Errorf("home directory %s is not /home/%s", home(), guestUser())
	}
	for name, g := range f.Guests {
		if !guestName.MatchString(name) {
			return nil, fmt.Errorf("%s: invalid guest name %q (use a-z, 0-9 and -)", configFile(), name)
		}
		g.Name, g.file = name, &f
		if dst, err := os.Readlink(filepath.Join(namesDir(), name)); err == nil {
			g.ID = filepath.Base(dst)
		}
		if g.Flake == "" {
			g.Flake = filepath.Join(configDir(), "guest")
		}
		g.Flake = expandHome(g.Flake)
		if real, err := filepath.EvalSymlinks(g.Flake); err == nil {
			g.Flake = real
		}
		if g.Attr == "" {
			g.Attr = "hanare"
		}
		if g.CPUs == 0 {
			g.CPUs = 2
		}
		if g.Memory == "" {
			g.Memory = "4G"
		}
		if g.DiskSize == "" {
			g.DiskSize = "20G"
		}
		if g.VsockCID == 0 {
			g.VsockCID = 4242
		}
		if g.VsockCID < 3 {
			return nil, fmt.Errorf("%s: guests.%s.vsock_cid must be >= 3", configFile(), name)
		}
	}
	if f.Default != "" && f.Guests[f.Default] == nil {
		return nil, fmt.Errorf("%s: default guest %q is not defined", configFile(), f.Default)
	}
	return &f, nil
}

// names returns the guest names, sorted.
func (f *File) names() []string {
	var out []string
	for n := range f.Guests {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// pick selects a guest: name if given, else $HANARE_GUEST, else "default"
// from hanare.toml, else the only guest defined.
func (f *File) pick(name string) (*Guest, error) {
	if name == "" {
		name = os.Getenv("HANARE_GUEST")
	}
	if name == "" {
		name = f.Default
	}
	if name == "" && len(f.Guests) == 1 {
		name = f.names()[0]
	}
	if name == "" {
		return nil, fmt.Errorf("several guests defined (%s); choose one with -g NAME or set default in %s",
			strings.Join(f.names(), ", "), configFile())
	}
	g := f.Guests[name]
	if g == nil {
		return nil, fmt.Errorf("no guest %q in %s (defined: %s)", name, configFile(), strings.Join(f.names(), ", "))
	}
	return g, nil
}

// appendShare records a share for g in hanare.toml, keeping the file's comments.
func (g *Guest) appendShare(s Share) error {
	f, err := os.OpenFile(configFile(), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	block := fmt.Sprintf("\n[[guests.%s.share]]\npath = %q\n", g.Name, tildePath(s.Path))
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
// that could write these would escape to the host). Symlinked config (e.g.
// into a dotfiles checkout) is covered through its resolved location, and
// every guest's flake is protected from every guest.
func (g *Guest) sensitive() []string {
	h := home()
	out := []string{
		filepath.Join(h, ".ssh"),
		filepath.Join(h, ".gnupg"),
		filepath.Join(h, ".claude"),
		filepath.Join(h, ".config"),
		filepath.Join(h, ".local"),
	}
	for _, other := range g.file.Guests {
		out = append(out, other.Flake)
	}
	if real, err := filepath.EvalSymlinks(configFile()); err == nil {
		out = append(out, filepath.Dir(real))
	}
	return out
}

// resolveShare validates a share path and returns its real absolute path.
func (g *Guest) resolveShare(p string) (string, error) {
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
	for _, s := range g.sensitive() {
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
func (g *Guest) activeShares() []Share {
	var out []Share
	for _, s := range g.Shares {
		if p, err := g.resolveShare(s.Path); err == nil {
			out = append(out, Share{Path: p, ReadOnly: s.ReadOnly})
		}
	}
	return append(out, g.runtimeShares()...)
}

// guestDir returns the current directory, which must lie inside a share; the
// guest sees shares at the same path.
func (g *Guest) guestDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(wd); err == nil {
		wd = real
	}
	var paths []string
	for _, s := range g.activeShares() {
		if within(wd, s.Path) {
			return wd, nil
		}
		paths = append(paths, s.Path)
	}
	return "", fmt.Errorf("%s is not inside a directory shared with guest %q (%s); see \"hanare share add\"",
		wd, g.Name, strings.Join(paths, ", "))
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
