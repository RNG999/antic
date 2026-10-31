package refine_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// The check that the hand-written Horn clauses (saga_chc_test.go) cover
// Saga.tla: every edge of TLC's state graph for SagaJournal.cfg, abstracted
// to the lock holder as the clauses are, must be an instance of a rule of
// chcModel(true) between the same phases with the same action. Spacer's
// proof is about the clauses, so it carries over to the model only if the
// clauses allow at least what the model does; this test establishes that on
// the bounded instance, and that every rule is used (none is spurious).
//
// The check is one step at a time: a clause variable the abstraction leaves
// free in a phase (stale in the clauses, forgotten by the model, such as the
// command's base while recovering) is existentially chosen per edge, so a
// rule that wrongly reads such a variable is not caught here.

// chcPhase maps the lock holder's pc to its predicate.
var chcPhase = map[string]string{
	"idle": phIdle, "recover": phRecover, "plan": phPlan, "prepare": phPrepare,
	"commit": phCommit, "compensate": phCompensate, "done": phDone,
}

// tlaInts parses a TLC sequence of integers.
func tlaInts(seq string) []string {
	inner := strings.TrimSuffix(strings.TrimPrefix(seq, "<<"), ">>")
	if inner == "" {
		return nil
	}
	return strings.Split(inner, ",")
}

// recField returns field f of a canonical record "[a|->x,b|->y]" whose
// values contain no "|->".
func recField(rec, f string) string {
	for _, kv := range splitTop(strings.TrimSuffix(strings.TrimPrefix(rec, "["), "]")) {
		if k, v, ok := strings.Cut(kv, "|->"); ok && k == f {
			return v
		}
	}
	return ""
}

// splitTop splits s at commas outside << >>.
func splitTop(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch {
		case strings.HasPrefix(s[i:], "<<"):
			depth++
			i++
		case strings.HasPrefix(s[i:], ">>"):
			depth--
			i++
		case s[i] == ',' && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// chcAbstract maps a TLC state to the holder's phase and the values of the
// clause variables it determines (the others are free: stale in the
// clauses, forgotten in the model). suffix renames the variables.
func chcAbstract(vs map[string]string, suffix string) (string, []string) {
	holder := vs["lock"]
	pc := "idle"
	if holder != "None" {
		pc = strings.Trim(fnAt(vs["pc"], holder), `"`)
	}
	ph := chcPhase[pc]
	var eqs []string
	eq := func(v, val string) { eqs = append(eqs, fmt.Sprintf("(= %s%s %s)", v, suffix, val)) }
	for i, x := range tlaInts(vs["val"]) {
		eq(fmt.Sprintf("v%d", i+1), x)
	}
	for i, x := range tlaInts(vs["lastOk"]) {
		eq(fmt.Sprintf("lo%d", i+1), x)
	}
	eq("cmds", vs["cmds"])
	if vs["journal"] == "None" {
		eq("jon", "false")
	} else {
		eq("jon", "true")
		for i, x := range tlaInts(recField(vs["journal"], "base")) {
			eq(fmt.Sprintf("jb%d", i+1), x)
		}
		eq("ji", fmt.Sprint(len(tlaInts(recField(vs["journal"], "intents")))))
	}
	if ph != phIdle {
		eq("target", fnAt(vs["target"], holder))
	}
	switch ph {
	case phRecover, phCommit, phCompensate:
		eq("k", fmt.Sprint(len(tlaInts(fnAt(vs["done"], holder)))))
	}
	switch ph {
	case phPrepare, phCommit, phCompensate:
		eq("n", fnAt(vs["plen"], holder))
		for i, x := range tlaInts(fnAt(vs["base"], holder)) {
			eq(fmt.Sprintf("b%d", i+1), x)
		}
	}
	return ph, eqs
}

// transition renders rule r as a formula over the variables and their
// "_n" successors.
func (sys chcSystem) transition(r chcRule) string {
	cur := strings.Fields(sys.state())
	next := r.next(sys.state())
	parts := []string{r.guard}
	for i, v := range cur {
		parts = append(parts, fmt.Sprintf("(= %s_n %s)", v, next[i]))
	}
	return "(and " + strings.Join(parts, " ") + ")"
}

// chcCoverage returns how the rules of sys fail to cover the edges of g.
func chcCoverage(t *testing.T, g traceGraph, sys chcSystem) (problems []string, edgeCount, queryCount int) {
	t.Helper()
	var decls strings.Builder
	for _, v := range sys.ints {
		fmt.Fprintf(&decls, "(declare-const %s Int)(declare-const %s_n Int)", v, v)
	}
	decls.WriteString("(declare-const jon Bool)(declare-const jon_n Bool)(declare-const m Int)\n")

	// Abstract edges, each with the rules that may implement it.
	type query struct{ edge, rule int }
	var edges []string
	var queries []query
	var script strings.Builder
	seen := map[string]bool{}
	for from, succ := range g.next {
		for _, e := range succ {
			pf, ef := chcAbstract(g.vars[from], "")
			pt, et := chcAbstract(g.vars[e[0]], "_n")
			key := pf + " " + strings.Join(ef, "") + " " + e[1] + " " + pt + " " + strings.Join(et, "")
			if seen[key] {
				continue
			}
			seen[key] = true
			edges = append(edges, fmt.Sprintf("%s: %s -> %s", e[1], pf, pt))
			found := false
			for ri, r := range sys.rules {
				if r.name != e[1] || r.from != pf || r.to != pt {
					continue
				}
				found = true
				queries = append(queries, query{len(edges) - 1, ri})
				fmt.Fprintf(&script, "(push)%s(assert (and %s %s %s))(check-sat)(pop)\n",
					decls.String(), strings.Join(ef, " "), strings.Join(et, " "), sys.transition(r))
			}
			if !found {
				problems = append(problems, fmt.Sprintf("no rule %s from %s to %s (TLC edge %s)", e[1], pf, pt, key))
			}
		}
	}
	answers := z3(t, script.String())
	if len(answers) != len(queries) {
		t.Fatalf("z3 answered %d of %d queries", len(answers), len(queries))
	}
	covered := map[int]bool{}
	used := map[int]bool{}
	for i, q := range queries {
		if answers[i] == "sat" {
			covered[q.edge], used[q.rule] = true, true
		}
	}
	for i, e := range edges {
		if !covered[i] {
			problems = append(problems, fmt.Sprintf("TLC edge %s is not an instance of its rules", e))
		}
	}
	var unused []string
	for ri, r := range sys.rules {
		if !used[ri] {
			unused = append(unused, fmt.Sprintf("%s (%s -> %s)", r.name, r.from, r.to))
		}
	}
	sort.Strings(unused)
	if len(unused) > 0 {
		problems = append(problems, fmt.Sprintf("rules no TLC edge uses (spurious, or the bounds are too small): %v", unused))
	}
	return problems, len(edges), len(queries)
}

func TestCHCCoversSagaModel(t *testing.T) {
	g := indexGraph(readTLCGraph(t, "SagaJournal.cfg"))
	sys := chcModel(true)
	problems, edges, queries := chcCoverage(t, g, sys)
	for _, p := range problems {
		t.Error(p)
	}
	t.Logf("%d abstract edges of %d TLC states covered by %d rules (%d queries)", edges, len(g.vars), len(sys.rules), queries)

	// Mutants of the clauses must be caught: a missing rule, an undo that
	// restores the wrong version, and a guard that is too strong.
	mutate := func(name string, f func(*chcRule) bool) {
		m := chcModel(true)
		var rules []chcRule
		changed := false
		for _, r := range m.rules {
			if !changed && f(&r) {
				changed = true
				if r.name == "" {
					continue // dropped
				}
			}
			rules = append(rules, r)
		}
		m.rules = rules
		if !changed {
			t.Fatalf("mutant %q changed nothing", name)
		}
		if ps, _, _ := chcCoverage(t, g, m); len(ps) == 0 {
			t.Errorf("mutant %q: the coverage check did not detect it", name)
		}
	}
	mutate("no IntentFail", func(r *chcRule) bool {
		if r.name == "IntentFail" {
			r.name = ""
			return true
		}
		return false
	})
	mutate("recovery restores the new version", func(r *chcRule) bool {
		if r.name != "Recover" {
			return false
		}
		upd := map[string]string{}
		for k, v := range r.upd {
			for i := 1; i <= nRes; i++ {
				v = strings.ReplaceAll(v, fmt.Sprintf("jb%d", i), "target")
			}
			upd[k] = v
		}
		r.upd = upd
		return true
	})
	mutate("apply only the first step", func(r *chcRule) bool {
		if r.name != "Apply" {
			return false
		}
		r.guard = "(and " + r.guard + " (= k 0))"
		return true
	})
}
