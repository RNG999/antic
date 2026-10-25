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
	CPUs   int    `toml:"cpus" json:"cpus,omitempty"`
	Memory string `toml:"memory" json:"memory,omitempty"`
}

// NetworkConfig holds network settings in [network].
type NetworkConfig struct {
	Mode  string   `toml:"mode" json:"mode"`
	Allow []string `toml:"allow" json:"allow,omitempty"`
	Deny  []string `toml:"deny" json:"deny,omitempty"`
	Ports []Port   `toml:"ports" json:"ports,omitempty"`
}

// UnmarshalTOML supports both `network = "internet"` (string) and
// `[network]\nmode = "internet"` (table).
func (n *NetworkConfig) UnmarshalTOML(data any) error {
	switch v := data.(type) {
	case string:
		n.Mode = v
		return nil
	case map[string]any:
		for k := range v {
			if !slices.Contains([]string{"mode", "allow", "deny", "ports"}, k) {
				return fmt.Errorf("unknown key %q%s", k, suggest(toml.Key{k}))
			}
		}
		if m, ok := v["mode"].(string); ok {
			n.Mode = m
		}
		if a, ok := v["allow"].([]any); ok {
			for _, item := range a {
				if s, ok := item.(string); ok {
					n.Allow = append(n.Allow, s)
				}
			}
		}
		if d, ok := v["deny"].([]any); ok {
			for _, item := range d {
				if s, ok := item.(string); ok {
					n.Deny = append(n.Deny, s)
				}
			}
		}
		switch pts := v["ports"].(type) {
		case []any:
			for _, item := range pts {
				if m, ok := item.(map[string]any); ok {
					n.Ports = append(n.Ports, decodePortMap(m))
				}
			}
		case []map[string]any:
			for _, m := range pts {
				n.Ports = append(n.Ports, decodePortMap(m))
			}
		}
		return nil
	default:
		return fmt.Errorf("invalid network value: must be a mode string (e.g. \"internet\") or a [network] section")
	}
}

func decodePortMap(m map[string]any) Port {
	var p Port
	if h, ok := m["host"].(int64); ok {
		p.Host = int(h)
	} else if h, ok := m["host"].(int); ok {
		p.Host = h
	}
	if g, ok := m["guest"].(int64); ok {
		p.Guest = int(g)
	} else if g, ok := m["guest"].(int); ok {
		p.Guest = g
	} else {
		p.Guest = p.Host
	}
	if desc, ok := m["description"].(string); ok {
		p.Description = desc
	}
	return p
}

// Port is one host-to-guest TCP port mapping (127.0.0.1:Host -> Guest).
type Port struct {
	Host        int    `toml:"host" json:"host"`                                     // 1..65535
	Guest       int    `toml:"guest" json:"guest"`                                   // 1..65535 (defaults to Host if omitted)
	Description string `toml:"description,omitempty" json:"description,omitempty"` // optional explanation
}

// Share is a host directory visible in the guest at the same path (or a remote share).
type Share struct {
	Source      string `toml:"source" json:"source,omitempty"`
	Path        string `toml:"path" json:"path,omitempty"` // legacy alias or canonical local path
	Target      string `toml:"target" json:"target,omitempty"`
	Mode        string `toml:"mode" json:"mode,omitempty"` // "rw" | "ro"
	ReadOnly    bool   `toml:"readonly" json:"readonly,omitempty"`
	Credentials string `toml:"credentials" json:"credentials,omitempty"`
}

// UnmarshalTOML unmarshals a share entry, supporting both:
// - New format: `source = "~/Work"`, `mode = "rw"` (or `"ro"`)
// - Legacy format: `path = "~/Work"`, `readonly = true`
func (s *Share) UnmarshalTOML(data any) error {
	m, ok := data.(map[string]any)
	if !ok {
		return fmt.Errorf("invalid share entry: expected a table")
	}
	for k := range m {
		if !slices.Contains([]string{"source", "path", "target", "mode", "readonly", "credentials"}, k) {
			return fmt.Errorf("unknown key %q%s", k, suggest(toml.Key{k}))
		}
	}
	if src, ok := m["source"].(string); ok {
		s.Source = src
	}
	if p, ok := m["path"].(string); ok {
		s.Path = p
		if s.Source == "" {
			s.Source = p
		}
	}
	if tgt, ok := m["target"].(string); ok {
		s.Target = tgt
	}
	if md, ok := m["mode"].(string); ok {
		s.Mode = md
		switch s.Mode {
		case "ro":
			s.ReadOnly = true
		case "rw":
			s.ReadOnly = false
		default:
			return fmt.Errorf("invalid share mode %q (must be \"rw\" or \"ro\")", s.Mode)
		}
	} else if ro, ok := m["readonly"].(bool); ok {
		s.ReadOnly = ro
		if ro {
			s.Mode = "ro"
		} else {
			s.Mode = "rw"
		}
	} else {
		s.Mode = "rw"
		s.ReadOnly = false
	}
	if creds, ok := m["credentials"].(string); ok {
		s.Credentials = creds
	}
	return nil
}

// Guest is one ~/.config/antic/guests/<name>.toml file.
type Guest struct {
	Name string `toml:"-"`
	Path string `toml:"-"`

	Description string   `toml:"description"`
	Packages    []string `toml:"packages"`
	// Optional custom flake ("/path/to/flake" or "/path/to/flake#attr").
	// When empty, antic builds its built-in guest configuration with Packages.
	Flake string `toml:"flake"`

	Spec    SpecConfig        `toml:"spec"`
	CPUs    int               `toml:"cpus"`   // fallback if [spec] omitted
	Memory  string            `toml:"memory"` // fallback if [spec] omitted
	Network NetworkConfig     `toml:"network"`
	Shares  []Share           `toml:"shares"`
	Ports   []Port            `toml:"ports"`  // fallback if [[network.ports]] omitted
	Env     map[string]string `toml:"env"`

	FlakePath    string         `toml:"-"`
	FlakeAttr    string         `toml:"-"`
	MemoryBytes  int64          `toml:"-"`
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

var (
	nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	pkgRE  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+-]*$`)
)

// ValidName reports whether s can name a guest.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// ValidPackage reports whether s is a valid nixpkgs attribute name.
func ValidPackage(s string) bool { return pkgRE.MatchString(s) }

// Load reads and validates all *.toml guest files in dir. If dir does not
// exist yet, an empty Config is returned without error.
// Parse errors in individual guests are recorded in c.Errors rather than
// causing Load to fail, so valid guests remain usable.
func Load(dir, home string) (*Config, error) {
	c := &Config{
		Guests: map[string]*Guest{},
		Errors: map[string]error{},
		dir:    dir,
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".toml")
		path := filepath.Join(dir, e.Name())
		g, err := LoadGuest(path, name, home)
		if err != nil {
			c.Errors[name] = err
			continue
		}
		c.Guests[name] = g
	}
	return c, nil
}

// LoadGuest reads and validates a single guest's TOML file.
func LoadGuest(path, name, home string) (*Guest, error) {
	var g Guest
	md, err := toml.DecodeFile(path, &g)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
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

func (g *Guest) normalize(home string) []error {
	var errs []error
	if g.Flake != "" {
		rawPath, attr, hasAttr := strings.Cut(g.Flake, "#")
		if !hasAttr || attr == "" {
			attr = "antic"
		}
		var p string
		p, errs = absPath(rawPath, home, "flake", errs)
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
		if !pkgRE.MatchString(p) {
			errs = append(errs, fmt.Errorf("packages: invalid package attribute %q", p))
		}
	}
	if g.Spec.CPUs > 0 {
		g.CPUs = g.Spec.CPUs
	} else if g.CPUs > 0 {
		g.Spec.CPUs = g.CPUs
	}
	if g.Spec.Memory != "" {
		g.Memory = g.Spec.Memory
	} else if g.Memory != "" {
		g.Spec.Memory = g.Memory
	}
	if g.CPUs == 0 {
		g.CPUs = 2
		g.Spec.CPUs = 2
	}
	if g.CPUs < 1 || g.CPUs > 256 {
		errs = append(errs, errors.New("cpus must be between 1 and 256"))
	}
	if g.Memory == "" {
		g.Memory = "4G"
		g.Spec.Memory = "4G"
	}
	if n, err := ParseSize(g.Memory); err != nil {
		errs = append(errs, fmt.Errorf("memory: %w", err))
	} else if n < 256<<20 {
		errs = append(errs, errors.New("memory must be at least 256M"))
	} else {
		g.MemoryBytes = n
	}
	if g.Network.Mode == "" {
		g.Network.Mode = NetworkInternet
	}
	if !slices.Contains([]string{NetworkNone, NetworkInternet, NetworkLAN}, g.Network.Mode) {
		errs = append(errs, errors.New("network.mode must be \"none\", \"internet\" or \"lan\""))
	}
	if g.Network.Mode == NetworkNone {
		if len(g.Ports) > 0 {
			errs = append(errs, errors.New("ports cannot be used when network.mode is \"none\""))
		}
		if len(g.Network.Allow) > 0 {
			errs = append(errs, errors.New("network.allow cannot be used when network.mode is \"none\""))
		}
		if len(g.Network.Deny) > 0 {
			errs = append(errs, errors.New("network.deny cannot be used when network.mode is \"none\""))
		}
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
	if len(g.Network.Ports) > 0 {
		g.Ports = append(g.Ports, g.Network.Ports...)
	}
	for k, v := range g.Env {
		if k == "" || strings.Contains(k, "=") || strings.ContainsRune(k, 0) {
			errs = append(errs, fmt.Errorf("env: invalid variable name %q", k))
		}
		if strings.ContainsRune(v, 0) {
			errs = append(errs, fmt.Errorf("env: variable %q contains null byte", k))
		}
	}
	seenPorts := map[int]bool{}
	for i := range g.Ports {
		if g.Ports[i].Guest == 0 {
			g.Ports[i].Guest = g.Ports[i].Host
		}
		hp, gp := g.Ports[i].Host, g.Ports[i].Guest
		if hp < 1 || hp > 65535 || gp < 1 || gp > 65535 {
			errs = append(errs, fmt.Errorf("ports[%d]: port numbers must be between 1 and 65535", i))
			continue
		}
		if seenPorts[hp] {
			errs = append(errs, fmt.Errorf("ports[%d]: duplicate host port %d", i, hp))
			continue
		}
		seenPorts[hp] = true
	}
	for i := range g.Shares {
		sp := g.Shares[i].Path
		if sp == "" {
			sp = g.Shares[i].Source
		}
		if !strings.Contains(sp, "://") {
			var abs string
			abs, errs = absPath(sp, home, fmt.Sprintf("shares[%d].source", i), errs)
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
				abs, errs = absPath(g.Shares[i].Target, home, fmt.Sprintf("shares[%d].target", i), errs)
				g.Shares[i].Target = abs
			}
		}
		if g.Shares[i].Mode == "" {
			if g.Shares[i].ReadOnly {
				g.Shares[i].Mode = "ro"
			} else {
				g.Shares[i].Mode = "rw"
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

// ParsePort parses a CLI port mapping string: "PORT" or "HOST_PORT:GUEST_PORT".
func ParsePort(raw string) (Port, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Port{}, errors.New("empty port specification")
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
	return Port{Host: hp, Guest: gp}, nil
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
	"description", "packages", "flake",
	"spec", "cpus", "memory",
	"network", "mode", "allow", "deny",
	"shares", "source", "path", "target", "mode", "readonly", "credentials",
	"ports", "host", "guest", "env",
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
