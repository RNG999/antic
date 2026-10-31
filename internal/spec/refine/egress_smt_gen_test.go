package refine_test

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/RNG999/antic/internal/spec"
	"github.com/RNG999/antic/internal/tea/egress"
)

// The differential check of the SMT generator of egress_smt_test.go: the Z3 proof
// is only as good as the translation of Go data into SMT-LIB, so on
// concrete inputs the generated formulas must agree with the Go code they
// stand for: smtIn with netip.Prefix.Contains (at and around every prefix
// boundary), rulesFormula with egress.Eval and reachFormula with spec.Reach
// (with the set memberships computed by Go).

// bitvec renders address d as an SMT-LIB bit-vector literal.
func bitvec(d netip.Addr) string {
	var b strings.Builder
	b.WriteString("#b")
	for _, x := range d.AsSlice() {
		fmt.Fprintf(&b, "%08b", x)
	}
	return b.String()
}

// around returns the first and last addresses of p and their neighbours.
func around(p netip.Prefix) []netip.Addr {
	first := p.Masked().Addr()
	b := first.AsSlice()
	for i := p.Bits(); i < first.BitLen(); i++ {
		b[i/8] |= 0x80 >> (i % 8)
	}
	last, _ := netip.AddrFromSlice(b)
	out := []netip.Addr{first, last}
	if prev := first.Prev(); prev.IsValid() {
		out = append(out, prev)
	}
	if next := last.Next(); next.IsValid() {
		out = append(out, next)
	}
	return out
}

func inAnyPrefix(d netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(d) {
			return true
		}
	}
	return false
}

func TestSMTGeneratorAgreesWithGo(t *testing.T) {
	sh, eh := testHost()
	shape := spec.Shape(specPolicy(t, "internet", "", false, false), sh)
	sets := map[string][]netip.Prefix{
		"never": shape.Never, "private": shape.Private,
		"allow": {allowMarker}, "deny": {denyMarker},
	}
	// (i) membership: every set, every address around every prefix in it.
	var script strings.Builder
	type check struct {
		what string
		want bool
	}
	var checks []check
	var addrs []netip.Addr
	for _, ps := range sets {
		for _, p := range ps {
			addrs = append(addrs, around(p)...)
		}
	}
	for name, ps := range sets {
		for _, d := range addrs {
			bits := d.BitLen()
			fmt.Fprintf(&script, "(push)(declare-const d (_ BitVec %d))(assert (= d %s))(assert %s)(check-sat)(pop)\n",
				bits, bitvec(d), smtIn(ps, bits))
			checks = append(checks, check{fmt.Sprintf("%v in %s", d, name), inAnyPrefix(d, ps)})
		}
	}
	// (ii) structure: concrete destinations of every class, for every policy
	// shape of the proof.
	var dsts []netip.Addr
	for _, s := range []string{
		"192.168.1.1", "192.168.1.10", "2404:db8:1::10", "1.1.1.1", "2606:4700::1111",
		"10.0.0.1", "fd00::1", "127.0.0.1", "169.254.169.254", "fe80::1",
		"203.0.113.5", "198.51.100.5", "203.0.114.1",
	} {
		dsts = append(dsts, netip.MustParseAddr(s))
	}
	for _, mode := range []string{"internet", "lan"} {
		for _, def := range []string{"allow", "deny"} {
			for _, allow := range []bool{false, true} {
				for _, deny := range []bool{false, true} {
					sp := specPolicy(t, mode, def, allow, deny)
					pol, _, _ := egressPolicy(sp)
					rules := egress.Rules(pol, eh)
					rf, sf := rulesFormula(rules), reachFormula(spec.Shape(sp, sh))
					for _, d := range dsts {
						for _, learned := range []bool{false, true} {
							env := fmt.Sprintf("(declare-const N Bool)(declare-const D Bool)(declare-const A Bool)(declare-const P Bool)(declare-const learned Bool)"+
								"(assert (= N %v))(assert (= D %v))(assert (= A %v))(assert (= P %v))(assert (= learned %v))",
								inAnyPrefix(d, shape.Never), deny && denyMarker.Contains(d), allow && allowMarker.Contains(d),
								inAnyPrefix(d, shape.Private), learned)
							what := fmt.Sprintf("mode=%s default=%s allow=%v deny=%v %v learned=%v", mode, def, allow, deny, d, learned)
							fmt.Fprintf(&script, "(push)%s(assert %s)(check-sat)(pop)\n", env, rf)
							checks = append(checks, check{"rules " + what, egress.Eval(rules, d, learned)})
							fmt.Fprintf(&script, "(push)%s(assert %s)(check-sat)(pop)\n", env, sf)
							checks = append(checks, check{"reach " + what, spec.Reach(sp, sh, d, learned)})
						}
					}
				}
			}
		}
	}
	got := z3(t, script.String())
	if len(got) != len(checks) {
		t.Fatalf("z3 answered %d of %d queries", len(got), len(checks))
	}
	for i, c := range checks {
		if (got[i] == "sat") != c.want {
			t.Errorf("%s: the formula says %s, Go says %v", c.what, got[i], c.want)
		}
	}
	t.Logf("checked %d formula evaluations against Go", len(checks))
}
