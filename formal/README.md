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
- **Proof**: [`internal/spec/refine/smt_test.go`](../internal/spec/refine/smt_test.go) generates SMT-LIB from the data `internal/tea/egress.Rules` and `spec.Shape` return and checks it with Z3:
  1. the rules' built-in "never" and "LAN" address sets equal the specification's, for all IPv4 and IPv6 addresses (bit-vectors);
  2. the rules' first-match structure equals the specification's `Reach` formula with free Boolean set memberships, which covers every allow/deny list, mode and default.
- A deliberately satisfiable control query guards against a pipeline that answers `unsat` to everything.

### 3. Command saga under locking and crashes (TLA+ / TLC)
- **Specification**: [`saga/Saga.tla`](./saga/Saga.tla): several antic processes run the command saga of `internal/tea/operation` on one guest under its lock; any process may crash (its flock is released, a journal survives).
- **Configurations**: `SagaJournal.cfg` (P1 `Consistent` and `Termination` hold), `SagaNoJournal.cfg` and `SagaStaleRead.cfg` (TLC must find a P1 counterexample: without a journal a crash leaves mixed resources; planning from state read before taking the lock undoes another command's success).
- **Runner**: [`internal/spec/refine/tla_test.go`](../internal/spec/refine/tla_test.go) runs all three and checks the verdicts.

Both run in the dev shell (which provides `z3` and `tlc`):

```bash
nix develop -c go test ./internal/spec/refine
```
