package refine_test

import (
	"fmt"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/RNG999/antic/internal/spec"
	"github.com/RNG999/antic/internal/tea/egress"
)

// The Z3 proof that the egress rules equal the specification's Reach for
// every destination and every policy, in two parts:
//
//	(i)  the built-in address sets agree: the rules' "never" and "LAN" sets
//	     contain exactly the addresses of the specification's Never and
//	     Private sets, for all 2^32 IPv4 and 2^128 IPv6 addresses;
//	(ii) the rules' first-match structure equals Reach's formula when the
//	     membership of the destination in each set (never N, deny D, allow
//	     A, private P) and learned L are free Booleans. Free Booleans
//	     over-approximate every allow and deny list, so this covers every
//	     policy, for each mode, default, and empty or non-empty list.
//
// Both sides are generated from the data internal/tea/egress.Rules and
// spec.Shape return; only Reach's three-line evaluation of a ReachShape is
// transcribed (reachFormula).

// smtPrefix is the SMT-LIB membership test of bit-vector d in p.
func smtPrefix(p netip.Prefix) string {
	bits := p.Addr().BitLen()
	n := p.Bits()
	if n == 0 {
		return "true"
	}
	var b strings.Builder
	for _, x := range p.Addr().AsSlice() {
		fmt.Fprintf(&b, "%08b", x)
	}
	return fmt.Sprintf("(= ((_ extract %d %d) d) #b%s)", bits-1, bits-n, b.String()[:n])
}

// smtIn is the membership of d in the prefixes of one address family.
func smtIn(ps []netip.Prefix, bits int) string {
	var terms []string
	for _, p := range ps {
		if p.Addr().BitLen() == bits {
			terms = append(terms, smtPrefix(p))
		}
	}
	switch len(terms) {
	case 0:
		return "false"
	case 1:
		return terms[0]
	}
	return "(or " + strings.Join(terms, " ") + ")"
}

// z3 runs script and returns its check-sat answers.
func z3(t *testing.T, script string) []string {
	t.Helper()
	path, err := exec.LookPath("z3")
	if err != nil {
		t.Skip("z3 is not installed (it is in the dev shell: nix develop)")
	}
	cmd := exec.Command(path, "-in")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("z3: %v\n%s", err, out)
	}
	return strings.Fields(string(out))
}

type query struct {
	name    string
	formula string // must be unsatisfiable
}

// prove checks that every query is unsatisfiable, and that a deliberately
// satisfiable control query is reported as sat (so a broken pipeline that
// answers "unsat" to everything cannot pass).
func prove(t *testing.T, decls string, qs []query) {
	t.Helper()
	var b strings.Builder
	for _, q := range append(qs, query{name: "control", formula: "(not (= learned (not learned)))"}) {
		fmt.Fprintf(&b, "(push)\n%s(assert %s)\n(check-sat)\n(pop)\n", decls, q.formula)
	}
	got := z3(t, b.String())
	if len(got) != len(qs)+1 {
		t.Fatalf("z3 answered %d queries, want %d: %v", len(got), len(qs)+1, got)
	}
	if got[len(got)-1] != "sat" {
		t.Fatalf("control query: z3 says %s, want sat", got[len(got)-1])
	}
	for i, q := range qs {
		if got[i] != "unsat" {
			t.Errorf("%s: z3 says %s (a counterexample exists)", q.name, got[i])
		}
	}
	t.Logf("z3 proved %d properties", len(qs))
}

func testHost() (spec.HostNet, egress.HostNet) {
	var s spec.HostNet
	var e egress.HostNet
	for _, a := range []string{"192.168.1.10", "2404:db8:1::10"} {
		s.Addrs = append(s.Addrs, netip.MustParseAddr(a))
		e.Addrs = append(e.Addrs, netip.MustParseAddr(a))
	}
	for _, p := range []string{"192.168.1.0/24", "2404:db8:1::/64"} {
		s.OnLink = append(s.OnLink, netip.MustParsePrefix(p))
		e.OnLink = append(e.OnLink, netip.MustParsePrefix(p))
	}
	return s, e
}

// Marker entries identify which rule a list came from.
var (
	allowMarker = netip.MustParsePrefix("203.0.113.0/24")
	denyMarker  = netip.MustParsePrefix("198.51.100.0/24")
)

func specPolicy(t *testing.T, mode, def string, allow, deny bool) spec.NetPolicy {
	raw := spec.RawNetwork{Mode: mode, Default: def}
	if allow {
		raw.Allow = []string{allowMarker.String()}
	}
	if deny {
		raw.Deny = []string{denyMarker.String()}
	}
	cfg, err := spec.ParseConfig(spec.Env{}, spec.RawConfig{Network: raw})
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Res.Net
}

// setVar names the Boolean that stands for membership in ps.
func setVar(ps []netip.Prefix) string {
	switch {
	case len(ps) == 0:
		return "false"
	case slices.Equal(ps, []netip.Prefix{allowMarker}):
		return "A"
	case slices.Equal(ps, []netip.Prefix{denyMarker}):
		return "D"
	case slices.Contains(ps, netip.MustParsePrefix("127.0.0.0/8")):
		return "N"
	case slices.Contains(ps, netip.MustParsePrefix("10.0.0.0/8")):
		return "P"
	}
	return "unknown"
}

// rulesFormula is the first-match decision of rules over set Booleans.
func rulesFormula(rules []egress.Rule) string {
	acc := "true" // no rule matches: accepted
	for i := len(rules) - 1; i >= 0; i-- {
		r := rules[i]
		var match string
		switch r.Match {
		case egress.MatchPrefixes:
			match = setVar(r.Prefixes)
		case egress.MatchLearned:
			match = "learned"
		case egress.MatchAll:
			match = "true"
		}
		acc = fmt.Sprintf("(ite %s %v %s)", match, r.Verdict == egress.VerdictAccept, acc)
	}
	return acc
}

// reachFormula transcribes Reach's evaluation of a ReachShape.
func reachFormula(s spec.ReachShape) string {
	if !s.Enabled {
		return "false"
	}
	learned := "true"
	if s.Closed {
		learned = "learned"
	}
	return fmt.Sprintf("(and (not %s) (not %s) (or %s (and (not %s) %s)))",
		setVar(s.Never), setVar(s.Deny), setVar(s.Allow), setVar(s.Private), learned)
}

func TestEgressBuiltinSetsEqualSpec(t *testing.T) {
	sh, eh := testHost()
	internet := specPolicy(t, "internet", "", false, false)
	shape := spec.Shape(internet, sh)
	rules := egress.Rules(egress.Policy{Mode: egress.ModeInternet}, eh)
	var never, lan []netip.Prefix
	for _, r := range rules {
		switch setVar(r.Prefixes) {
		case "N":
			never = r.Prefixes
		case "P":
			lan = r.Prefixes
		}
	}
	if never == nil || lan == nil {
		t.Fatalf("cannot find the never and LAN rules in %v", rules)
	}
	var qs []query
	for _, fam := range []struct {
		name string
		bits int
		dom  string // the family's domain (IPv4-mapped IPv6 is IPv4 on the wire)
	}{
		{"IPv4", 32, "true"},
		{"IPv6", 128, "(not " + smtPrefix(netip.MustParsePrefix("::ffff:0:0/96")) + ")"},
	} {
		decls := fmt.Sprintf("(declare-const d (_ BitVec %d))\n(declare-const learned Bool)\n(assert %s)\n", fam.bits, fam.dom)
		qs = append(qs,
			query{fam.name + " never", fmt.Sprintf("(not (= %s %s))", smtIn(never, fam.bits), smtIn(shape.Never, fam.bits))},
			query{fam.name + " private", fmt.Sprintf("(not (= %s %s))", smtIn(lan, fam.bits), smtIn(shape.Private, fam.bits))},
		)
		prove(t, decls, qs)
		qs = nil
	}
}

func TestEgressRuleStructureEqualsSpec(t *testing.T) {
	sh, eh := testHost()
	decls := "(declare-const N Bool)\n(declare-const D Bool)\n(declare-const A Bool)\n(declare-const P Bool)\n(declare-const learned Bool)\n"
	var qs []query
	for _, mode := range []string{"internet", "lan"} {
		for _, def := range []string{"allow", "deny"} {
			for _, allow := range []bool{false, true} {
				for _, deny := range []bool{false, true} {
					sp := specPolicy(t, mode, def, allow, deny)
					pol, _, _ := egressPolicy(sp)
					rf := rulesFormula(egress.Rules(pol, eh))
					sf := reachFormula(spec.Shape(sp, sh))
					if strings.Contains(rf+sf, "unknown") {
						t.Fatalf("unidentified set in %s / %s", rf, sf)
					}
					qs = append(qs, query{
						name:    fmt.Sprintf("mode=%s default=%s allow=%v deny=%v", mode, def, allow, deny),
						formula: fmt.Sprintf("(not (= %s %s))", rf, sf),
					})
				}
			}
		}
	}
	prove(t, decls, qs)
}
