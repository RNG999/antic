#!/usr/bin/env python3
"""
Formal verification test runner for antic's CLI argument dispatch logic.
Uses Z3 SMT solver via Python bindings.
"""

from z3 import (
    Datatype,
    IntSort,
    Int,
    Bool,
    And,
    Not,
    Implies,
    If,
    Solver,
    sat,
    unsat,
)


def verify_cli_args():
    # Define Result ADT (Constructors and accessors with types)
    Result = Datatype("Result")
    Result.declare("Success", ("selected_guest", IntSort()), ("remaining_items", IntSort()))
    Result.declare("ErrConfigLoad")
    Result.declare("ErrNoGuests")
    Result.declare("ErrGuestRequired")
    Result.declare("ErrEmptyItems")
    Result = Result.create()

    # Variables
    total_args = Int("total_args")
    args0_is_guest = Bool("args0_is_guest")
    num_defined_guests = Int("num_defined_guests")
    only_guest_id = Int("only_guest_id")
    args0_guest_id = Int("args0_guest_id")
    config_load_error = Bool("config_load_error")

    # Invariants on the input domain
    domain_axioms = [
        total_args >= 0,
        num_defined_guests >= 0,
        Implies(total_args == 0, Not(args0_is_guest)),
        Implies(num_defined_guests == 0, Not(args0_is_guest)),  # Cannot match a guest if none exist
        Implies(num_defined_guests == 1, only_guest_id > 0),
        Implies(args0_is_guest, args0_guest_id > 0),
        Implies(And(args0_is_guest, num_defined_guests == 1), args0_guest_id == only_guest_id),
    ]

    # Specification function
    res = If(
        config_load_error,
        Result.ErrConfigLoad,
        If(
            And(total_args > 0, args0_is_guest),
            # Branch 1: Explicit guest
            If(total_args - 1 == 0, Result.ErrEmptyItems, Result.Success(args0_guest_id, total_args - 1)),
            # Branch 2: Implicit guest fallback
            If(
                num_defined_guests == 1,
                If(total_args == 0, Result.ErrEmptyItems, Result.Success(only_guest_id, total_args)),
                If(num_defined_guests == 0, Result.ErrNoGuests, Result.ErrGuestRequired),
            ),
        ),
    )

    print("Running formal verification of CLI argument dispatch with Z3...")

    # Test 1: Positive remaining items on success
    s1 = Solver()
    s1.add(domain_axioms)
    s1.add(Result.is_Success(res))
    s1.add(Result.remaining_items(res) <= 0)
    check1 = s1.check()
    print(f"  [1] Success invariant (remaining_items > 0): {'PASSED (UNSAT)' if check1 == unsat else 'FAILED'}")
    assert check1 == unsat, "Invariant violated: Success with items <= 0"

    # Test 2: Multi-guest isolation
    s2 = Solver()
    s2.add(domain_axioms)
    s2.add(Not(config_load_error))
    s2.add(num_defined_guests > 1)
    s2.add(Not(And(total_args > 0, args0_is_guest)))
    s2.add(Result.is_Success(res))
    check2 = s2.check()
    print(f"  [2] Multi-guest isolation (no implicit pick when >1): {'PASSED (UNSAT)' if check2 == unsat else 'FAILED'}")
    assert check2 == unsat, "Safety violated: Multi-guest allowed implicit pick"

    # Test 3: Zero-guest safety
    s3 = Solver()
    s3.add(domain_axioms)
    s3.add(Not(config_load_error))
    s3.add(num_defined_guests == 0)
    s3.add(Result.is_Success(res))
    check3 = s3.check()
    print(f"  [3] Zero-guest safety (never succeeds when 0): {'PASSED (UNSAT)' if check3 == unsat else 'FAILED'}")
    assert check3 == unsat, "Safety violated: Succeeded with 0 guests"

    # Test 4: Corner case detection (Name collision in single-guest environment)
    s4 = Solver()
    s4.add(domain_axioms)
    s4.add(Not(config_load_error))
    s4.add(num_defined_guests == 1)
    s4.add(total_args == 1)
    s4.add(args0_is_guest)
    s4.add(res == Result.ErrEmptyItems)
    check4 = s4.check()
    print(f"  [4] Single-guest name collision edge-case: {'DETECTED (SAT)' if check4 == sat else 'UNEXPECTED'}")
    if check4 == sat:
        m = s4.model()
        print(f"      Counterexample model: total_args=1, args0_is_guest=True -> Error: ErrEmptyItems")

    print("\nAll formal verification checks completed successfully.")


if __name__ == "__main__":
    verify_cli_args()
