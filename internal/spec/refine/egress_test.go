package refine_test

import (
	"net/netip"
	"testing"

	"github.com/RNG999/antic/internal/domain"
	"github.com/RNG999/antic/internal/spec"
	"github.com/RNG999/antic/internal/tea/egress"
)

// subsets returns every subset of xs.
func subsets(xs []string) [][]string {
	out := [][]string{nil}
	for _, x := range xs {
		for _, s := range out {
			out = append(out, append(append([]string(nil), s...), x))
		}
	}
	return out
}

// egressPolicy is the abstraction the host implements: the IP-level policy
// internal/tea/egress enforces for a specification policy.
func egressPolicy(p spec.NetPolicy) (egress.Policy, []domain.DomainPattern, []domain.DomainPattern) {
	out := egress.Policy{Mode: egress.ModeInternet}
	if p.Mode == spec.NetLAN {
		out.Mode = egress.ModeLAN
	}
	if p.Default == spec.DefaultDeny {
		out.Default = egress.DefaultDeny
	}
	var allow, deny []domain.DomainPattern
	for _, e := range p.Allow.All() {
		if e.Kind == spec.EntryCIDR {
			out.AllowNets = append(out.AllowNets, e.Prefix)
		} else {
			allow = append(allow, e.Pattern)
		}
	}
	for _, e := range p.Deny.All() {
		if e.Kind == spec.EntryCIDR {
			out.DenyNets = append(out.DenyNets, e.Prefix)
		} else {
			deny = append(deny, e.Pattern)
		}
	}
	return out, allow, deny
}

// TestEgressRulesRefineReach is the L1b check: for every valid policy over
// a small universe, the ordered rules the host loads into nftables decide
// every representative destination as the specification's Reach does, and
// the DNS filter answers exactly the names Resolvable allows.
func TestEgressRulesRefineReach(t *testing.T) {
	addrs := []string{"192.168.1.10", "2404:db8:1::10"}
	onLink := []string{"192.168.1.0/24", "2404:db8:1::/64"}
	var specHost spec.HostNet
	var host egress.HostNet
	for _, a := range addrs {
		specHost.Addrs = append(specHost.Addrs, netip.MustParseAddr(a))
		host.Addrs = append(host.Addrs, netip.MustParseAddr(a))
	}
	for _, p := range onLink {
		specHost.OnLink = append(specHost.OnLink, netip.MustParsePrefix(p))
		host.OnLink = append(host.OnLink, netip.MustParsePrefix(p))
	}
	// One representative per class Reach distinguishes.
	var dsts []netip.Addr
	for _, s := range []string{
		"192.168.1.1", "192.168.1.10", "2404:db8:1::10", "2404:db8:1::1", // on-link, host itself
		"1.1.1.1", "1.1.1.2", "8.8.8.8", "2606:4700::1111", "::ffff:1.1.1.1", // public
		"10.0.0.1", "100.64.0.1", "172.16.0.1", "198.18.0.1", "fd00::1", "64:ff9b::1", // private
		"127.0.0.1", "::1", "169.254.169.254", "fe80::1", "224.0.0.1", "0.0.0.0", // never
	} {
		dsts = append(dsts, netip.MustParseAddr(s))
	}
	var names []domain.DomainName
	for _, s := range []string{"one.one.one.one", "evil.com", "api.evil.com", "example.org"} {
		n, _ := domain.ParseDomainName(s)
		names = append(names, n)
	}

	policies, decisions := 0, 0
	for _, mode := range []string{"internet", "lan"} {
		for _, def := range []string{"", "allow", "deny"} {
			for _, allow := range subsets([]string{"192.168.1.1/32", "1.1.1.1/32", "one.one.one.one"}) {
				for _, deny := range subsets([]string{"1.1.1.0/24", "192.168.1.0/24", "*.evil.com"}) {
					raw := spec.RawConfig{Network: spec.RawNetwork{Mode: mode, Default: def, Allow: allow, Deny: deny}}
					cfg, err := spec.ParseConfig(spec.Env{}, raw)
					if err != nil {
						continue // invalid under option B (e.g. allow without default)
					}
					policies++
					np := cfg.Res.Net
					pol, allowDomains, denyDomains := egressPolicy(np)
					rules := egress.Rules(pol, host)
					for _, d := range dsts {
						for _, learned := range []bool{false, true} {
							want := spec.Reach(np, specHost, d, learned)
							if got := egress.Eval(rules, d, learned); got != want {
								t.Errorf("%+v: %v (learned %v): rules say %v, spec says %v", raw.Network, d, learned, got, want)
							}
							decisions++
						}
					}
					dns := egress.Init(pol.Default, allowDomains, denyDomains)
					for _, n := range names {
						if got, want := dns.DomainAllowed(n), spec.Resolvable(np, n); got != want {
							t.Errorf("%+v: %s: DNS filter says %v, spec says %v", raw.Network, n.Get(), got, want)
						}
					}
				}
			}
		}
	}
	t.Logf("checked %d destination decisions over %d policies", decisions, policies)
	if policies < 50 {
		t.Fatalf("only %d valid policies; the universe is too small", policies)
	}
}
