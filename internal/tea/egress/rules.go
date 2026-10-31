package egress

import "net/netip"

// NetMode is the base reachable set of a guest's network (a guest with mode
// "none" has no network device, so no rules).
type NetMode uint8

const (
	// ModeInternet reaches public destinations only.
	ModeInternet NetMode = iota
	// ModeLAN reaches public and private (LAN) destinations.
	ModeLAN
)

// Policy is the IP-level part of a guest's network policy.
type Policy struct {
	Mode      NetMode
	Default   Default
	AllowNets []netip.Prefix
	DenyNets  []netip.Prefix
}

// HostNet is the host's network when the guest starts: its own unicast
// addresses and its on-link subnets.
type HostNet struct {
	Addrs  []netip.Addr
	OnLink []netip.Prefix
}

// Verdict is what a rule does with a matching connection.
type Verdict uint8

const (
	// VerdictAccept lets the connection through.
	VerdictAccept Verdict = iota
	// VerdictReject refuses it.
	VerdictReject
)

// MatchKind is what a rule matches.
type MatchKind uint8

const (
	// MatchPrefixes matches destinations in Rule.Prefixes.
	MatchPrefixes MatchKind = iota
	// MatchLearned matches addresses the DNS filter learned (the dyn sets).
	MatchLearned
	// MatchAll matches every destination.
	MatchAll
)

// Rule is one egress rule; the first matching rule decides, and a
// connection no rule matches is accepted.
type Rule struct {
	Match    MatchKind
	Prefixes []netip.Prefix
	Verdict  Verdict
}

// NeverPrefixes are destinations no guest may ever reach: unspecified,
// loopback, link-local (including cloud metadata), multicast, reserved,
// and IPv4-mapped IPv6.
func NeverPrefixes() []netip.Prefix {
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

// PrivatePrefixes are the LAN-like ranges "internet" mode blocks: private
// networks, CGNAT (Tailscale), IETF protocol assignments, benchmarking,
// ULA, site-local and NAT64.
func PrivatePrefixes() []netip.Prefix {
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

// Rules returns the ordered egress rules for p on host network h:
//
//  1. reject destinations no guest may reach, and the host's own addresses
//  2. reject deny CIDRs
//  3. accept allow CIDRs (static exceptions override the mode)
//  4. "internet": reject LAN ranges and on-link subnets (so DNS answers
//     pointing into the LAN never open it)
//  5. default "deny": accept learned addresses, reject everything else
func Rules(p Policy, h HostNet) []Rule {
	never := NeverPrefixes()
	for _, a := range h.Addrs {
		never = append(never, netip.PrefixFrom(a, a.BitLen()))
	}
	rules := []Rule{{Match: MatchPrefixes, Prefixes: never, Verdict: VerdictReject}}
	if len(p.DenyNets) > 0 {
		rules = append(rules, Rule{Match: MatchPrefixes, Prefixes: append([]netip.Prefix(nil), p.DenyNets...), Verdict: VerdictReject})
	}
	if len(p.AllowNets) > 0 {
		rules = append(rules, Rule{Match: MatchPrefixes, Prefixes: append([]netip.Prefix(nil), p.AllowNets...), Verdict: VerdictAccept})
	}
	if p.Mode == ModeInternet {
		rules = append(rules, Rule{Match: MatchPrefixes, Prefixes: append(PrivatePrefixes(), h.OnLink...), Verdict: VerdictReject})
	}
	if p.Default == DefaultDeny {
		rules = append(rules,
			Rule{Match: MatchLearned, Verdict: VerdictAccept},
			Rule{Match: MatchAll, Verdict: VerdictReject},
		)
	}
	return rules
}

// Eval reports whether rules let a guest connect to d. learned reports
// whether d is in the DNS filter's learned set. An IPv4-mapped IPv6
// destination is an IPv4 connection on the wire.
func Eval(rules []Rule, d netip.Addr, learned bool) bool {
	d = d.Unmap()
	for _, r := range rules {
		match := false
		switch r.Match {
		case MatchPrefixes:
			for _, p := range r.Prefixes {
				match = match || p.Contains(d)
			}
		case MatchLearned:
			match = learned
		case MatchAll:
			match = true
		}
		if match {
			return r.Verdict == VerdictAccept
		}
	}
	return true
}
