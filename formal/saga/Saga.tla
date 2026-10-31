-------------------------------- MODULE Saga --------------------------------
(***************************************************************************)
(* The host-side command saga of internal/tea/operation, run by several   *)
(* antic processes on one guest, under the guest's lock, with crashes.    *)
(*                                                                         *)
(* Every command plans 0..NRes commit steps (its plan length) and moves     *)
(* resources 1..length from the versions it found to a new version, one   *)
(* commit step at a time; a failed step compensates the committed ones in  *)
(* reverse (P1: atomic per command). A process can crash at any point: its *)
(* in-memory saga is lost and the kernel releases its flock.               *)
(*                                                                         *)
(* Journaling: before the first commit step the process durably opens a   *)
(* journal, and before each commit step it records the step's intent. The  *)
(* next process to take the lock first undoes every recorded intent (undo  *)
(* is idempotent, so a step that crashed before it applied is harmless)    *)
(* and clears the journal. A journal that cannot be cleared stays, and is  *)
(* recovered again.                                                        *)
(*                                                                         *)
(* PlanUnderLock: the process reads the state it plans from after taking  *)
(* the lock and recovering (FALSE models reading it before locking).       *)
(*                                                                         *)
(* Each action is one transition of the Go core (one effect and its        *)
(* result, see internal/spec/refine/saga_refine_test.go, which replays     *)
(* this model's state graph against operation.Update).                    *)
(*                                                                         *)
(* Consistent is P1 as other commands observe it: whenever no process holds *)
(* the lock and no journal is pending, every resource is at the version of *)
(* the last command that succeeded in changing it.                         *)
(***************************************************************************)
EXTENDS Naturals, Sequences

CONSTANTS Procs, NRes, MaxCmds, MaxCrashes, Journaling, PlanUnderLock, None

ASSUME /\ NRes \in Nat \ {0} /\ None \notin Procs
       /\ Journaling \in BOOLEAN /\ PlanUnderLock \in BOOLEAN

\* The resources; a plan of length n moves resources 1..n in order.
Res == 1..NRes
Plan(n) == [i \in 1..n |-> i]

Front(s) == SubSeq(s, 1, Len(s) - 1)
Last(s) == s[Len(s)]

VARIABLES
    val,      \* [Res -> Nat]: the version each resource is at
    lock,     \* the process holding the guest's lock, or None
    pc,       \* [Procs -> phase]
    base,     \* [Procs -> [Res -> Nat]]: the versions the process's command started from
    target,   \* [Procs -> Nat]: the version it installs
    todo,     \* [Procs -> Seq(Res)]: commit steps still to run
    done,     \* [Procs -> Seq(Res)]: committed steps (the undo stack), or
              \* the recorded intents still to undo while recovering
    plen,     \* [Procs -> 0..NRes]: the length of the process's plan
    journal,  \* None, or [base: [Res -> Nat], intents: Seq(Res)] (durable)
    cmds,     \* commands started so far (bounded by MaxCmds)
    crashes,  \* crashes so far (bounded by MaxCrashes)
    lastOk    \* [Res -> Nat]: the version of the last command that changed each resource

vars == <<val, lock, pc, base, target, todo, done, plen, journal, cmds, crashes, lastOk>>

\* "done": the saga reported; the process still holds the lock (and runs
\* irreversible clean-up after a success).
Phases == {"idle", "waiting", "recover", "plan", "prepare", "commit", "compensate", "done"}

TypeOK ==
    /\ val \in [Res -> Nat]
    /\ lock \in Procs \cup {None}
    /\ pc \in [Procs -> Phases]
    /\ todo \in [Procs -> Seq(Res)] /\ done \in [Procs -> Seq(Res)]
    /\ plen \in [Procs -> 0..NRes]
    /\ journal = None \/ journal \in [base: [Res -> Nat], intents: Seq(Res)]

Init ==
    /\ val = [r \in Res |-> 0]
    /\ lock = None
    /\ pc = [p \in Procs |-> "idle"]
    /\ base = [p \in Procs |-> [r \in Res |-> 0]]
    /\ target = [p \in Procs |-> 0]
    /\ todo = [p \in Procs |-> <<>>]
    /\ done = [p \in Procs |-> <<>>]
    /\ plen = [p \in Procs |-> 0]
    /\ journal = None
    /\ cmds = 0
    /\ crashes = 0
    /\ lastOk = [r \in Res |-> 0]

\* Reading the state to plan from before taking the lock (PlanUnderLock = FALSE).
ReadEarly(p) ==
    /\ ~PlanUnderLock
    /\ pc[p] = "idle" /\ cmds < MaxCmds
    /\ base' = [base EXCEPT ![p] = val]
    /\ pc' = [pc EXCEPT ![p] = "waiting"]
    /\ UNCHANGED <<val, lock, target, todo, done, journal, cmds, crashes, lastOk, plen>>

\* Taking the lock; a pending journal is recovered first (MsgRecover).
Acquire(p) ==
    /\ lock = None
    /\ IF PlanUnderLock THEN pc[p] = "idle" /\ cmds < MaxCmds ELSE pc[p] = "waiting"
    /\ lock' = p
    /\ cmds' = cmds + 1
    /\ target' = [target EXCEPT ![p] = cmds + 1]
    /\ pc' = [pc EXCEPT ![p] = IF journal # None THEN "recover" ELSE "plan"]
    /\ done' = [done EXCEPT ![p] = IF journal # None THEN journal.intents ELSE <<>>]
    /\ UNCHANGED <<val, base, todo, journal, crashes, lastOk, plen>>

\* Recovery undoes the recorded intents in reverse (the journal is not
\* rewritten: a recovery that crashes is redone, idempotently).
Recover(p) ==
    /\ pc[p] = "recover" /\ done[p] # <<>>
    /\ val' = [val EXCEPT ![Last(done[p])] = journal.base[Last(done[p])]]
    /\ done' = [done EXCEPT ![p] = Front(@)]
    /\ UNCHANGED <<lock, pc, base, target, todo, journal, cmds, crashes, lastOk, plen>>

RecoverDone(p) ==
    /\ pc[p] = "recover" /\ done[p] = <<>>
    /\ journal' = None
    /\ pc' = [pc EXCEPT ![p] = "plan"]
    /\ UNCHANGED <<val, lock, base, target, todo, done, cmds, crashes, lastOk, plen>>

\* Clearing the journal failed: the command is not run; the journal stays.
RecoverFail(p) ==
    /\ pc[p] = "recover" /\ done[p] = <<>>
    /\ pc' = [pc EXCEPT ![p] = "done"]
    /\ UNCHANGED <<val, lock, base, target, todo, done, journal, cmds, crashes, lastOk, plen>>

\* Planning (MsgCommand): the process reads the state it changes and plans
\* n commit steps; a command with none (a no-op) has nothing to prepare.
PlanCmd(p) ==
    /\ pc[p] = "plan"
    /\ \E n \in 0..NRes :
        /\ plen' = [plen EXCEPT ![p] = n]
        /\ pc' = [pc EXCEPT ![p] = IF n = 0 THEN "done" ELSE "prepare"]
    /\ base' = IF PlanUnderLock THEN [base EXCEPT ![p] = val] ELSE base
    /\ UNCHANGED <<val, lock, target, todo, done, journal, cmds, crashes, lastOk>>

\* Prepare checks and builds (changing nothing observable), then opens the journal.
Prepare(p) ==
    /\ pc[p] = "prepare"
    /\ todo' = [todo EXCEPT ![p] = Plan(plen[p])]
    /\ done' = [done EXCEPT ![p] = <<>>]
    /\ journal' = IF Journaling THEN [base |-> base[p], intents |-> <<>>] ELSE None
    /\ pc' = [pc EXCEPT ![p] = "commit"]
    /\ UNCHANGED <<val, lock, base, target, cmds, crashes, lastOk, plen>>

PrepareFail(p) ==
    /\ pc[p] = "prepare"
    /\ pc' = [pc EXCEPT ![p] = "done"]
    /\ UNCHANGED <<val, lock, base, target, todo, done, journal, cmds, crashes, lastOk, plen>>

\* With journaling, a step's intent is recorded before the step runs.
Intent(p) ==
    /\ Journaling
    /\ pc[p] = "commit" /\ todo[p] # <<>>
    /\ Len(journal.intents) = Len(done[p])
    /\ journal' = [journal EXCEPT !.intents = Append(@, Head(todo[p]))]
    /\ UNCHANGED <<val, lock, pc, base, target, todo, done, cmds, crashes, lastOk, plen>>

IntentFail(p) ==
    /\ Journaling
    /\ pc[p] = "commit" /\ todo[p] # <<>>
    /\ Len(journal.intents) = Len(done[p])
    /\ todo' = [todo EXCEPT ![p] = <<>>]
    /\ pc' = [pc EXCEPT ![p] = "compensate"]
    /\ UNCHANGED <<val, lock, base, target, done, journal, cmds, crashes, lastOk, plen>>

\* (IF, not \/: TLC splits disjunctions in actions into separate branches.)
Ready(p) == IF Journaling THEN Len(journal.intents) = Len(done[p]) + 1 ELSE TRUE

Apply(p) ==
    /\ pc[p] = "commit" /\ todo[p] # <<>> /\ Ready(p)
    /\ val' = [val EXCEPT ![Head(todo[p])] = target[p]]
    /\ done' = [done EXCEPT ![p] = Append(@, Head(todo[p]))]
    /\ todo' = [todo EXCEPT ![p] = Tail(@)]
    /\ UNCHANGED <<lock, pc, base, target, journal, cmds, crashes, lastOk, plen>>

\* A failed step changes nothing (compound effects are atomic in the shell).
ApplyFail(p) ==
    /\ pc[p] = "commit" /\ todo[p] # <<>> /\ Ready(p)
    /\ todo' = [todo EXCEPT ![p] = <<>>]
    /\ pc' = [pc EXCEPT ![p] = "compensate"]
    /\ UNCHANGED <<val, lock, base, target, done, journal, cmds, crashes, lastOk, plen>>

Compensate(p) ==
    /\ pc[p] = "compensate" /\ done[p] # <<>>
    /\ val' = [val EXCEPT ![Last(done[p])] = base[p][Last(done[p])]]
    /\ done' = [done EXCEPT ![p] = Front(@)]
    /\ UNCHANGED <<lock, pc, base, target, todo, journal, cmds, crashes, lastOk, plen>>

CompensateDone(p) ==
    /\ pc[p] = "compensate" /\ done[p] = <<>>
    /\ journal' = None
    /\ pc' = [pc EXCEPT ![p] = "done"]
    /\ UNCHANGED <<val, lock, base, target, todo, done, cmds, crashes, lastOk, plen>>

\* Clearing the journal failed after the undo: it is recovered (again) later.
CompensateDoneFail(p) ==
    /\ Journaling
    /\ pc[p] = "compensate" /\ done[p] = <<>>
    /\ pc' = [pc EXCEPT ![p] = "done"]
    /\ UNCHANGED <<val, lock, base, target, todo, done, journal, cmds, crashes, lastOk, plen>>

\* Every step committed: clearing the journal makes the command succeed.
Finish(p) ==
    /\ pc[p] = "commit" /\ todo[p] = <<>>
    /\ journal' = None
    /\ lastOk' = [r \in Res |-> IF r <= plen[p] THEN target[p] ELSE lastOk[r]]
    /\ done' = [done EXCEPT ![p] = <<>>]
    /\ pc' = [pc EXCEPT ![p] = "done"]
    /\ UNCHANGED <<val, lock, base, target, todo, cmds, crashes, plen>>

\* A journal that cannot be cleared would undo the command later, so the
\* command is undone now.
FinishFail(p) ==
    /\ Journaling
    /\ pc[p] = "commit" /\ todo[p] = <<>>
    /\ pc' = [pc EXCEPT ![p] = "compensate"]
    /\ UNCHANGED <<val, lock, base, target, todo, done, journal, cmds, crashes, lastOk, plen>>

\* The process reports and releases the lock.
Release(p) ==
    /\ pc[p] = "done"
    /\ lock' = None
    /\ pc' = [pc EXCEPT ![p] = "idle"]
    /\ plen' = [plen EXCEPT ![p] = 0]
    /\ UNCHANGED <<val, base, target, todo, done, journal, cmds, crashes, lastOk>>

\* A crash loses the process's memory; the kernel releases its flock; the
\* journal survives.
Crash(p) ==
    /\ crashes < MaxCrashes /\ pc[p] # "idle"
    /\ crashes' = crashes + 1
    /\ lock' = IF lock = p THEN None ELSE lock
    /\ pc' = [pc EXCEPT ![p] = "idle"]
    /\ todo' = [todo EXCEPT ![p] = <<>>]
    /\ done' = [done EXCEPT ![p] = <<>>]
    /\ plen' = [plen EXCEPT ![p] = 0]
    /\ UNCHANGED <<val, base, target, journal, cmds, lastOk>>

\* Everything finished: let the behavior stutter instead of deadlocking.
Quiescent ==
    /\ cmds >= MaxCmds /\ \A p \in Procs : pc[p] = "idle"
    /\ UNCHANGED vars

Progress(p) ==
    \/ ReadEarly(p) \/ Acquire(p) \/ Recover(p) \/ RecoverDone(p) \/ PlanCmd(p)
    \/ Prepare(p) \/ Intent(p) \/ Apply(p) \/ Compensate(p) \/ CompensateDone(p)
    \/ Finish(p) \/ Release(p)

Fail(p) ==
    \/ RecoverFail(p) \/ PrepareFail(p) \/ IntentFail(p) \/ ApplyFail(p)
    \/ CompensateDoneFail(p) \/ FinishFail(p)

Next ==
    \/ \E p \in Procs : Progress(p) \/ Fail(p) \/ Crash(p)
    \/ Quiescent

Spec == Init /\ [][Next]_vars /\ \A p \in Procs : WF_vars(Progress(p))

-----------------------------------------------------------------------------
\* P1 as other commands observe it.
Consistent == (lock = None /\ journal = None) => \A r \in Res : val[r] = lastOk[r]

\* Every process eventually finishes (no saga or recovery gets stuck).
Termination == <>[](\A p \in Procs : pc[p] = "idle")
=============================================================================
