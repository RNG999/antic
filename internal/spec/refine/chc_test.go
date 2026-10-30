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
// guest's lock (PlanUnderLock), so a process without the lock holds no
// state; any number of processes is therefore represented by the current
// lock holder, and a crash resets it. The plan moves resources 1..3 in
// order, so the undo stack, the remaining steps and the journal's intents
// are the counts k and ji. Versions and the command counter are unbounded
// integers.
//
// Phases: 0 idle, 1 recover, 2 prepare, 3 commit, 4 compensate.
func chcSaga(journaling bool) string {
	// State: v1 v2 v3 held ph base target k jon jbase ji cmds lastOk
	sig := "(v1 Int) (v2 Int) (v3 Int) (held Bool) (ph Int) (base Int) (target Int) (k Int) (jon Bool) (jbase Int) (ji Int) (cmds Int) (lastOk Int)"
	state := "v1 v2 v3 held ph base target k jon jbase ji cmds lastOk"
	// rule renders "Inv(s) ∧ guard ⇒ Inv(s')" where s' replaces the named
	// variables.
	rule := func(guard string, upd map[string]string) string {
		next := strings.Fields(state)
		for i, v := range next {
			if e, ok := upd[v]; ok {
				next[i] = e
			}
		}
		return fmt.Sprintf("(assert (forall (%s) (=> (and (Inv %s) %s) (Inv %s))))\n", sig, state, guard, strings.Join(next, " "))
	}
	set := func(idx, val string) map[string]string {
		return map[string]string{
			"v1": fmt.Sprintf("(ite (= %s 1) %s v1)", idx, val),
			"v2": fmt.Sprintf("(ite (= %s 2) %s v2)", idx, val),
			"v3": fmt.Sprintf("(ite (= %s 3) %s v3)", idx, val),
		}
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
	var b strings.Builder
	b.WriteString("(set-logic HORN)\n(declare-fun Inv (Int Int Int Bool Int Int Int Int Bool Int Int Int Int) Bool)\n")
	// Init.
	fmt.Fprintf(&b, "(assert (Inv 0 0 0 false 0 0 0 0 false 0 0 0 0))\n")
	// Acquire: a new command takes the lock; a pending journal is recovered first.
	b.WriteString(rule("(and (not held) (= ph 0))", map[string]string{
		"held": "true", "cmds": "(+ cmds 1)", "target": "(+ cmds 1)", "ph": "(ite jon 1 2)"}))
	// Recover: undo the last recorded intent (idempotent), then clear the journal.
	b.WriteString(rule("(and (= ph 1) (> ji 0))", merge(set("ji", "jbase"), map[string]string{"ji": "(- ji 1)"})))
	b.WriteString(rule("(and (= ph 1) (= ji 0))", map[string]string{"jon": "false", "ph": "2"}))
	// Prepare (changes nothing observable), or fail and release.
	prep := map[string]string{"base": "v1", "k": "0", "ph": "3"}
	if journaling {
		prep = merge(prep, map[string]string{"jon": "true", "jbase": "v1", "ji": "0"})
	}
	b.WriteString(rule("(= ph 2)", prep))
	b.WriteString(rule("(= ph 2)", map[string]string{"held": "false", "ph": "0"}))
	// Commit: record the intent (journaling), then apply the step or fail.
	ready := "true"
	if journaling {
		b.WriteString(rule("(and (= ph 3) (< k 3) (= ji k))", map[string]string{"ji": "(+ k 1)"}))
		ready = "(= ji (+ k 1))"
	}
	b.WriteString(rule(fmt.Sprintf("(and (= ph 3) (< k 3) %s)", ready), merge(set("(+ k 1)", "target"), map[string]string{"k": "(+ k 1)"})))
	b.WriteString(rule(fmt.Sprintf("(and (= ph 3) (< k 3) %s)", ready), map[string]string{"ph": "4"}))
	// Compensate in reverse, then release.
	b.WriteString(rule("(and (= ph 4) (> k 0))", merge(set("k", "base"), map[string]string{"k": "(- k 1)"})))
	b.WriteString(rule("(and (= ph 4) (= k 0))", map[string]string{"held": "false", "jon": "false", "ph": "0"}))
	// Finish.
	b.WriteString(rule("(and (= ph 3) (= k 3))", map[string]string{"held": "false", "jon": "false", "lastOk": "target", "ph": "0"}))
	// Crash: the holder's memory is lost and its flock released; the journal survives.
	b.WriteString(rule("(not (= ph 0))", map[string]string{"held": "false", "ph": "0", "k": "0"}))
	// Safety: with the lock free and no journal pending, every resource is
	// at the version of the last command that succeeded.
	fmt.Fprintf(&b, "(assert (forall (%s) (=> (and (Inv %s) (not held) (not jon) (not (and (= v1 lastOk) (= v2 lastOk) (= v3 lastOk)))) false)))\n", sig, state)
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
