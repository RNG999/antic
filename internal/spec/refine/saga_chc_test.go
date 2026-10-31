package refine_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The unbounded counterpart of formal/saga/Saga.tla, as Constrained Horn
// Clauses solved by Z3's Spacer engine (IC3/PDR): TLC checks two processes,
// three commands and two crashes; Spacer finds an inductive invariant that
// proves Consistent (P1 as other commands observe it) for any number of
// commands, crashes and processes.
//
// Abstraction: a command plans from the state it reads after taking the
// guest's lock and recovering, so a process without the lock holds no
// state; any number of processes is therefore represented by the current
// lock holder, and a crash resets it. A plan of length n moves resources
// 1..n in order, so the undo stack (or, while recovering, the recorded
// intents still to undo), the remaining steps and the journal's intents are
// the counts k and ji. Versions and the command counter are unbounded
// integers. The rules follow Saga.tla action by action; each phase of the
// lock holder (Saga.tla's pc) has its own invariant predicate, which keeps
// the inductive invariant Spacer must find small per control location.

// The phases of the lock holder; idle means no process holds the lock.
const (
	phIdle       = "Idle"
	phRecover    = "Recover"
	phPlan       = "Plan"
	phPrepare    = "Prepare"
	phCommit     = "Commit"
	phCompensate = "Compensate"
	phDone       = "Done" // reported, still holding the lock
)

// chcRule is one transition: from phase from, when guard holds, to phase to
// with the variables upd names replaced (the others unchanged). Guards and
// updates may use the free m (the plan length PlanCmd chooses).
type chcRule struct {
	name, from, guard, to string
	upd                   map[string]string
}

// chcSystem is the saga as a transition system over integer variables ints
// and the Boolean jon.
type chcSystem struct {
	ints   []string
	phases []string
	rules  []chcRule
	safety string // the bad states of phIdle
}

// chcModel builds the saga's transition system with or without a journal.
func chcModel(journaling bool) chcSystem {
	phases := []string{phIdle, phRecover, phPlan, phPrepare, phCommit, phCompensate, phDone}
	// Per resource i: v (its version), b (the command's base), jb (the
	// journal's base), lo (the last successful version); then the scalars.
	vec := func(prefix string) []string {
		var out []string
		for i := 1; i <= nRes; i++ {
			out = append(out, fmt.Sprintf("%s%d", prefix, i))
		}
		return out
	}
	vs, bs, jbs, los := vec("v"), vec("b"), vec("jb"), vec("lo")
	ints := slices.Concat(vs, bs, jbs, los, []string{"target", "k", "n", "ji", "cmds"})
	var rules []chcRule
	add := func(name, from, guard, to string, upd map[string]string) {
		rules = append(rules, chcRule{name: name, from: from, guard: guard, to: to, upd: upd})
	}
	// set assigns resource idx the value at[i] (the i-th resource's value).
	set := func(idx string, at func(i int) string) map[string]string {
		out := map[string]string{}
		for i, v := range vs {
			out[v] = fmt.Sprintf("(ite (= %s %d) %s %s)", idx, i+1, at(i), v)
		}
		return out
	}
	each := func(xs []string, val func(i int) string) map[string]string {
		out := map[string]string{}
		for i, x := range xs {
			out[x] = val(i)
		}
		return out
	}
	merge := func(ms ...map[string]string) map[string]string {
		out := map[string]string{}
		for _, m := range ms {
			for k, v := range m {
				out[k] = v
			}
		}
		return out
	}
	and := func(cs ...string) string { return "(and " + strings.Join(cs, " ") + ")" }
	target := func(int) string { return "target" }
	none, some, all, more := "(= k 0)", "(> k 0)", "(= k n)", "(< k n)"
	// Acquire: a new command takes the lock; a pending journal is recovered first.
	acquire := map[string]string{"cmds": "(+ cmds 1)", "target": "(+ cmds 1)"}
	add("Acquire", phIdle, "jon", phRecover, merge(acquire, map[string]string{"k": "ji"}))
	add("Acquire", phIdle, "(not jon)", phPlan, merge(acquire, map[string]string{"k": "0"}))
	// Recover: undo the recorded intents in reverse (idempotent), then clear
	// the journal, or fail to clear it and stop.
	add("Recover", phRecover, some, phRecover, merge(set("k", func(i int) string { return jbs[i] }), map[string]string{"k": "(- k 1)"}))
	add("RecoverDone", phRecover, none, phPlan, map[string]string{"jon": "false"})
	add("RecoverFail", phRecover, none, phDone, nil)
	// Plan m commit steps from the state read under the lock; a no-op has
	// nothing to prepare.
	read := merge(each(bs, func(i int) string { return vs[i] }), map[string]string{"n": "m"})
	add("PlanCmd", phPlan, "(= m 0)", phDone, read)
	add("PlanCmd", phPlan, fmt.Sprintf("(and (>= m 1) (<= m %d))", nRes), phPrepare, read)
	// Prepare (changes nothing observable) and open the journal, or fail.
	prep := map[string]string{"k": "0"}
	if journaling {
		prep = merge(prep, each(jbs, func(i int) string { return bs[i] }), map[string]string{"jon": "true", "ji": "0"})
	}
	add("Prepare", phPrepare, "true", phCommit, prep)
	add("PrepareFail", phPrepare, "true", phDone, nil)
	// Commit: record the intent (journaling) or fail to, then apply the step or fail.
	ready := "true"
	if journaling {
		add("Intent", phCommit, and(more, "(= ji k)"), phCommit, map[string]string{"ji": "(+ k 1)"})
		add("IntentFail", phCommit, and(more, "(= ji k)"), phCompensate, nil)
		ready = "(= ji (+ k 1))"
	}
	add("Apply", phCommit, and(more, ready), phCommit, merge(set("(+ k 1)", target), map[string]string{"k": "(+ k 1)"}))
	add("ApplyFail", phCommit, and(more, ready), phCompensate, nil)
	// Compensate in reverse, then clear the journal (or fail to, leaving it).
	add("Compensate", phCompensate, some, phCompensate, merge(set("k", func(i int) string { return bs[i] }), map[string]string{"k": "(- k 1)"}))
	add("CompensateDone", phCompensate, none, phDone, map[string]string{"jon": "false"})
	if journaling {
		add("CompensateDoneFail", phCompensate, none, phDone, nil)
	}
	// Finish by clearing the journal, or compensate when it cannot be cleared.
	finish := each(los, func(i int) string { return fmt.Sprintf("(ite (<= %d n) target %s)", i+1, los[i]) })
	add("Finish", phCommit, all, phDone, merge(finish, map[string]string{"jon": "false", "k": "0"}))
	if journaling {
		add("FinishFail", phCommit, all, phCompensate, nil)
	}
	// Release the lock after reporting.
	add("Release", phDone, "true", phIdle, nil)
	// Crash: the holder's memory is lost and its flock released; the journal survives.
	for _, ph := range phases[1:] {
		add("Crash", ph, "true", phIdle, map[string]string{"k": "0"})
	}
	// Safety: with the lock free and no journal pending, every resource is
	// at the version of the last command that succeeded in changing it.
	var atLast []string
	for i := range vs {
		atLast = append(atLast, fmt.Sprintf("(= %s %s)", vs[i], los[i]))
	}
	return chcSystem{ints: ints, phases: phases, rules: rules,
		safety: fmt.Sprintf("(and (not jon) (not (and %s)))", strings.Join(atLast, " "))}
}

// state is the argument list of a phase predicate.
func (sys chcSystem) state() string { return strings.Join(sys.ints, " ") + " jon" }

// sig declares the state variables and m for a quantifier.
func (sys chcSystem) sig() string {
	var sig []string
	for _, v := range sys.ints {
		sig = append(sig, "("+v+" Int)")
	}
	return strings.Join(append(sig, "(jon Bool)", "(m Int)"), " ")
}

// next is the state after r: the arguments of its target predicate.
func (r chcRule) next(state string) []string {
	next := strings.Fields(state)
	for i, v := range next {
		if e, ok := r.upd[v]; ok {
			next[i] = e
		}
	}
	return next
}

// chcSaga renders the Horn clauses of the saga with or without a journal.
func chcSaga(journaling bool) string {
	sys := chcModel(journaling)
	state, sig := sys.state(), sys.sig()
	var b strings.Builder
	b.WriteString("(set-logic HORN)\n")
	sorts := strings.Repeat("Int ", len(sys.ints)) + "Bool"
	for _, ph := range sys.phases {
		fmt.Fprintf(&b, "(declare-fun %s (%s) Bool)\n", ph, sorts)
	}
	fmt.Fprintf(&b, "(assert (%s %s))\n", phIdle, strings.Repeat("0 ", len(sys.ints))+"false")
	for _, r := range sys.rules {
		fmt.Fprintf(&b, "(assert (forall (%s) (=> (and (%s %s) %s) (%s %s))))\n", sig, r.from, state, r.guard, r.to, strings.Join(r.next(state), " "))
	}
	fmt.Fprintf(&b, "(assert (forall (%s) (=> (and (%s %s) %s) false)))\n", sig, phIdle, state, sys.safety)
	b.WriteString("(check-sat)\n")
	return b.String()
}

// TestSagaConsistentWithoutBounds: with a journal, the Horn clauses are satisfiable
// (Spacer found an inductive invariant: P1 holds without bounds); without a
// journal they are unsatisfiable (a crash breaks P1), which also checks that
// the encoding can fail.
func TestSagaConsistentWithoutBounds(t *testing.T) {
	if got := z3(t, chcSaga(true)); len(got) != 1 || got[0] != "sat" {
		t.Fatalf("with a journal: z3 says %v, want sat (an inductive invariant exists)", got)
	}
	if got := z3(t, chcSaga(false)); len(got) != 1 || got[0] != "unsat" {
		t.Fatalf("without a journal: z3 says %v, want unsat (a counterexample exists)", got)
	}
}
