; SMT-LIB 2.0 Specification & Formal Verification of antic's CLI Argument Dispatch Logic
;
; Target: resolveGuestAndItems (internal/host/cli/cli.go)
; Purpose: Verify deterministic argument handling, absence of unhandled states,
;          and invariant adherence across varying guest counts and user inputs.

(set-logic ALL)

; Result algebraic data type corresponding to the function's return outcomes:
(declare-datatypes () ((Result 
    (Success (selected_guest Int) (remaining_items Int))
    (ErrConfigLoad)
    (ErrNoGuests)
    (ErrGuestRequired)
    (ErrEmptyItems)
)))

; State variables:
; - total_args: count of command-line arguments passed after the subcommand
; - args0_is_guest: whether args[0] exactly matches an existing guest name
; - num_defined_guests: count of guests defined in configuration
; - only_guest_id: integer identifier of the sole defined guest (valid when num_defined_guests == 1)
; - args0_guest_id: integer identifier of the guest matching args[0]
; - config_load_error: whether loading configuration failed (e.g. invalid syntax)

(declare-const total_args Int)
(declare-const args0_is_guest Bool)
(declare-const num_defined_guests Int)
(declare-const only_guest_id Int)
(declare-const args0_guest_id Int)
(declare-const config_load_error Bool)

; Domain axioms / physical invariants:
(assert (>= total_args 0))
(assert (>= num_defined_guests 0))
(assert (=> (= total_args 0) (not args0_is_guest)))
(assert (=> (= num_defined_guests 0) (not args0_is_guest))) ; A non-existent guest can never be matched
(assert (=> (= num_defined_guests 1) (> only_guest_id 0)))
(assert (=> args0_is_guest (> args0_guest_id 0)))
(assert (=> (and args0_is_guest (= num_defined_guests 1)) (= args0_guest_id only_guest_id)))

; Specification of resolveGuestAndItems:
(declare-const res Result)

(assert (= res
    (ite config_load_error
        ErrConfigLoad
        (ite (and (> total_args 0) args0_is_guest)
            ; Branch 1: Explicit guest supplied in args[0]
            (ite (= (- total_args 1) 0)
                ErrEmptyItems
                (Success args0_guest_id (- total_args 1))
            )
            ; Branch 2: Implicit guest fallback
            (ite (= num_defined_guests 1)
                (ite (= total_args 0)
                    ErrEmptyItems
                    (Success only_guest_id total_args)
                )
                (ite (= num_defined_guests 0)
                    ErrNoGuests
                    ErrGuestRequired
                )
            )
        )
    )
))

(echo "=== Property 1: Success invariant (remaining_items must be > 0) ===")
(push)
(assert (and ((_ is Success) res) (<= (remaining_items res) 0)))
(check-sat) ; Proves: remaining_items is strictly positive on Success
(pop)

(echo "=== Property 2: Multi-guest isolation invariant ===")
; When >1 guests exist and args[0] is not a guest, Success must never occur.
(push)
(assert (and 
    (not config_load_error)
    (> num_defined_guests 1)
    (not (and (> total_args 0) args0_is_guest))
    ((_ is Success) res)
))
(check-sat) ; Proves: No implicit selection when multiple guests exist
(pop)

(echo "=== Property 3: Zero-guest safety invariant ===")
; When 0 guests exist, Success must never occur.
(push)
(assert (and
    (not config_load_error)
    (= num_defined_guests 0)
    ((_ is Success) res)
))
(check-sat) ; Proves: Never succeeds without defined guests
(pop)

(echo "=== Property 4: Single-guest name collision ambiguity check ===")
; When 1 guest exists and the user specifies 1 argument whose name matches the guest,
; does it fail with ErrEmptyItems?
(push)
(assert (and
    (not config_load_error)
    (= num_defined_guests 1)
    (= total_args 1)
    args0_is_guest
    ((_ is ErrEmptyItems) res)
))
(check-sat) ; SAT indicates existence of counterexample / collision scenario
(get-model)
(pop)
