// Package config loads and edits ~/.config/hanare/hanare.toml (schema
// version 1): the guests that can be started and the sharing policy.
// Loading is strict: unknown keys, bad sizes and invalid paths are errors.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Version is the schema version this hanare reads.
const Version = 1

// Config is hanare.toml.
type Config struct {
	Version      int               `toml:"version"`
	DefaultGuest string            `toml:"default_guest"`
	Policy       Policy            `toml:"policy"`
	Guests       map[string]*Guest `toml:"guests"`

	path string
}

// Policy limits what may be shared.
type Policy struct {
	// Runtime "share add" outside these directories asks for confirmation
	// on a terminal and is refused otherwise. Empty: no restriction.
	ShareRoots []string `toml:"share_roots"`
	// Paths that are never shared, in addition to the built-in list.
	Deny []string `toml:"deny"`
	// Names exported read-only inside shares, in addition to the built-in
	// list (proto.ProtectedNames).
	Protect []string `toml:"protect"`
}

// Network modes.
const (
	NetworkNone     = "none"     // no network device at all
	NetworkFiltered = "filtered" // internet only: no LAN, CGNAT, link-local or host addresses
	NetworkLAN      = "lan"      // internet and LAN (still no host loopback)
)

// Guest is one [guests.NAME] table.
type Guest struct {
	Name string `toml:"-"`

	Description string `toml:"description"`
	// Flake with nixosConfigurations.<Attr>: the guest's OS.
	Flake string `toml:"flake"`
	Attr  string `toml:"attr"`

	CPUs    int    `toml:"cpus"`
	Memory  string `toml:"memory"`
	Network string `toml:"network"`

	Commands Commands `toml:"commands"`
	Shares   []Share  `toml:"share"`

	MemoryBytes int64 `toml:"-"`
}

// Commands configures tools exposed on the host.
type Commands struct {
	// ~/.local/bin links that run these commands in this guest.
	Link []string `toml:"link"`
	// Commands that always run without network.
	Offline []string `toml:"offline"`
}

// Share is a host directory visible in the guest at the same path.
type Share struct {
	Path     string `toml:"path" json:"path"`
	ReadOnly bool   `toml:"read_only" json:"read_only,omitempty"`
}

var (
	nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	toolRE = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)
)

// ValidName reports whether s can name a guest.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Load reads and validates the config at path. home is the user's home
// directory, used to expand "~".
func Load(path, home string) (*Config, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.path = path
	var errs []error
	if und := md.Undecoded(); len(und) > 0 {
		for _, k := range und {
			errs = append(errs, fmt.Errorf("unknown key %q%s", k.String(), suggest(k)))
		}
	}
	if c.Version != Version {
		if c.Version == 0 {
			errs = append(errs, fmt.Errorf("missing \"version = %d\" (this looks like an older hanare.toml; see examples/hanare.toml)", Version))
		} else {
			errs = append(errs, fmt.Errorf("version %d is not supported (this hanare reads version %d)", c.Version, Version))
		}
	}
	errs = append(errs, c.normalize(home)...)
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s:\n  %w", path, joinIndented(errs))
	}
	return &c, nil
}

// Path is the file the config was loaded from.
func (c *Config) Path() string { return c.path }

// normalize fills in defaults and validates; it returns every problem.
func (c *Config) normalize(home string) []error {
	var errs []error
	if len(c.Guests) == 0 {
		errs = append(errs, errors.New("no guests defined (add a [guests.NAME] table)"))
	}
	if c.DefaultGuest != "" && c.Guests[c.DefaultGuest] == nil {
		errs = append(errs, fmt.Errorf("default_guest %q is not defined", c.DefaultGuest))
	}
	for i, p := range c.Policy.ShareRoots {
		c.Policy.ShareRoots[i], errs = absPath(p, home, "policy.share_roots", errs)
	}
	for i, p := range c.Policy.Deny {
		c.Policy.Deny[i], errs = absPath(p, home, "policy.deny", errs)
	}
	linked := map[string]string{}
	for _, name := range c.Names() {
		g := c.Guests[name]
		g.Name = name
		where := "guests." + name
		if !ValidName(name) {
			errs = append(errs, fmt.Errorf("%s: invalid guest name (a-z, 0-9 and -, up to 32 characters)", where))
		}
		if g.Flake == "" {
			errs = append(errs, fmt.Errorf("%s.flake is required", where))
		} else {
			g.Flake, errs = absPath(g.Flake, home, where+".flake", errs)
			// Nix refuses symlinked flake directories (e.g. into dotfiles).
			if real, err := filepath.EvalSymlinks(g.Flake); err == nil {
				g.Flake = real
			}
		}
		if g.Attr == "" {
			g.Attr = "hanare"
		}
		if g.CPUs == 0 {
			g.CPUs = 2
		}
		if g.CPUs < 1 || g.CPUs > 256 {
			errs = append(errs, fmt.Errorf("%s.cpus must be between 1 and 256", where))
		}
		if g.Memory == "" {
			g.Memory = "4G"
		}
		if n, err := ParseSize(g.Memory); err != nil {
			errs = append(errs, fmt.Errorf("%s.memory: %w", where, err))
		} else if n < 256<<20 {
			errs = append(errs, fmt.Errorf("%s.memory must be at least 256M", where))
		} else {
			g.MemoryBytes = n
		}
		if g.Network == "" {
			g.Network = NetworkFiltered
		}
		if !slices.Contains([]string{NetworkNone, NetworkFiltered, NetworkLAN}, g.Network) {
			errs = append(errs, fmt.Errorf("%s.network must be \"none\", \"filtered\" or \"lan\"", where))
		}
		for _, t := range append(slices.Clone(g.Commands.Link), g.Commands.Offline...) {
			if !toolRE.MatchString(t) || t == "hanare" {
				errs = append(errs, fmt.Errorf("%s.commands: invalid command name %q", where, t))
			}
		}
		for _, t := range g.Commands.Link {
			if other, ok := linked[t]; ok {
				errs = append(errs, fmt.Errorf("%s.commands.link: %q is already linked to guest %s", where, t, other))
			}
			linked[t] = name
		}
		for i := range g.Shares {
			g.Shares[i].Path, errs = absPath(g.Shares[i].Path, home, fmt.Sprintf("%s.share[%d].path", where, i), errs)
		}
		for i, a := range g.Shares {
			for _, b := range g.Shares[i+1:] {
				if within(a.Path, b.Path) || within(b.Path, a.Path) {
					errs = append(errs, fmt.Errorf("%s: shares %s and %s overlap", where, a.Path, b.Path))
				}
			}
		}
	}
	return errs
}

// absPath expands "~" and requires an absolute path without control
// characters.
func absPath(p, home, where string, errs []error) (string, []error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	if !filepath.IsAbs(p) {
		return p, append(errs, fmt.Errorf("%s: %q must be absolute or start with ~/", where, p))
	}
	if strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return p, append(errs, fmt.Errorf("%s: %q contains control characters", where, p))
	}
	return filepath.Clean(p), errs
}

func within(path, dir string) bool { return path == dir || strings.HasPrefix(path, dir+"/") }

// Names returns the guest names, sorted.
func (c *Config) Names() []string {
	names := make([]string, 0, len(c.Guests))
	for n := range c.Guests {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Pick selects a guest: name if given, else $HANARE_GUEST, else
// default_guest, else the only guest defined.
func (c *Config) Pick(name string) (*Guest, error) {
	if name == "" {
		name = os.Getenv("HANARE_GUEST")
	}
	if name == "" {
		name = c.DefaultGuest
	}
	if name == "" && len(c.Guests) == 1 {
		name = c.Names()[0]
	}
	if name == "" {
		return nil, fmt.Errorf("several guests are defined (%s); choose one with -g NAME or set default_guest",
			strings.Join(c.Names(), ", "))
	}
	g := c.Guests[name]
	if g == nil {
		return nil, fmt.Errorf("no guest %q in %s (defined: %s)", name, c.path, strings.Join(c.Names(), ", "))
	}
	return g, nil
}

// ParseSize parses sizes like "4G", "512M" or "2048K" (base 1024; a bare
// number is bytes).
func ParseSize(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("empty size")
	}
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
	num := s
	if mult > 1 {
		num = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid size %q (use e.g. 4G or 512M)", s)
	}
	return n * mult, nil
}

var knownKeys = []string{
	"version", "default_guest", "policy", "share_roots", "deny", "protect", "guests",
	"description", "flake", "attr", "cpus", "memory", "network", "commands", "link", "offline",
	"share", "path", "read_only",
}

// suggest proposes the closest known key for a misspelled one.
func suggest(k toml.Key) string {
	last := k[len(k)-1]
	best, bestD := "", 4
	for _, cand := range knownKeys {
		if d := distance(last, cand); d < bestD {
			best, bestD = cand, d
		}
	}
	if best == "" || best == last {
		return ""
	}
	return fmt.Sprintf(" (did you mean %q?)", best)
}

// distance is the Levenshtein distance.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

func joinIndented(errs []error) error {
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.Error()
	}
	return errors.New(strings.Join(msgs, "\n  "))
}
