// Package spec is antic's top-level behavioral specification: a pure state
// machine over the user-visible World of guests, written independently of
// the implementation (internal/tea, internal/host) so that it can serve as the
// test oracle of the E2E suite and of refinement checks. See
// docs/design/spec.md.
//
// Under the desired semantics (atomic commands that take effect immediately),
// a created guest's installed and booted systems, resources, and port forwards
// always equal its Config, so the World stores them once, in Config. The only
// runtime state that can diverge from Config is the set of active mounts.
package spec

import (
	"net/netip"
	"slices"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/types"
)

// ShareMode is the access mode of a shared directory.
type ShareMode uint8

const (
	// ModeRW shares a directory read-write.
	ModeRW ShareMode = iota
	// ModeRO shares a directory read-only.
	ModeRO
)

// MountOrigin records how an active mount came to exist.
type MountOrigin uint8

const (
	// OriginBoot means the share was mounted when the guest booted.
	OriginBoot MountOrigin = iota
	// OriginRuntime means the share was hot-plugged into the running guest.
	OriginRuntime
)

// Phase is a guest's lifecycle phase as seen by the user.
type Phase uint8

const (
	// PhaseUncreated means only the config exists ("-" in "guest ls").
	PhaseUncreated Phase = iota
	// PhaseOff means the guest is built but not running.
	PhaseOff
	// PhaseRunning means the guest is running.
	PhaseRunning
)

// NetMode is the base reachable set of a guest's network.
type NetMode uint8

const (
	// NetInternet reaches public addresses only.
	NetInternet NetMode = iota
	// NetLAN reaches public and private (LAN) addresses.
	NetLAN
	// NetNone attaches no network device.
	NetNone
)

// NetDefault decides what happens to destinations no allow entry names.
type NetDefault uint8

const (
	// DefaultAllow reaches the base set of the mode (default = "allow").
	DefaultAllow NetDefault = iota
	// DefaultDeny reaches only allow entries (default = "deny").
	DefaultDeny
)

// Proto is the transport protocol of a port forward.
type Proto uint8

const (
	// ProtoTCP forwards TCP.
	ProtoTCP Proto = iota
	// ProtoUDP forwards UDP.
	ProtoUDP
	// ProtoBoth forwards TCP and UDP.
	ProtoBoth
)

func (p Proto) tcp() bool { return p != ProtoUDP }
func (p Proto) udp() bool { return p != ProtoTCP }

// overlaps reports whether p and q forward a common protocol.
func (p Proto) overlaps(q Proto) bool { return (p.tcp() && q.tcp()) || (p.udp() && q.udp()) }

// EntryKind distinguishes CIDR and domain entries of allow and deny lists.
type EntryKind uint8

const (
	// EntryCIDR is an IP prefix entry such as "10.0.0.0/8".
	EntryCIDR EntryKind = iota
	// EntryDomain is a domain entry such as "example.com" or "*.example.com".
	EntryDomain
)

// PackageSet is a canonical set of nixpkgs attributes.
type PackageSet = types.SortedSet[domain.PackageName, domain.PackageName, domain.PackageNameOrder]

// Share is a configured shared directory.
type Share struct {
	Path domain.AbsPath
	Mode ShareMode
}

// ShareByPath orders shares by path.
type ShareByPath struct{}

// Key returns the share's path.
func (ShareByPath) Key(s Share) domain.AbsPath { return s.Path }

// Less orders paths lexicographically.
func (ShareByPath) Less(a, b domain.AbsPath) bool { return a.Get() < b.Get() }

// ShareSet is a canonical set of shares keyed by path.
type ShareSet = types.SortedSet[Share, domain.AbsPath, ShareByPath]

// Mount is a share that is currently mounted in a running guest.
type Mount struct {
	Path   domain.AbsPath
	Mode   ShareMode
	Origin MountOrigin
}

// MountByPath orders mounts by path.
type MountByPath struct{}

// Key returns the mount's path.
func (MountByPath) Key(m Mount) domain.AbsPath { return m.Path }

// Less orders paths lexicographically.
func (MountByPath) Less(a, b domain.AbsPath) bool { return a.Get() < b.Get() }

// MountSet is a canonical set of mounts keyed by path.
type MountSet = types.SortedSet[Mount, domain.AbsPath, MountByPath]

// Session is a command running in a guest ("antic run" that has not
// returned), as "guest ps" lists it, identified by its command line.
type Session struct {
	Command string
}

// SessionByCommand orders sessions by command line.
type SessionByCommand struct{}

// Key returns the session's command line.
func (SessionByCommand) Key(s Session) string { return s.Command }

// Less orders command lines lexicographically.
func (SessionByCommand) Less(a, b string) bool { return a < b }

// SessionSet is a canonical set of sessions keyed by command line.
type SessionSet = types.SortedSet[Session, string, SessionByCommand]

// Port forwards 127.0.0.1:Host on the host to Guest in the guest.
type Port struct {
	Host  domain.PortNumber
	Guest domain.PortNumber
	Proto Proto
}

// PortKey identifies a port forward within a guest.
type PortKey struct {
	Host  domain.PortNumber
	Proto Proto
}

// PortByHost orders port forwards by host port, then protocol.
type PortByHost struct{}

// Key returns the forward's host port and protocol.
func (PortByHost) Key(p Port) PortKey { return PortKey{Host: p.Host, Proto: p.Proto} }

// Less orders by host port, then protocol.
func (PortByHost) Less(a, b PortKey) bool {
	if a.Host.Get() != b.Host.Get() {
		return a.Host.Get() < b.Host.Get()
	}
	return a.Proto < b.Proto
}

// PortSet is a canonical set of port forwards.
type PortSet = types.SortedSet[Port, PortKey, PortByHost]

// Entry is one allow or deny entry.
type Entry struct {
	Kind    EntryKind
	Prefix  netip.Prefix
	Pattern domain.DomainPattern
}

// EntryOrder orders entries: CIDRs before domains, then by text.
type EntryOrder struct{}

// Key returns the entry itself.
func (EntryOrder) Key(e Entry) Entry { return e }

// Less orders entries by kind, then textual form.
func (EntryOrder) Less(a, b Entry) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.Kind == EntryCIDR {
		return a.Prefix.String() < b.Prefix.String()
	}
	return a.Pattern.Get() < b.Pattern.Get()
}

// EntrySet is a canonical set of allow or deny entries.
type EntrySet = types.SortedSet[Entry, Entry, EntryOrder]

// NetPolicy is a guest's network policy (design option B: mode, default,
// and allow/deny exceptions are orthogonal).
type NetPolicy struct {
	Mode    NetMode
	Default NetDefault
	Allow   EntrySet
	Deny    EntrySet
}

// Resources are a guest's VM resources and network policy.
type Resources struct {
	CPUs   domain.CPUCount
	Memory domain.MemorySize
	Net    NetPolicy
}

// Config is a guest's persisted, desired state (its TOML file).
type Config struct {
	Packages PackageSet
	Shares   ShareSet
	Res      Resources
	Ports    PortSet
}

// Guest is one guest managed by the test.
type Guest struct {
	Name   domain.GuestName
	Config Config
	Phase  Phase
	// Mounts are the active mounts; empty unless Phase is PhaseRunning.
	Mounts MountSet
	// Sessions are the running commands; empty unless Phase is
	// PhaseRunning (stopping or restarting the guest ends them).
	Sessions SessionSet
}

// GuestByName orders guests by name.
type GuestByName struct{}

// Key returns the guest's name.
func (GuestByName) Key(g Guest) domain.GuestName { return g.Name }

// Less orders names lexicographically.
func (GuestByName) Less(a, b domain.GuestName) bool { return a.Get() < b.Get() }

// GuestSet is a canonical set of guests keyed by name.
type GuestSet = types.SortedSet[Guest, domain.GuestName, GuestByName]

// External is a guest that existed before the test started. The test never
// targets it, and no command may change it (frame condition P5).
type External struct {
	Name  domain.GuestName
	Phase Phase
	Ports PortSet
}

// ExternalByName orders external guests by name.
type ExternalByName struct{}

// Key returns the external guest's name.
func (ExternalByName) Key(e External) domain.GuestName { return e.Name }

// Less orders names lexicographically.
func (ExternalByName) Less(a, b domain.GuestName) bool { return a.Get() < b.Get() }

// ExternalSet is a canonical set of external guests keyed by name.
type ExternalSet = types.SortedSet[External, domain.GuestName, ExternalByName]

// World is the complete user-visible state.
type World struct {
	Guests   GuestSet
	External ExternalSet
}

// Equal reports whether two worlds are identical.
func (w World) Equal(o World) bool {
	return slices.EqualFunc(w.Guests.All(), o.Guests.All(), Guest.Equal) &&
		slices.EqualFunc(w.External.All(), o.External.All(), External.Equal)
}

// Equal reports whether two guests are identical.
func (g Guest) Equal(o Guest) bool {
	return g.Name == o.Name && g.Phase == o.Phase &&
		g.Config.Equal(o.Config) &&
		slices.Equal(g.Mounts.All(), o.Mounts.All()) &&
		slices.Equal(g.Sessions.All(), o.Sessions.All())
}

// Equal reports whether two external guests are identical.
func (e External) Equal(o External) bool {
	return e.Name == o.Name && e.Phase == o.Phase && slices.Equal(e.Ports.All(), o.Ports.All())
}

// Equal reports whether two configs are identical.
func (c Config) Equal(o Config) bool {
	return slices.Equal(c.Packages.All(), o.Packages.All()) &&
		slices.Equal(c.Shares.All(), o.Shares.All()) &&
		c.Res.Equal(o.Res) &&
		slices.Equal(c.Ports.All(), o.Ports.All())
}

// Equal reports whether two resource sets are identical.
func (r Resources) Equal(o Resources) bool {
	return r.CPUs == o.CPUs && r.Memory == o.Memory && r.Net.Equal(o.Net)
}

// Equal reports whether two network policies are identical.
func (p NetPolicy) Equal(o NetPolicy) bool {
	return p.Mode == o.Mode && p.Default == o.Default &&
		slices.Equal(p.Allow.All(), o.Allow.All()) &&
		slices.Equal(p.Deny.All(), o.Deny.All())
}

// bootMounts returns the mounts of a guest freshly booted with cfg.
func bootMounts(cfg Config) MountSet {
	var ms []Mount
	for _, s := range cfg.Shares.All() {
		ms = append(ms, Mount{Path: s.Path, Mode: s.Mode, Origin: OriginBoot})
	}
	return types.BuildSet[Mount, domain.AbsPath, MountByPath](ms)
}

// names returns every defined guest name (managed and external), sorted.
func (w World) names() []domain.GuestName {
	var out []domain.GuestName
	for _, g := range w.Guests.All() {
		out = append(out, g.Name)
	}
	for _, e := range w.External.All() {
		out = append(out, e.Name)
	}
	slices.SortFunc(out, func(a, b domain.GuestName) int {
		switch {
		case a.Get() < b.Get():
			return -1
		case a.Get() > b.Get():
			return 1
		}
		return 0
	})
	return out
}

// otherPorts returns the port forwards of every guest except name.
func (w World) otherPorts(name domain.GuestName) []Port {
	var out []Port
	for _, g := range w.Guests.All() {
		if g.Name != name {
			out = append(out, g.Config.Ports.All()...)
		}
	}
	for _, e := range w.External.All() {
		if e.Name != name {
			out = append(out, e.Ports.All()...)
		}
	}
	return out
}
