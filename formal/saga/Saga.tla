-------------------------------- MODULE Saga --------------------------------
(***************************************************************************)
(* The host-side command saga of internal/tea/operation, run by several   *)
(* antic processes on one guest, under the guest's lock, with crashes.    *)
(*                                                                         *)
(* Every command moves every resource (config file, system profile, power  *)
(* state, ...) from the version its predecessor left to a new version, one *)
(* commit step at a time; a failed step compensates the committed ones in  *)
(* reverse (P1: atomic per command). A process can crash at any point: its *)
(* in-memory saga is lost and the kernel releases its flock.               *)
(*                                                                         *)
(* Journaling: before each commit step the process durably records the    *)
(* step's intent; the next process to take the lock first undoes every     *)
(* recorded intent (undo is idempotent, so a step that crashed before it   *)
(* applied is harmless) and clears the journal.                            *)
(*                                                                         *)
(* PlanUnderLock: the process reads the state it plans from after taking  *)
(* the lock (FALSE models reading the config before locking, as the CLI   *)
(* does today for config edits).                                           *)
(*                                                                         *)
(* Consistent is P1 as other commands observe it: whenever no process holds *)
(* the lock and no journal is pending, every resource is at the version of *)
(* the last command that succeeded.                                        *)
(***************************************************************************)
EXTENDS Naturals, Sequences, FiniteSets

CONSTANTS Procs, Res, MaxCmds, MaxCrashes, Journaling, PlanUnderLock, None

ASSUME /\ Res # {} /\ None \notin Procs
       /\ Journaling \in BOOLEAN /\ PlanUnderLock \in BOOLEAN

\* The plan: one commit step per resource, in a fixed order.
Plan == CHOOSE s \in [1..Cardinality(Res) -> Res] : \A i, j \in DOMAIN s : i # j => s[i] # s[j]

Front(s) == SubSeq(s, 1, Len(s) - 1)
Last(s) == s[Len(s)]

VARIABLES
    val,      \* [Res -> Nat]: the version each resource is at
    lock,     \* the process holding the guest's lock, or None
    pc,       \* [Procs -> phase]
    base,     \* [Procs -> Nat]: the version the process's command started from
    target,   \* [Procs -> Nat]: the version it installs
    todo,     \* [Procs -> Seq(Res)]: commit steps still to run
    done,     \* [Procs -> Seq(Res)]: committed steps (the undo stack)
    journal,  \* None, or [base: Nat, intents: Seq(Res)] (durable)
    cmds,     \* commands started so far (bounded by MaxCmds)
    crashes,  \* crashes so far (bounded by MaxCrashes)
    lastOk    \* the version of the last command that succeeded

vars == <<val, lock, pc, base, target, todo, done, journal, cmds, crashes, lastOk>>

Phases == {"idle", "waiting", "recover", "prepare", "commit", "compensate"}

TypeOK ==
    /\ val \in [Res -> Nat]
    /\ lock \in Procs \cup {None}
    /\ pc \in [Procs -> Phases]
    /\ journal = None \/ journal \in [base: Nat, intents: Seq(Res)]

Init ==
    /\ val = [r \in Res |-> 0]
    /\ lock = None
    /\ pc = [p \in Procs |-> "idle"]
    /\ base = [p \in Procs |-> 0]
    /\ target = [p \in Procs |-> 0]
    /\ todo = [p \in Procs |-> <<>>]
    /\ done = [p \in Procs |-> <<>>]
    /\ journal = None
    /\ cmds = 0
    /\ crashes = 0
    /\ lastOk = 0

\* Reading the state to plan from before taking the lock (PlanUnderLock = FALSE).
ReadEarly(p) ==
    /\ ~PlanUnderLock
    /\ pc[p] = "idle" /\ cmds < MaxCmds
    /\ base' = [base EXCEPT ![p] = val[Plan[1]]]
    /\ pc' = [pc EXCEPT ![p] = "waiting"]
    /\ UNCHANGED <<val, lock, target, todo, done, journal, cmds, crashes, lastOk>>

Acquire(p) ==
    /\ lock = None
    /\ \/ PlanUnderLock /\ pc[p] = "idle" /\ cmds < MaxCmds
       \/ pc[p] = "waiting"
    /\ lock' = p
    /\ cmds' = cmds + 1
    /\ target' = [target EXCEPT ![p] = cmds + 1]
    /\ pc' = [pc EXCEPT ![p] = IF journal # None THEN "recover" ELSE "prepare"]
    /\ UNCHANGED <<val, base, todo, done, journal, crashes, lastOk>>

\* Recovery undoes the recorded intents in reverse, one durable step at a time.
Recover(p) ==
    /\ pc[p] = "recover" /\ journal.intents # <<>>
    /\ val' = [val EXCEPT ![Last(journal.intents)] = journal.base]
    /\ journal' = [journal EXCEPT !.intents = Front(journal.intents)]
    /\ UNCHANGED <<lock, pc, base, target, todo, done, cmds, crashes, lastOk>>

RecoverDone(p) ==
    /\ pc[p] = "recover" /\ journal.intents = <<>>
    /\ journal' = None
    /\ pc' = [pc EXCEPT ![p] = "prepare"]
    /\ UNCHANGED <<val, lock, base, target, todo, done, cmds, crashes, lastOk>>

\* Prepare checks and builds; it changes nothing observable.
Prepare(p) ==
    /\ pc[p] = "prepare"
    /\ base' = IF PlanUnderLock THEN [base EXCEPT ![p] = val[Plan[1]]] ELSE base
    /\ todo' = [todo EXCEPT ![p] = [i \in 1..Len(Plan) |-> Plan[i]]]
    /\ done' = [done EXCEPT ![p] = <<>>]
    /\ journal' = IF Journaling THEN [base |-> base'[p], intents |-> <<>>] ELSE None
    /\ pc' = [pc EXCEPT ![p] = "commit"]
    /\ UNCHANGED <<val, lock, target, cmds, crashes, lastOk>>

PrepareFail(p) ==
    /\ pc[p] = "prepare"
    /\ lock' = None
    /\ pc' = [pc EXCEPT ![p] = "idle"]
    /\ UNCHANGED <<val, base, target, todo, done, journal, cmds, crashes, lastOk>>

\* With journaling, a step's intent is recorded before the step runs.
Intent(p) ==
    /\ Journaling
    /\ pc[p] = "commit" /\ todo[p] # <<>>
    /\ Len(journal.intents) = Len(done[p])
    /\ journal' = [journal EXCEPT !.intents = Append(@, Head(todo[p]))]
    /\ UNCHANGED <<val, lock, pc, base, target, todo, done, cmds, crashes, lastOk>>

\* (IF, not \/: TLC splits disjunctions in actions into separate branches.)
Ready(p) == IF Journaling THEN Len(journal.intents) = Len(done[p]) + 1 ELSE TRUE

Apply(p) ==
    /\ pc[p] = "commit" /\ todo[p] # <<>> /\ Ready(p)
    /\ val' = [val EXCEPT ![Head(todo[p])] = target[p]]
    /\ done' = [done EXCEPT ![p] = Append(@, Head(todo[p]))]
    /\ todo' = [todo EXCEPT ![p] = Tail(@)]
    /\ UNCHANGED <<lock, pc, base, target, journal, cmds, crashes, lastOk>>

\* A failed step changes nothing (compound effects are atomic in the shell).
ApplyFail(p) ==
    /\ pc[p] = "commit" /\ todo[p] # <<>> /\ Ready(p)
    /\ pc' = [pc EXCEPT ![p] = "compensate"]
    /\ UNCHANGED <<val, lock, base, target, todo, done, journal, cmds, crashes, lastOk>>

Compensate(p) ==
    /\ pc[p] = "compensate" /\ done[p] # <<>>
    /\ val' = [val EXCEPT ![Last(done[p])] = base[p]]
    /\ done' = [done EXCEPT ![p] = Front(@)]
    /\ UNCHANGED <<lock, pc, base, target, todo, journal, cmds, crashes, lastOk>>

CompensateDone(p) ==
    /\ pc[p] = "compensate" /\ done[p] = <<>>
    /\ lock' = None
    /\ journal' = None
    /\ pc' = [pc EXCEPT ![p] = "idle"]
    /\ UNCHANGED <<val, base, target, todo, done, cmds, crashes, lastOk>>

Finish(p) ==
    /\ pc[p] = "commit" /\ todo[p] = <<>>
    /\ lock' = None
    /\ journal' = None
    /\ lastOk' = target[p]
    /\ pc' = [pc EXCEPT ![p] = "idle"]
    /\ UNCHANGED <<val, base, target, todo, done, cmds, crashes>>

\* A crash loses the process's memory; the kernel releases its flock; the
\* journal survives.
Crash(p) ==
    /\ crashes < MaxCrashes /\ pc[p] # "idle"
    /\ crashes' = crashes + 1
    /\ lock' = IF lock = p THEN None ELSE lock
    /\ pc' = [pc EXCEPT ![p] = "idle"]
    /\ todo' = [todo EXCEPT ![p] = <<>>]
    /\ done' = [done EXCEPT ![p] = <<>>]
    /\ UNCHANGED <<val, base, target, journal, cmds, lastOk>>

\* Everything finished: let the behavior stutter instead of deadlocking.
Quiescent ==
    /\ cmds >= MaxCmds /\ \A p \in Procs : pc[p] = "idle"
    /\ UNCHANGED vars

Progress(p) ==
    \/ ReadEarly(p) \/ Acquire(p) \/ Recover(p) \/ RecoverDone(p) \/ Prepare(p)
    \/ Intent(p) \/ Apply(p) \/ Compensate(p) \/ CompensateDone(p) \/ Finish(p)

Next ==
    \/ \E p \in Procs : Progress(p) \/ PrepareFail(p) \/ ApplyFail(p) \/ Crash(p)
    \/ Quiescent

Spec == Init /\ [][Next]_vars /\ \A p \in Procs : WF_vars(Progress(p))

-----------------------------------------------------------------------------
\* P1 as other commands observe it.
Consistent == (lock = None /\ journal = None) => \A r \in Res : val[r] = lastOk

\* Every process eventually finishes (no saga or recovery gets stuck).
Termination == <>[](\A p \in Procs : pc[p] = "idle")
=============================================================================
