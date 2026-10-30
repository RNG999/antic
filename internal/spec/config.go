package spec

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/types"
)

// Env holds the host facts commands depend on but never change.
type Env struct {
	// Home is the user's home directory; only directories below it can be shared.
	Home domain.AbsPath
	// Dirs are the existing directories (symlinks already resolved).
	Dirs []domain.AbsPath
	// Links are symlinks to directories.
	Links []Link
	// Denied are paths that may never be shared, nor any directory containing them.
	Denied []domain.AbsPath
	// Busy are host ports held by processes other than antic.
	Busy []PortKey
}

// Link is a symlink From -> To.
type Link struct {
	From domain.AbsPath
	To   domain.AbsPath
}

// RawConfig is the unvalidated content of a guest's TOML file, as written by
// "guest edit". Zero values mean "not set" and take the defaults.
type RawConfig struct {
	Packages []string
	CPUs     int
	Memory   string
	Network  RawNetwork
	Ports    []RawPort
	Shares   []RawShare
}

// RawNetwork is the unvalidated [network] table.
type RawNetwork struct {
	Mode    string
	Default string
	Allow   []string
	Deny    []string
}

// RawPort is one unvalidated [[network.ports]] entry.
type RawPort struct {
	Host  int
	Guest int
	Proto string
}

// RawShare is one unvalidated [[shares]] entry.
type RawShare struct {
	Path string
	Mode string
}

// defaults returns the CLI defaults for a new guest.
func defaults() RawConfig {
	return RawConfig{Packages: []string{"git"}, CPUs: 2, Memory: "4G"}
}

// parseConfig validates raw as the config of guest name in w.
func parseConfig(w World, env Env, name domain.GuestName, raw RawConfig) (Config, error) {
	var cfg Config
	pkgs, err := parsePackages(raw.Packages)
	if err != nil {
		return cfg, err
	}
	res, err := parseResources(raw)
	if err != nil {
		return cfg, err
	}
	var ports []Port
	for _, rp := range raw.Ports {
		p, err := rawPort(rp)
		if err != nil {
			return cfg, err
		}
		ports = append(ports, p)
	}
	portSet, err := addPorts(types.EmptySet[Port, PortKey, PortByHost](), ports, w.otherPorts(name))
	if err != nil {
		return cfg, err
	}
	if res.Net.Mode == NetNone && portSet.Len() > 0 {
		return cfg, errors.New("port forwards cannot be used when network.mode is \"none\"")
	}
	var shares []Share
	for _, rs := range raw.Shares {
		p, err := sharePath(env, rs.Path)
		if err != nil {
			return cfg, err
		}
		m, err := parseShareMode(rs.Mode)
		if err != nil {
			return cfg, err
		}
		for _, s := range shares {
			if domain.PathsOverlap(s.Path, p) {
				return cfg, fmt.Errorf("shares %s and %s overlap", s.Path.Get(), p.Get())
			}
		}
		shares = append(shares, Share{Path: p, Mode: m})
	}
	return Config{
		Packages: pkgs,
		Shares:   types.BuildSet[Share, domain.AbsPath, ShareByPath](shares),
		Res:      res,
		Ports:    portSet,
	}, nil
}

func parsePackages(raw []string) (PackageSet, error) {
	var out []domain.PackageName
	for _, s := range raw {
		p, err := domain.ParsePackageName(s)
		if err != nil {
			return PackageSet{}, err
		}
		out = append(out, p)
	}
	return types.BuildSet[domain.PackageName, domain.PackageName, domain.PackageNameOrder](out), nil
}

func parseResources(raw RawConfig) (Resources, error) {
	var res Resources
	cpus := raw.CPUs
	if cpus == 0 {
		cpus = 2
	}
	c, err := domain.ParseCPUCount(cpus)
	if err != nil {
		return res, err
	}
	memory := raw.Memory
	if memory == "" {
		memory = "4G"
	}
	n, err := parseSize(memory)
	if err != nil {
		return res, err
	}
	m, err := domain.ParseMemorySize(n)
	if err != nil {
		return res, err
	}
	net, err := parseNetwork(raw.Network)
	if err != nil {
		return res, err
	}
	return Resources{CPUs: c, Memory: m, Net: net}, nil
}

// parseSize parses N, NK, NM, NG or NT (case-insensitive) into bytes.
func parseSize(s string) (int64, error) {
	mult := int64(1)
	num := s
	if s != "" {
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
			num = s[:len(s)-1]
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n <= 0 || n > (1<<62)/mult {
		return 0, fmt.Errorf("invalid size %q (use e.g. 4G or 512M)", s)
	}
	return n * mult, nil
}

func parseNetMode(s string) (NetMode, error) {
	switch s {
	case "", "internet":
		return NetInternet, nil
	case "lan":
		return NetLAN, nil
	case "none":
		return NetNone, nil
	}
	return NetInternet, fmt.Errorf("network.mode must be \"none\", \"internet\" or \"lan\", not %q", s)
}

// parseNetwork validates a [network] table under design option B.
func parseNetwork(raw RawNetwork) (NetPolicy, error) {
	var p NetPolicy
	mode, err := parseNetMode(raw.Mode)
	if err != nil {
		return p, err
	}
	var def NetDefault
	switch raw.Default {
	case "":
		// Migration rule: before option B, a non-empty allow list implied
		// "deny everything else". Reading such a file as "allow" would
		// silently widen reachability, so the default must be explicit.
		if len(raw.Allow) > 0 {
			return p, errors.New("network.allow requires an explicit network.default (\"deny\" keeps the previous allow-list behavior)")
		}
		def = DefaultAllow
	case "allow":
		def = DefaultAllow
	case "deny":
		def = DefaultDeny
	default:
		return p, fmt.Errorf("network.default must be \"allow\" or \"deny\", not %q", raw.Default)
	}
	if mode == NetNone && (len(raw.Allow) > 0 || len(raw.Deny) > 0 || raw.Default != "") {
		return p, errors.New("network.default, allow and deny cannot be used when network.mode is \"none\"")
	}
	allow, err := parseEntries(raw.Allow)
	if err != nil {
		return p, fmt.Errorf("network.allow: %w", err)
	}
	deny, err := parseEntries(raw.Deny)
	if err != nil {
		return p, fmt.Errorf("network.deny: %w", err)
	}
	if def == DefaultAllow {
		for _, e := range allow.All() {
			if e.Kind == EntryDomain {
				// With default = "allow", public names are reachable anyway and
				// private answers never open the LAN, so the entry would be a no-op.
				return p, fmt.Errorf("network.allow: domain %q has no effect with network.default = \"allow\"", e.Pattern.Get())
			}
		}
	}
	return NetPolicy{Mode: mode, Default: def, Allow: allow, Deny: deny}, nil
}

func parseEntries(raw []string) (EntrySet, error) {
	var out []Entry
	for _, s := range raw {
		e, err := parseEntry(s)
		if err != nil {
			return EntrySet{}, err
		}
		out = append(out, e)
	}
	return types.BuildSet[Entry, Entry, EntryOrder](out), nil
}

// parseEntry classifies one allow/deny entry as a CIDR or a domain pattern.
func parseEntry(raw string) (Entry, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil || p.Addr().Zone() != "" {
			return Entry{}, fmt.Errorf("%q: invalid CIDR", raw)
		}
		p = p.Masked()
		for _, n := range neverPrefixes() {
			if p.Overlaps(n) {
				return Entry{}, fmt.Errorf("%q overlaps %s, which guests can never reach", raw, n)
			}
		}
		return Entry{Kind: EntryCIDR, Prefix: p}, nil
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return Entry{}, fmt.Errorf("%q: IP addresses must use CIDR notation", raw)
	}
	base := strings.TrimSuffix(strings.TrimPrefix(s, "*."), ".")
	if !strings.Contains(base, ".") {
		return Entry{}, fmt.Errorf("%q: not a CIDR or domain (a domain needs at least two labels)", raw)
	}
	pat := base
	if strings.HasPrefix(s, "*.") {
		pat = "*." + base
	}
	d, err := domain.ParseDomainPattern(pat)
	if err != nil {
		return Entry{}, fmt.Errorf("%q: invalid domain", raw)
	}
	return Entry{Kind: EntryDomain, Pattern: d}, nil
}

func parseProto(s string) (Proto, error) {
	switch strings.ToLower(s) {
	case "", "tcp":
		return ProtoTCP, nil
	case "udp":
		return ProtoUDP, nil
	case "tcp,udp", "udp,tcp":
		return ProtoBoth, nil
	}
	return ProtoTCP, fmt.Errorf("invalid proto %q (use tcp, udp, or tcp,udp)", s)
}

func rawPort(rp RawPort) (Port, error) {
	h, err := domain.ParsePortNumber(rp.Host)
	if err != nil {
		return Port{}, err
	}
	g, err := domain.ParsePortNumber(rp.Guest)
	if err != nil {
		return Port{}, err
	}
	pr, err := parseProto(rp.Proto)
	if err != nil {
		return Port{}, err
	}
	return Port{Host: h, Guest: g, Proto: pr}, nil
}

// parsePortSpec parses a CLI port spec: PORT, HOST:GUEST, with an optional
// "/proto" suffix that overrides proto.
func parsePortSpec(spec, proto string) (Port, error) {
	s := strings.TrimSpace(spec)
	if before, after, ok := strings.Cut(s, "/"); ok {
		s, proto = before, after
	}
	hostStr, guestStr, ok := strings.Cut(s, ":")
	if !ok {
		guestStr = hostStr
	}
	h, err1 := strconv.Atoi(hostStr)
	g, err2 := strconv.Atoi(guestStr)
	if err1 != nil || err2 != nil {
		return Port{}, fmt.Errorf("invalid port %q (use PORT or HOST_PORT:GUEST_PORT, 1..65535)", spec)
	}
	return rawPort(RawPort{Host: h, Guest: g, Proto: proto})
}

// addPorts adds ports to set, rejecting conflicts with set, with each other
// and with other guests' forwards. An identical forward is a no-op (P4).
func addPorts(set PortSet, ports []Port, others []Port) (PortSet, error) {
	out := set.Clone()
	for _, p := range ports {
		if have, ok := out.Lookup(PortByHost{}.Key(p)); ok && have == p {
			continue
		}
		for _, q := range append(out.All(), others...) {
			if q.Host == p.Host && q.Proto.overlaps(p.Proto) {
				return set, fmt.Errorf("host port %d is already forwarded", p.Host.Get())
			}
		}
		out = out.Upsert(p)
	}
	return out, nil
}

// busy reports whether a process other than antic holds one of ports.
func busy(env Env, ports PortSet) bool {
	for _, p := range ports.All() {
		for _, b := range env.Busy {
			if b.Host == p.Host && b.Proto.overlaps(p.Proto) {
				return true
			}
		}
	}
	return false
}

func parseShareMode(s string) (ShareMode, error) {
	switch s {
	case "", "rw":
		return ModeRW, nil
	case "ro":
		return ModeRO, nil
	}
	return ModeRW, fmt.Errorf("share mode must be \"rw\" or \"ro\", not %q", s)
}

// resolve follows a symlink to a directory, if raw names one.
func resolve(env Env, raw string) (domain.AbsPath, error) {
	p, err := domain.ParseAbsPath(raw)
	if err != nil {
		return p, err
	}
	for _, l := range env.Links {
		if l.From == p {
			return l.To, nil
		}
	}
	return p, nil
}

// sharePath resolves raw and checks that it may be shared.
func sharePath(env Env, raw string) (domain.AbsPath, error) {
	p, err := resolve(env, raw)
	if err != nil {
		return p, err
	}
	isDir := false
	for _, d := range env.Dirs {
		if d == p {
			isDir = true
		}
	}
	if !isDir {
		return p, fmt.Errorf("%s is not a directory", p.Get())
	}
	if !domain.WithinSubtree(env.Home, p) {
		return p, fmt.Errorf("%s: only directories below %s can be shared", p.Get(), env.Home.Get())
	}
	for _, d := range env.Denied {
		if domain.PathsOverlap(d, p) {
			return p, fmt.Errorf("%s: refusing to share (overlaps %s)", p.Get(), d.Get())
		}
	}
	return p, nil
}
