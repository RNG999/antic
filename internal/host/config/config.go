// Package config loads and edits guest configuration files in
// ~/.config/antic/guests/<name>.toml. Loading is strict: unknown keys,
// bad sizes and invalid paths are errors.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/RNG999/antic/internal/domain"
)

// Config holds all loaded guest configurations from ~/.config/antic/guests.
type Config struct {
	Guests map[string]*Guest
	Errors map[string]error // guests that failed to load (name -> parse error)

	dir string
}

// Network modes.
const (
	NetworkNone     = "none"     // no network device at all
	NetworkInternet = "internet" // internet only: no LAN, CGNAT, link-local or host addresses
	NetworkLAN      = "lan"      // internet and LAN (still no host loopback)
)

// SpecConfig holds hardware specifications in [spec].
type SpecConfig struct {
	CPUs   int    `toml:"cpus" json:"cpus"`
	Memory string `toml:"memory" json:"memory"`
}

// NetworkConfig holds network settings in [network].
type NetworkConfig struct {
	Mode string `toml:"mode" json:"mode"`
	// Default is "allow" (the mode's base set, plus allow exceptions) or
	// "deny" (only allow entries and what allowed domains resolve to).
	Default string   `toml:"default" json:"default,omitempty"`
	Allow   []string `toml:"allow" json:"allow,omitempty"`
	Deny    []string `toml:"deny" json:"deny,omitempty"`
	Ports   []Port   `toml:"ports" json:"ports,omitempty"`
}

// Port is one host-to-guest port mapping (127.0.0.1:Host -> Guest).
type Port struct {
	Host        int    `toml:"host" json:"host"`                                   // 1..65535
	Guest       int    `toml:"guest" json:"guest"`                                 // 1..65535 (defaults to Host if omitted)
	Proto       string `toml:"proto,omitempty" json:"proto,omitempty"`             // "tcp", "udp", or "tcp,udp" (defaults to "tcp")
	Description string `toml:"description,omitempty" json:"description,omitempty"` // optional explanation
}

func (p Port) TCP() bool {
	switch strings.ToLower(p.Proto) {
	case "", "tcp", "tcp,udp", "udp,tcp":
		return true
	default:
		return false
	}
}

func (p Port) UDP() bool {
	switch strings.ToLower(p.Proto) {
	case "udp", "tcp,udp", "udp,tcp":
		return true
	default:
		return false
	}
}

func (p Port) ProtoStr() string {
	if p.TCP() && p.UDP() {
		return "tcp,udp"
	}
	if p.UDP() {
		return "udp"
	}
	return "tcp"
}

// Share is a host directory visible in the guest at the same path (or a remote share).
type Share struct {
	Source      string `toml:"source" json:"source"`
	Target      string `toml:"target,omitempty" json:"target,omitempty"`
	Mode        string `toml:"mode" json:"mode"` // "rw" | "ro"
	Credentials string `toml:"credentials,omitempty" json:"credentials,omitempty"`

	Path     string `toml:"-" json:"path"` // normalized local absolute path
	ReadOnly bool   `toml:"-" json:"readonly,omitempty"`
}

// Guest is one ~/.config/antic/guests/<name>.toml or antic.toml file.
type Guest struct {
	Name string `toml:"name"`
	Path string `toml:"-"`

	Description string            `toml:"description"`
	Packages    []string          `toml:"packages"`
	Flake       string            `toml:"flake"`
	Spec        SpecConfig        `toml:"spec"`
	Network     NetworkConfig     `toml:"network"`
	Shares      []Share           `toml:"shares"`
	Env         map[string]string `toml:"env"`

	CPUs         int            `toml:"-"`
	Memory       string         `toml:"-"`
	Ports        []Port         `toml:"-"`
	MemoryBytes  int64          `toml:"-"`
	FlakePath    string         `toml:"-"`
	FlakeAttr    string         `toml:"-"`
	AllowDomains []string       `toml:"-"` // lowercase, no trailing dot
	AllowNets    []netip.Prefix `toml:"-"` // masked
	DenyDomains  []string       `toml:"-"` // lowercase, no trailing dot
	DenyNets     []netip.Prefix `toml:"-"` // masked
}

// neverAllowed are destinations an allow entry may not overlap: loopback,
// unspecified, link-local (including cloud metadata), multicast, reserved
// and IPv4-mapped IPv6. The egress filter always blocks them.
var neverAllowed = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("::ffff:0:0/96"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

var domainLabelRE = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?$`)

// parseEntry classifies one allow/deny entry as a prefix or a domain.
// Bare IP addresses without prefix lengths (e.g. "1.1.1.1") are rejected.
func parseEntry(raw string) (netip.Prefix, string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return netip.Prefix{}, "", errors.New("empty entry")
	}
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return p, "", fmt.Errorf("%q: invalid CIDR", raw)
		}
		if p.Addr().Zone() != "" {
			return netip.Prefix{}, "", fmt.Errorf("%q: zones are not allowed", raw)
		}
		p = p.Masked()
		for _, n := range neverAllowed {
			if p.Overlaps(n) {
				return netip.Prefix{}, "", fmt.Errorf("%q overlaps %s, which guests can never reach", raw, n)
			}
		}
		return p, "", nil
	}
	if a, err := netip.ParseAddr(s); err == nil {
		bitLen := 32
		if a.Is6() {
			bitLen = 128
		}
		return netip.Prefix{}, "", fmt.Errorf("%q: IP addresses must use CIDR notation (e.g. %s/%d)", raw, s, bitLen)
	}
	wildcard := false
	if strings.HasPrefix(s, "*.") {
		wildcard = true
		s = strings.TrimPrefix(s, "*.")
	}
	d := strings.TrimSuffix(s, ".")
	labels := strings.Split(d, ".")
	if len(d) > 253 || len(labels) < 2 {
		return netip.Prefix{}, "", fmt.Errorf("%q: not a CIDR or domain (a domain needs at least two labels)", raw)
	}
	for _, l := range labels {
		if !domainLabelRE.MatchString(l) {
			return netip.Prefix{}, "", fmt.Errorf("%q: invalid domain", raw)
		}
	}
	if wildcard {
		return netip.Prefix{}, "*." + d, nil
	}
	return netip.Prefix{}, d, nil
}

// ValidName reports whether s can name a guest (delegates to SSOT domain.ParseGuestName).
func ValidName(s string) bool {
	_, err := domain.ParseGuestName(s)
	return err == nil
}

// ValidPackage reports whether s is a valid nixpkgs attribute name (delegates to SSOT domain.ParsePackageName).
func ValidPackage(s string) bool {
	_, err := domain.ParsePackageName(s)
	return err == nil
}

// Load reads and validates all *.toml guest files in dir. If dir does not
// exist yet, an empty Config is returned without error.
// Parse errors in individual guests are recorded in c.Errors rather than
// causing Load to fail, so valid guests remain usable.
func Load(dir, home string) (*Config, error) {
	return LoadWithProject(dir, home, "")
}

// LoadWithProject reads guests from dir, and additionally discovers a project
// guest config (e.g. antic.toml) from startDir (walking up parent directories).
func LoadWithProject(dir, home, startDir string) (*Config, error) {
	c := &Config{
		Guests: map[string]*Guest{},
		Errors: map[string]error{},
		dir:    dir,
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".toml")
		path := filepath.Join(dir, e.Name())
		g, err := loadGuest(path, name, home)
		if err != nil {
			c.Errors[name] = err
			continue
		}
		c.Guests[name] = g
	}

	// Discover project antic.toml if startDir is provided
	if startDir != "" {
		if ppath := findProjectConfig(startDir); ppath != "" {
			g, err := loadProjectGuest(ppath, home)
			if err != nil {
				c.Errors[filepath.Base(ppath)] = err
			} else {
				if existing, ok := c.Guests[g.Name]; ok && existing.Path != g.Path {
					fmt.Fprintf(os.Stderr, "antic: warning: guest %q in %s shadows %s\n", g.Name, g.Path, existing.Path)
				}
				// Project guest takes precedence or adds to guests
				c.Guests[g.Name] = g
			}
		}
	}

	return c, nil
}

// loadProjectGuest reads and validates a project-level antic.toml file.
// In project-level configs, "name" is strictly required, and relative paths
// (e.g. for flake or shares) are resolved relative to the directory containing
// antic.toml.
func loadProjectGuest(path, home string) (*Guest, error) {
	var g Guest
	md, err := toml.DecodeFile(path, &g)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if g.Name == "" {
		return nil, fmt.Errorf("%s: \"name\" is required", path)
	}
	g.Path = path
	var errs []error
	if !ValidName(g.Name) {
		errs = append(errs, fmt.Errorf("invalid guest name %q (a-z, 0-9 and -, up to 32 characters)", g.Name))
	}
	if und := md.Undecoded(); len(und) > 0 {
		for _, k := range und {
			errs = append(errs, fmt.Errorf("unknown key %q%s", k.String(), suggest(k)))
		}
	}
	baseDir := filepath.Dir(path)
	errs = append(errs, g.normalize(home, baseDir)...)
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s:\n  %w", path, joinIndented(errs))
	}
	return &g, nil
}

func findProjectConfig(start string) string {
	curr, err := filepath.Abs(start)
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(curr, "antic.toml")
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return candidate
		}
		parent := filepath.Dir(curr)
		if parent == curr {
			break
		}
		curr = parent
	}
	return ""
}

// loadGuest reads and validates a single global guest TOML file (~/.config/antic/guests/<name>.toml).
// In global guest files, paths must be absolute or start with ~/.
func loadGuest(path, name, home string) (*Guest, error) {
	var g Guest
	md, err := toml.DecodeFile(path, &g)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if name == "" {
		name = g.Name
	}
	if name == "" {
		return nil, fmt.Errorf("%s: \"name\" is required", path)
	}
	g.Name = name
	g.Path = path
	var errs []error
	if !ValidName(name) {
		errs = append(errs, fmt.Errorf("invalid guest name %q (a-z, 0-9 and -, up to 32 characters)", name))
	}
	if und := md.Undecoded(); len(und) > 0 {
		for _, k := range und {
			errs = append(errs, fmt.Errorf("unknown key %q%s", k.String(), suggest(k)))
		}
	}
	errs = append(errs, g.normalize(home)...)
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s:\n  %w", path, joinIndented(errs))
	}
	return &g, nil
}

// Dir is the directory guest TOML files live in.
func (c *Config) Dir() string { return c.dir }

// GuestPath returns the path to guest name's TOML file.
func (c *Config) GuestPath(name string) string {
	return filepath.Join(c.dir, name+".toml")
}

func (g *Guest) normalize(home string, baseDir ...string) []error {
	var dir string
	if len(baseDir) > 0 {
		dir = baseDir[0]
	}
	var errs []error
	if g.Flake != "" {
		rawPath, attr, hasAttr := strings.Cut(g.Flake, "#")
		if !hasAttr || attr == "" {
			attr = "antic"
		}
		var p string
		p, errs = resolvePath(rawPath, home, dir, "flake", errs)
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		g.FlakePath = p
		g.FlakeAttr = attr
		if len(g.Packages) > 0 {
			errs = append(errs, errors.New("cannot set both packages and flake"))
		}
	}
	for _, p := range g.Packages {
		if !ValidPackage(p) {
			errs = append(errs, fmt.Errorf("packages: invalid package attribute %q", p))
		}
	}
	g.CPUs = g.Spec.CPUs
	if g.CPUs == 0 {
		g.CPUs = 2
		g.Spec.CPUs = 2
	}
	if g.CPUs < 1 || g.CPUs > 256 {
		errs = append(errs, errors.New("spec.cpus must be between 1 and 256"))
	}
	g.Memory = g.Spec.Memory
	if g.Memory == "" {
		g.Memory = "4G"
		g.Spec.Memory = "4G"
	}
	if n, err := parseSize(g.Memory); err != nil {
		errs = append(errs, fmt.Errorf("spec.memory: %w", err))
	} else if n < 256<<20 {
		errs = append(errs, errors.New("spec.memory must be at least 256M"))
	} else {
		g.MemoryBytes = n
	}
	if g.Network.Mode == "" {
		g.Network.Mode = NetworkInternet
	}
	if !slices.Contains([]string{NetworkNone, NetworkInternet, NetworkLAN}, g.Network.Mode) {
		errs = append(errs, errors.New("network.mode must be \"none\", \"internet\" or \"lan\""))
	}
	g.Ports = g.Network.Ports
	if g.Network.Mode == NetworkNone {
		if len(g.Ports) > 0 {
			errs = append(errs, errors.New("network.ports cannot be used when network.mode is \"none\""))
		}
		if len(g.Network.Allow) > 0 {
			errs = append(errs, errors.New("network.allow cannot be used when network.mode is \"none\""))
		}
		if len(g.Network.Deny) > 0 {
			errs = append(errs, errors.New("network.deny cannot be used when network.mode is \"none\""))
		}
		if g.Network.Default != "" {
			errs = append(errs, errors.New("network.default cannot be used when network.mode is \"none\""))
		}
	}
	switch g.Network.Default {
	case "", "allow", "deny":
	default:
		errs = append(errs, fmt.Errorf("network.default must be \"allow\" or \"deny\", not %q", g.Network.Default))
	}
	if g.Network.Default == "" && len(g.Network.Allow) > 0 {
		// Before network.default existed, any allow entry meant "deny
		// everything else"; reading such a file as "allow" would silently
		// widen what the guest can reach.
		errs = append(errs, errors.New("network.allow requires network.default: set it to \"deny\" to keep allowing only the allow list, or \"allow\" to add exceptions to the mode"))
	}
	g.AllowDomains, g.AllowNets = nil, nil
	for i, raw := range g.Network.Allow {
		p, d, err := parseEntry(raw)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("network.allow[%d]: %w", i, err))
		case d != "":
			if !slices.Contains(g.AllowDomains, d) {
				g.AllowDomains = append(g.AllowDomains, d)
			}
		default:
			if !slices.Contains(g.AllowNets, p) {
				g.AllowNets = append(g.AllowNets, p)
			}
		}
	}
	if !g.Network.DefaultDeny() && len(g.AllowDomains) > 0 {
		// Public names are reachable anyway, and DNS answers never open the LAN.
		errs = append(errs, fmt.Errorf("network.allow: domain %q has no effect unless network.default is \"deny\"", g.AllowDomains[0]))
	}
	g.DenyDomains, g.DenyNets = nil, nil
	for i, raw := range g.Network.Deny {
		p, d, err := parseEntry(raw)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("network.deny[%d]: %w", i, err))
		case d != "":
			if !slices.Contains(g.DenyDomains, d) {
				g.DenyDomains = append(g.DenyDomains, d)
			}
		default:
			if !slices.Contains(g.DenyNets, p) {
				g.DenyNets = append(g.DenyNets, p)
			}
		}
	}
	for k, v := range g.Env {
		if k == "" || strings.Contains(k, "=") || strings.ContainsRune(k, 0) {
			errs = append(errs, fmt.Errorf("env: invalid variable name %q", k))
		}
		if strings.ContainsRune(v, 0) {
			errs = append(errs, fmt.Errorf("env: variable %q contains null byte", k))
		}
	}
	seenTCP := map[int]bool{}
	seenUDP := map[int]bool{}
	for i := range g.Ports {
		if g.Ports[i].Guest == 0 {
			g.Ports[i].Guest = g.Ports[i].Host
		}
		proto := strings.ToLower(g.Ports[i].Proto)
		if proto == "" {
			proto = "tcp"
		}
		switch proto {
		case "tcp", "udp", "tcp,udp", "udp,tcp":
			if proto == "udp,tcp" {
				proto = "tcp,udp"
			}
			g.Ports[i].Proto = proto
		default:
			errs = append(errs, fmt.Errorf("network.ports[%d]: invalid proto %q (use \"tcp\", \"udp\", or \"tcp,udp\")", i, g.Ports[i].Proto))
			continue
		}
		hp, gp := g.Ports[i].Host, g.Ports[i].Guest
		if hp < 1 || hp > 65535 || gp < 1 || gp > 65535 {
			errs = append(errs, fmt.Errorf("network.ports[%d]: port numbers must be between 1 and 65535", i))
			continue
		}
		if g.Ports[i].TCP() {
			if seenTCP[hp] {
				errs = append(errs, fmt.Errorf("network.ports[%d]: duplicate host port %d (tcp)", i, hp))
				continue
			}
			seenTCP[hp] = true
		}
		if g.Ports[i].UDP() {
			if seenUDP[hp] {
				errs = append(errs, fmt.Errorf("network.ports[%d]: duplicate host port %d (udp)", i, hp))
				continue
			}
			seenUDP[hp] = true
		}
	}
	for i := range g.Shares {
		if g.Shares[i].Source == "" {
			errs = append(errs, fmt.Errorf("shares[%d].source is required", i))
			continue
		}
		if g.Shares[i].Mode == "" {
			g.Shares[i].Mode = "rw"
		}
		switch g.Shares[i].Mode {
		case "ro":
			g.Shares[i].ReadOnly = true
		case "rw":
			g.Shares[i].ReadOnly = false
		default:
			errs = append(errs, fmt.Errorf("shares[%d].mode must be \"rw\" or \"ro\"", i))
		}
		src := g.Shares[i].Source
		if !strings.Contains(src, "://") {
			var abs string
			abs, errs = resolvePath(src, home, dir, fmt.Sprintf("shares[%d].source", i), errs)
			g.Shares[i].Path = abs
			g.Shares[i].Source = tildePath(abs, home)
			if g.Shares[i].Target == "" {
				g.Shares[i].Target = abs
			}
		} else {
			if g.Shares[i].Target == "" {
				errs = append(errs, fmt.Errorf("shares[%d]: remote share requires target mount path", i))
			} else {
				var abs string
				abs, errs = resolvePath(g.Shares[i].Target, home, dir, fmt.Sprintf("shares[%d].target", i), errs)
				g.Shares[i].Target = abs
			}
		}
	}
	for i, a := range g.Shares {
		if a.Path == "" {
			continue
		}
		for _, b := range g.Shares[i+1:] {
			if b.Path == "" {
				continue
			}
			if within(a.Path, b.Path) || within(b.Path, a.Path) {
				errs = append(errs, fmt.Errorf("shares %s and %s overlap", a.Path, b.Path))
			}
		}
	}
	return errs
}

// ParsePort parses a CLI port mapping string: "PORT", "PORT/proto", "HOST_PORT:GUEST_PORT", or "HOST:GUEST/proto".
func ParsePort(raw string) (Port, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Port{}, errors.New("empty port specification")
	}
	var proto string
	if before, after, ok := strings.Cut(s, "/"); ok {
		s = before
		proto = strings.ToLower(after)
		switch proto {
		case "tcp", "udp", "tcp,udp", "udp,tcp":
			if proto == "udp,tcp" {
				proto = "tcp,udp"
			}
		default:
			return Port{}, fmt.Errorf("invalid proto %q in %q (use tcp, udp, or tcp,udp)", proto, raw)
		}
	}
	hostStr, guestStr, hasColon := strings.Cut(s, ":")
	if !hasColon {
		guestStr = hostStr
	}
	hp, err1 := strconv.Atoi(hostStr)
	gp, err2 := strconv.Atoi(guestStr)
	if err1 != nil || err2 != nil || hp < 1 || hp > 65535 || gp < 1 || gp > 65535 {
		return Port{}, fmt.Errorf("invalid port %q (use PORT or HOST_PORT:GUEST_PORT, 1..65535)", raw)
	}
	return Port{Host: hp, Guest: gp, Proto: proto}, nil
}

// resolvePath resolves a path relative to baseDir (if relative) or home (if starts with ~).
func resolvePath(p, home, baseDir, where string, errs []error) (string, []error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	} else if !filepath.IsAbs(p) {
		if baseDir != "" {
			p = filepath.Join(baseDir, p)
		} else {
			return p, append(errs, fmt.Errorf("%s: %q must be absolute or start with ~/", where, p))
		}
	}
	if strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return p, append(errs, fmt.Errorf("%s: %q contains control characters", where, p))
	}
	return filepath.Clean(p), errs
}

// absPath expands "~" and requires an absolute path without control
// characters.
func absPath(p, home, where string, errs []error) (string, []error) {
	return resolvePath(p, home, "", where, errs)
}

func within(path, dir string) bool { return path == dir || strings.HasPrefix(path, dir+"/") }

// Names returns the guest names (both valid and erroneous), sorted.
func (c *Config) Names() []string {
	seen := map[string]bool{}
	for n := range c.Guests {
		seen[n] = true
	}
	for n := range c.Errors {
		seen[n] = true
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Get returns the named guest configuration. An empty name is an error.
func (c *Config) Get(name string) (*Guest, error) {
	if name == "" {
		return nil, errors.New("syntax error: guest name is required")
	}
	if err := c.Errors[name]; err != nil {
		return nil, err
	}
	if len(c.Guests) == 0 && len(c.Errors) == 0 {
		return nil, fmt.Errorf("no guests defined in %s (run \"antic guest create %s\")", c.dir, name)
	}
	g := c.Guests[name]
	if g == nil {
		return nil, fmt.Errorf("no guest %q in %s (defined: %s)", name, c.dir, strings.Join(c.Names(), ", "))
	}
	return g, nil
}

// parseSize parses sizes like "4G", "512M" or "2048K" (base 1024; a bare
// number is bytes).
func parseSize(s string) (int64, error) {
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
	"description", "packages", "flake",
	"spec", "cpus", "memory",
	"network", "mode", "default", "allow", "deny", "ports",
	"shares", "source", "target", "mode", "credentials",
	"host", "guest", "env",
}

// DefaultDeny reports whether network.default is "deny".
func (n NetworkConfig) DefaultDeny() bool { return n.Default == "deny" }

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
