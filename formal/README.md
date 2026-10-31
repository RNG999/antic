# Formal Verification Suite for antic

This directory contains formal specifications and automated verification tests for `antic` components using the **Z3 SMT Solver**.

## Components Verified

### 1. CLI Argument Dispatch Logic (`resolveGuestAndItems`)
- **Specification**: [`cli_args.smt2`](./cli_args.smt2) (SMT-LIB 2.0 format)
- **Automated Runner**: [`test_cli_args.py`](./test_cli_args.py) (Python Z3 API)
- **Properties Proven**:
  1. **Strict Success Invariant**: `remaining_items > 0` is guaranteed on `Success`. No zero-item operation can succeed.
  2. **Multi-guest Isolation Invariant**: When `num_guests > 1`, implicit guest selection is strictly impossible without specifying `args[0]`.
  3. **Zero-guest Safety**: When `num_guests == 0`, operations always fail gracefully without selecting undefined guests.
  4. **Edge-case Discovery**: Detects argument ambiguity when a package/item name collides with a single defined guest's name.

## Running the Verification

Run inside the `formal` guest environment:

```bash
antic run formal -- python3 formal/test_cli_args.py
```

Or execute the SMT-LIB file directly:

```bash
antic run formal -- z3 formal/cli_args.smt2
```

### 2. Egress rules equal the specification (Z3)
- **Proof**: [`internal/spec/refine/egress_smt_test.go`](../internal/spec/refine/egress_smt_test.go) generates SMT-LIB from the data `internal/tea/egress.Rules` and `spec.Shape` return and checks it with Z3:
  1. the rules' built-in "never" and "LAN" address sets equal the specification's, for all IPv4 and IPv6 addresses (bit-vectors);
  2. the rules' first-match structure equals the specification's `Reach` formula with free Boolean set memberships, which covers every allow/deny list, mode and default.
- A deliberately satisfiable control query guards against a pipeline that answers `unsat` to everything.
- **The generator is checked too**: [`egress_smt_gen_test.go`](../internal/spec/refine/egress_smt_gen_test.go) evaluates the generated formulas on concrete inputs and compares them with the Go code they stand for (`smtIn` with `netip.Prefix.Contains` at and around every prefix boundary, `rulesFormula` with `egress.Eval`, `reachFormula` with `spec.Reach`). A generator bug that both sides of the proof share (an off-by-one in a prefix's bit range) is invisible to the equality proof but caught here.

### 3. Command saga under locking and crashes (TLA+ / TLC)
- **Specification**: [`saga/Saga.tla`](./saga/Saga.tla): several antic processes run the command saga of `internal/tea/operation` on one guest under its lock; any process may crash (its flock is released, the journal survives). Each action is one transition of the Go core (one effect and its result), including the failures of journal writes.
- **Configurations**: `SagaJournal.cfg` (P1 `Consistent` and `Termination` hold; the replay's bounds), `SagaTrace.cfg` (the same with plans of up to 5 steps, for trace validation), `SagaNoJournal.cfg` and `SagaStaleRead.cfg` (TLC must find a P1 counterexample: without a journal a crash leaves mixed resources; planning from state read before taking the lock undoes another command's success).
- **Runner**: [`internal/spec/refine/saga_tla_test.go`](../internal/spec/refine/saga_tla_test.go) runs all three and checks the verdicts.
- **Refinement of the Go core (TLC state-graph replay)**: [`internal/spec/refine/saga_refine_test.go`](../internal/spec/refine/saga_refine_test.go) dumps the complete state graph of `SagaJournal.cfg` and runs `operation.Update` for the same processes, commands and crashes. Through one abstraction function (`alpha`) and one action-mapping table (`action`: Go stage and outcome → TLA+ action), every Go transition must be a stutter or a TLC edge with that action, and every TLC state and edge must be reached: the core and the model are equivalent up to stuttering on this instance. Mutants of the Go side (no journal, no intents, wrong undo, short recovery, stale read, early success) must each be detected. The harness's bounds are checked against the cfg.
- **Plans of any length**: each command chooses its plan length (0..NRes commit steps) when it plans; `base` and `lastOk` are per resource. The replay harness runs one real command shape per length (start, stop, mount, a config and package change).
- **Trace validation of the shell**: antic appends NDJSON saga events to `$ANTIC_SAGA_TRACE` (lock taken and released, plan length, each effect's stage and outcome). [`internal/spec/refine/saga_trace_test.go`](../internal/spec/refine/saga_trace_test.go) maps them through the same action table and replays them on the state graph of `SagaTrace.cfg` (plans of up to 5 steps) as a nondeterministic automaton; a process that ends without releasing the lock is a `Crash`. The model is reset at quiescent points, so traces of any length are checked. `TestSagaTraceValidationRejectsWrongTraces` checks that the validator rejects wrong traces (planning without recovering, applying without an intent, ...).

  ```bash
  ANTIC_E2E_TRACES=$HOME/.cache/antic-e2e/traces ANTIC_E2E_BIN=... ./e2e.test   # writes the traces
  nix develop -c sh -c 'ANTIC_E2E_TRACES=$HOME/.cache/antic-e2e/traces go test ./internal/spec/refine -run TestShellTraces'
  ```
- **The Horn clauses cover the model**: [`saga_chc_edges_test.go`](../internal/spec/refine/saga_chc_edges_test.go) abstracts every edge of the `SagaJournal.cfg` graph to the lock holder and checks with Z3 that it is an instance of a clause rule with the same action (the clauses are built as data, `chcModel`, and rendered both as Horn clauses and as transition formulas), and that every rule is used. Spacer's proof carries over to the model only because the clauses allow at least what it does. The check is one step at a time, so a rule that reads a variable that is stale in its phase is not caught.
- **Unbounded proof (Z3 Spacer)**: [`internal/spec/refine/saga_chc_test.go`](../internal/spec/refine/saga_chc_test.go) encodes the journaling saga as Constrained Horn Clauses, one invariant predicate per phase of the lock holder. Spacer finds an inductive invariant, so `Consistent` holds for any number of commands, crashes and processes (a process without the lock holds no state, so the lock holder represents them all). The variant without a journal is unsatisfiable, as TLC also finds. Liveness (`Termination`) is checked by TLC only.

Both run in the dev shell (which provides `z3` and `tlc`):

```bash
nix develop -c go test ./internal/spec/refine
```
