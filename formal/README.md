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
