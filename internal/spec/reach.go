package spec

import (
	"net/netip"

	"github.com/RNG999/antic/internal/domain"
)

// HostNet is the host's network as seen when a guest boots: its own unicast
// addresses and its on-link subnets.
type HostNet struct {
	Addrs  []netip.Addr
	OnLink []netip.Prefix
}

// neverPrefixes are destinations no guest may ever reach: unspecified,
// loopback, link-local (including cloud metadata), multicast, reserved, and
// IPv4-mapped IPv6.
func neverPrefixes() []netip.Prefix {
	return []netip.Prefix{
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
}

// privatePrefixes are LAN-like destinations: RFC 1918, CGNAT (Tailscale),
// IETF protocol assignments, benchmarking, ULA, site-local, and NAT64.
func privatePrefixes() []netip.Prefix {
	return []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("fc00::/7"),
		netip.MustParsePrefix("fec0::/10"),
		netip.MustParsePrefix("64:ff9b::/96"),
		netip.MustParsePrefix("64:ff9b:1::/48"),
	}
}

func inAny(d netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(d) {
			return true
		}
	}
	return false
}

func cidrs(es EntrySet) []netip.Prefix {
	var out []netip.Prefix
	for _, e := range es.All() {
		if e.Kind == EntryCIDR {
			out = append(out, e.Prefix)
		}
	}
	return out
}

// never reports whether d is unreachable whatever the policy.
func never(h HostNet, d netip.Addr) bool {
	if inAny(d, neverPrefixes()) {
		return true
	}
	for _, a := range h.Addrs {
		if a == d {
			return true
		}
	}
	return false
}

// private reports whether d is a LAN-like destination.
func private(h HostNet, d netip.Addr) bool {
	return inAny(d, privatePrefixes()) || inAny(d, h.OnLink)
}

// Reach reports whether a guest running with policy p on host network h can
// connect to d. learned reports whether d was obtained, within its TTL, by
// resolving an allowed domain through the guest's DNS filter.
//
//	Reach = Mode ≠ None ∧ ¬Never(d) ∧ d ∉ Deny.CIDRs
//	      ∧ ( d ∈ Allow.CIDRs                                  -- static exceptions override the mode
//	        ∨ ( Base(Mode, d) ∧ (Default = allow ∨ learned) ) ) -- DNS answers never exceed the mode
func Reach(p NetPolicy, h HostNet, d netip.Addr, learned bool) bool {
	d = d.Unmap()
	if p.Mode == NetNone || never(h, d) || inAny(d, cidrs(p.Deny)) {
		return false
	}
	if inAny(d, cidrs(p.Allow)) {
		return true
	}
	base := p.Mode == NetLAN || !private(h, d)
	return base && (p.Default == DefaultAllow || learned)
}

// Resolvable reports whether the guest's DNS filter answers queries for name.
func Resolvable(p NetPolicy, name domain.DomainName) bool {
	if p.Mode == NetNone {
		return false
	}
	matches := func(es EntrySet) bool {
		for _, e := range es.All() {
			if e.Kind == EntryDomain && domain.DomainMatches(name, e.Pattern) {
				return true
			}
		}
		return false
	}
	if matches(p.Deny) {
		return false
	}
	return p.Default == DefaultAllow || matches(p.Allow)
}
