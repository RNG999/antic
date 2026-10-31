package refine_test

import (
	"fmt"
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
// lock holder, and a crash resets it. The plan moves resources 1..nRes in
// order (as the harness of saga_refine_test.go), so the undo stack (or, while recovering, the recorded intents
// still to undo), the remaining steps and the journal's intents are the
// counts k and ji. Versions and the command counter are unbounded integers.
// The rules follow Saga.tla action by action; each phase of the lock holder
// (Saga.tla's pc) has its own invariant predicate, which keeps the
// inductive invariant Spacer must find small per control location.

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

// chcSaga renders the Horn clauses of the saga with or without a journal.
func chcSaga(journaling bool) string {
	phases := []string{phIdle, phRecover, phPlan, phPrepare, phCommit, phCompensate, phDone}
	// State: v1..vN base target k jon jbase ji cmds lastOk
	var vs, vsig, atLast []string
	for i := 1; i <= nRes; i++ {
		v := fmt.Sprintf("v%d", i)
		vs, vsig, atLast = append(vs, v), append(vsig, "("+v+" Int)"), append(atLast, "(= "+v+" lastOk)")
	}
	state := strings.Join(vs, " ") + " base target k jon jbase ji cmds lastOk"
	sig := strings.Join(vsig, " ") + " (base Int) (target Int) (k Int) (jon Bool) (jbase Int) (ji Int) (cmds Int) (lastOk Int)"
	sorts := strings.Repeat("Int ", nRes) + "Int Int Int Bool Int Int Int Int"
	initial := strings.Repeat("0 ", nRes) + "0 0 0 false 0 0 0 0"
	n := fmt.Sprint(nRes)
	var b strings.Builder
	b.WriteString("(set-logic HORN)\n")
	for _, ph := range phases {
		fmt.Fprintf(&b, "(declare-fun %s (%s) Bool)\n", ph, sorts)
	}
	// add renders "from(s) ∧ guard ⇒ to(s')" where s' replaces the named
	// variables.
	add := func(from, guard, to string, upd map[string]string) {
		next := strings.Fields(state)
		for i, v := range next {
			if e, ok := upd[v]; ok {
				next[i] = e
			}
		}
		fmt.Fprintf(&b, "(assert (forall (%s) (=> (and (%s %s) %s) (%s %s))))\n", sig, from, state, guard, to, strings.Join(next, " "))
	}
	// set assigns val to the resource numbered idx.
	set := func(idx, val string) map[string]string {
		out := map[string]string{}
		for i, v := range vs {
			out[v] = fmt.Sprintf("(ite (= %s %d) %s %s)", idx, i+1, val, v)
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
	none, some, all := "(= k 0)", "(> k 0)", "(= k "+n+")"
	more := "(< k " + n + ")"
	// Init.
	fmt.Fprintf(&b, "(assert (%s %s))\n", phIdle, initial)
	// Acquire: a new command takes the lock; a pending journal is recovered first.
	acquire := map[string]string{"cmds": "(+ cmds 1)", "target": "(+ cmds 1)"}
	add(phIdle, "jon", phRecover, merge(acquire, map[string]string{"k": "ji"}))
	add(phIdle, "(not jon)", phPlan, merge(acquire, map[string]string{"k": "0"}))
	// Recover: undo the recorded intents in reverse (idempotent), then clear
	// the journal, or fail to clear it and stop.
	add(phRecover, some, phRecover, merge(set("k", "jbase"), map[string]string{"k": "(- k 1)"}))
	add(phRecover, none, phPlan, map[string]string{"jon": "false"})
	add(phRecover, none, phDone, nil)
	// Plan from the state read under the lock.
	add(phPlan, "true", phPrepare, map[string]string{"base": vs[firstRes]})
	// Prepare (changes nothing observable) and open the journal, or fail.
	prep := map[string]string{"k": "0"}
	if journaling {
		prep = merge(prep, map[string]string{"jon": "true", "jbase": "base", "ji": "0"})
	}
	add(phPrepare, "true", phCommit, prep)
	add(phPrepare, "true", phDone, nil)
	// Commit: record the intent (journaling) or fail to, then apply the step or fail.
	ready := "true"
	if journaling {
		add(phCommit, and(more, "(= ji k)"), phCommit, map[string]string{"ji": "(+ k 1)"})
		add(phCommit, and(more, "(= ji k)"), phCompensate, nil)
		ready = "(= ji (+ k 1))"
	}
	add(phCommit, and(more, ready), phCommit, merge(set("(+ k 1)", "target"), map[string]string{"k": "(+ k 1)"}))
	add(phCommit, and(more, ready), phCompensate, nil)
	// Compensate in reverse, then clear the journal (or fail to, leaving it).
	add(phCompensate, some, phCompensate, merge(set("k", "base"), map[string]string{"k": "(- k 1)"}))
	add(phCompensate, none, phDone, map[string]string{"jon": "false"})
	if journaling {
		add(phCompensate, none, phDone, nil)
	}
	// Finish by clearing the journal, or compensate when it cannot be cleared.
	add(phCommit, all, phDone, map[string]string{"jon": "false", "lastOk": "target", "k": "0"})
	if journaling {
		add(phCommit, all, phCompensate, nil)
	}
	// Release the lock after reporting.
	add(phDone, "true", phIdle, nil)
	// Crash: the holder's memory is lost and its flock released; the journal survives.
	for _, ph := range phases[1:] {
		add(ph, "true", phIdle, map[string]string{"k": "0"})
	}
	// Safety: with the lock free and no journal pending, every resource is
	// at the version of the last command that succeeded.
	fmt.Fprintf(&b, "(assert (forall (%s) (=> (and (%s %s) (not jon) (not (and %s))) false)))\n", sig, phIdle, state, strings.Join(atLast, " "))
	b.WriteString("(check-sat)\n")
	return b.String()
}

// TestSpacerSagaUnbounded: with a journal, the Horn clauses are satisfiable
// (Spacer found an inductive invariant: P1 holds without bounds); without a
// journal they are unsatisfiable (a crash breaks P1), which also checks that
// the encoding can fail.
func TestSpacerSagaUnbounded(t *testing.T) {
	if got := z3(t, chcSaga(true)); len(got) != 1 || got[0] != "sat" {
		t.Fatalf("with a journal: z3 says %v, want sat (an inductive invariant exists)", got)
	}
	if got := z3(t, chcSaga(false)); len(got) != 1 || got[0] != "unsat" {
		t.Fatalf("without a journal: z3 says %v, want unsat (a counterexample exists)", got)
	}
}
