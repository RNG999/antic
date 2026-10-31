package refine_test

import (
	"net/netip"
	"slices"
	"testing"

	"pgregory.net/rapid"

	"github.com/RNG999/antic/internal/spec"
	"github.com/RNG999/antic/internal/tea/egress"
)

// Property-based checks of the egress rules beyond the fixed universe of
// egress_test.go and the free-Boolean abstraction of egress_smt_test.go: random
// hosts, random policies (any CIDR lists the config accepts) and random
// destinations, biased to the edges of every prefix involved.
//
// Differential: the rules decide as spec.Reach does.
// Metamorphic (on the implementation, so they hold even if spec and
// implementation shared a mistake): adding a deny entry never makes a
// destination reachable, adding an allow entry never makes one
// unreachable, default deny reaches no more than default allow, a learned
// address is reached at least as often as an unknown one, and the order
// of the entries does not matter.

func genV4(t *rapid.T, label string) netip.Addr {
	b := rapid.SliceOfN(rapid.Byte(), 4, 4).Draw(t, label)
	return netip.AddrFrom4([4]byte(b))
}

func genV6(t *rapid.T, label string) netip.Addr {
	b := rapid.SliceOfN(rapid.Byte(), 16, 16).Draw(t, label)
	return netip.AddrFrom16([16]byte(b))
}

func genPrefix(t *rapid.T, label string) netip.Prefix {
	if rapid.Bool().Draw(t, label+"6") {
		return netip.PrefixFrom(genV6(t, label), rapid.IntRange(16, 128).Draw(t, label+"bits")).Masked()
	}
	return netip.PrefixFrom(genV4(t, label), rapid.IntRange(8, 32).Draw(t, label+"bits")).Masked()
}

// edgeOf is an address at or next to an edge of p.
func edgeOf(t *rapid.T, p netip.Prefix, label string) netip.Addr {
	as := around(p)
	return as[rapid.IntRange(0, len(as)-1).Draw(t, label)]
}

type egressCase struct {
	specHost spec.HostNet
	host     egress.HostNet
	raw      spec.RawNetwork
	policy   spec.NetPolicy
	prefixes []netip.Prefix // every prefix the case involves, for biased destinations
}

func genHost(t *rapid.T) (spec.HostNet, egress.HostNet, []netip.Prefix) {
	var sh spec.HostNet
	var eh egress.HostNet
	var ps []netip.Prefix
	for i := range rapid.IntRange(1, 3).Draw(t, "ifaces") {
		var p netip.Prefix
		if rapid.Bool().Draw(t, "v6iface") {
			p = netip.PrefixFrom(genV6(t, "hostaddr"), 64)
		} else {
			p = netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 168, byte(i), rapid.ByteRange(1, 254).Draw(t, "host")}), 24)
		}
		sh.Addrs, eh.Addrs = append(sh.Addrs, p.Addr()), append(eh.Addrs, p.Addr())
		sh.OnLink, eh.OnLink = append(sh.OnLink, p.Masked()), append(eh.OnLink, p.Masked())
		ps = append(ps, p.Masked(), netip.PrefixFrom(p.Addr(), p.Addr().BitLen()))
	}
	return sh, eh, ps
}

// genPolicy draws a policy the config accepts (others are discarded).
func genPolicy(t *rapid.T) (spec.RawNetwork, spec.NetPolicy, []netip.Prefix) {
	raw := spec.RawNetwork{
		Mode:    rapid.SampledFrom([]string{"internet", "lan"}).Draw(t, "mode"),
		Default: rapid.SampledFrom([]string{"allow", "deny"}).Draw(t, "default"),
	}
	var ps []netip.Prefix
	for range rapid.IntRange(0, 3).Draw(t, "allows") {
		p := genPrefix(t, "allow")
		raw.Allow, ps = append(raw.Allow, p.String()), append(ps, p)
	}
	for range rapid.IntRange(0, 3).Draw(t, "denies") {
		p := genPrefix(t, "deny")
		raw.Deny, ps = append(raw.Deny, p.String()), append(ps, p)
	}
	cfg, err := spec.ParseConfig(spec.Env{}, spec.RawConfig{Network: raw})
	if err != nil {
		t.Skipf("the config rejects it: %v", err)
	}
	return raw, cfg.Res.Net, ps
}

func genCase(t *rapid.T) egressCase {
	sh, eh, hps := genHost(t)
	raw, p, pps := genPolicy(t)
	ps := slices.Concat(hps, pps, spec.Shape(p, sh).Never, spec.Shape(p, sh).Private)
	return egressCase{specHost: sh, host: eh, raw: raw, policy: p, prefixes: ps}
}

// genDst draws a destination: anywhere, or at an edge of a prefix of c,
// and possibly in its IPv4-mapped IPv6 form (which random IPv6 addresses
// practically never are).
func genDst(t *rapid.T, c egressCase) netip.Addr {
	var d netip.Addr
	switch rapid.IntRange(0, 2).Draw(t, "dstkind") {
	case 0:
		d = genV4(t, "dst4")
	case 1:
		d = genV6(t, "dst6")
	default:
		p := c.prefixes[rapid.IntRange(0, len(c.prefixes)-1).Draw(t, "dstprefix")]
		d = edgeOf(t, p, "dstedge")
	}
	if d.Is4() && rapid.Bool().Draw(t, "mapped") {
		d = netip.AddrFrom16(d.As16())
	}
	return d
}

func decide(c egressCase, raw spec.RawNetwork, d netip.Addr, learned bool) (bool, bool) {
	cfg, err := spec.ParseConfig(spec.Env{}, spec.RawConfig{Network: raw})
	if err != nil {
		return false, false
	}
	pol, _, _ := egressPolicy(cfg.Res.Net)
	return egress.Eval(egress.Rules(pol, c.host), d, learned), true
}

func TestEgressRulesRefineReachOnRandomInputs(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := genCase(t)
		d := genDst(t, c)
		learned := rapid.Bool().Draw(t, "learned")
		got, _ := decide(c, c.raw, d, learned)
		if want := spec.Reach(c.policy, c.specHost, d, learned); got != want {
			t.Fatalf("%+v on %+v: %v (learned %v): rules %v, spec %v", c.raw, c.host, d, learned, got, want)
		}
	})
}

func TestEgressRulesMetamorphicRelations(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := genCase(t)
		d := genDst(t, c)
		learned := rapid.Bool().Draw(t, "learned")
		base, _ := decide(c, c.raw, d, learned)

		extra := genPrefix(t, "extra")
		if rapid.Bool().Draw(t, "extraAtDst") {
			extra = netip.PrefixFrom(d, rapid.IntRange(d.BitLen()/2, d.BitLen()).Draw(t, "extraBits")).Masked()
		}
		denied := c.raw
		denied.Deny = append(slices.Clone(c.raw.Deny), extra.String())
		if got, ok := decide(c, denied, d, learned); ok && got && !base {
			t.Fatalf("adding deny %v made %v reachable under %+v", extra, d, c.raw)
		}
		allowed := c.raw
		allowed.Allow = append(slices.Clone(c.raw.Allow), extra.String())
		if got, ok := decide(c, allowed, d, learned); ok && !got && base {
			t.Fatalf("adding allow %v made %v unreachable under %+v", extra, d, c.raw)
		}

		closed, open := c.raw, c.raw
		closed.Default, open.Default = "deny", "allow"
		dc, ok1 := decide(c, closed, d, learned)
		do, ok2 := decide(c, open, d, learned)
		if ok1 && ok2 && dc && !do {
			t.Fatalf("%v is reachable with default deny but not with default allow under %+v", d, c.raw)
		}

		if !learned {
			if l, _ := decide(c, c.raw, d, true); base && !l {
				t.Fatalf("learning %v made it unreachable under %+v", d, c.raw)
			}
		}

		shuffled := c.raw
		shuffled.Allow = rapid.Permutation(slices.Clone(c.raw.Allow)).Draw(t, "allowOrder")
		shuffled.Deny = rapid.Permutation(slices.Clone(c.raw.Deny)).Draw(t, "denyOrder")
		if got, _ := decide(c, shuffled, d, learned); got != base {
			t.Fatalf("reordering the entries of %+v changed the decision for %v", c.raw, d)
		}
	})
}
