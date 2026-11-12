// Package types provides pure, domain-independent type combinators for antic:
//
//  1. adt.go:        Algebraic Data Types (Option[T], NonEmpty[T], Result[T])
//  2. quotient.go:   Quotient Containers (SortedSet[T, K, Ord], KeyExtractor[T, K])
//  3. phantom.go:    Phantom / Branded Types & Typestate Witnesses (Phantom[T, Tag], Indexed[T, State])
//  4. refinement.go: Refinement Types / 篩型 (Predicate[T], Refined[T, P])
//  5. gdp.go:        Ghosts of Departed Proofs (Proof[Prop], Axiom)
//
// Every struct in this package has unexported fields so external packages
// cannot forge instances via struct literals, and this package imports zero
// internal packages.
package types

import "slices"

// Result represents the algebraic sum type T + Error (Ok(T) | Err(error)).
// It guarantees that either a valid value T exists OR an error exists,
// making invalid product-type combinations (nil, nil) or (val, err) unrepresentable.
type Result[T any] struct {
	val T
	err error
}

// Ok constructs a successful Result containing v.
func Ok[T any](v T) Result[T] {
	return Result[T]{val: v}
}

// Err constructs a failed Result containing err.
func Err[T any](err error) Result[T] {
	return Result[T]{err: err}
}

// From lifts a standard Go (T, error) tuple into an algebraic Result[T].
func From[T any](v T, err error) Result[T] {
	if err != nil {
		return Result[T]{err: err}
	}
	return Result[T]{val: v}
}

// IsOk reports whether the Result contains a value.
func (r Result[T]) IsOk() bool { return r.err == nil }

// IsErr reports whether the Result contains an error.
func (r Result[T]) IsErr() bool { return r.err != nil }

// Value returns the inner value if Ok, or the zero value of T if Err.
func (r Result[T]) Value() T { return r.val }

// ValueOr returns the inner value if Ok, or fallback if Err.
func (r Result[T]) ValueOr(fallback T) T {
	if r.err != nil {
		return fallback
	}
	return r.val
}

// Err returns the inner error if Err, or nil if Ok.
func (r Result[T]) Err() error { return r.err }

// Unwrap returns the value and error tuple (T, error) for standard Go interop.
func (r Result[T]) Unwrap() (T, error) { return r.val, r.err }

// Map transforms the inner value with fn if Ok, preserving Err.
func Map[T, U any](r Result[T], fn func(T) U) Result[U] {
	if r.err != nil {
		return Result[U]{err: r.err}
	}
	return Result[U]{val: fn(r.val)}
}

// AndThen (Monadic Bind / FlatMap) chains a computation that may fail,
// short-circuiting on the first error.
func AndThen[T, U any](r Result[T], fn func(T) Result[U]) Result[U] {
	if r.err != nil {
		return Result[U]{err: r.err}
	}
	return fn(r.val)
}

// Option represents the algebraic sum type 1 + T (None | Some(T)) using value
// semantics (no pointers), preserving structural equality in TEA models.
type Option[T comparable] struct {
	value   T
	present bool
}

// Some constructs an Option containing v.
func Some[T comparable](v T) Option[T] { return Option[T]{value: v, present: true} }

// None constructs an empty Option.
func None[T comparable]() Option[T] { return Option[T]{} }

// IsSome reports whether the Option contains a value.
func (o Option[T]) IsSome() bool { return o.present }

// Get returns the value and a boolean indicating whether it is present.
func (o Option[T]) Get() (T, bool) { return o.value, o.present }

// Value returns the underlying value (or the zero value of T if None).
func (o Option[T]) Value() T { return o.value }

// NonEmpty represents the inductive non-empty list { xs : []T | len(xs) >= 1 }
// decomposed into its guaranteed first element (head) and remaining elements (tail).
// Using NonEmpty[T] in Effect payloads makes empty-batch effects unrepresentable.
type NonEmpty[T any] struct {
	head T
	tail []T
}

// NewNonEmpty constructs a NonEmpty list from a mandatory head and optional tail.
func NewNonEmpty[T any](head T, tail ...T) NonEmpty[T] {
	return NonEmpty[T]{
		head: head,
		tail: slices.Clone(tail),
	}
}

// ParseNonEmpty constructs a NonEmpty[T] from a slice, returning false if empty.
func ParseNonEmpty[T any](xs []T) (NonEmpty[T], bool) {
	if len(xs) == 0 {
		return NonEmpty[T]{}, false
	}
	return NonEmpty[T]{
		head: xs[0],
		tail: slices.Clone(xs[1:]),
	}, true
}

// Head returns the guaranteed first element.
func (ne NonEmpty[T]) Head() T { return ne.head }

// Len returns the number of elements (always >= 1).
func (ne NonEmpty[T]) Len() int { return 1 + len(ne.tail) }

// Slice returns a freshly allocated slice containing all elements in order.
func (ne NonEmpty[T]) Slice() []T {
	out := make([]T, 0, 1+len(ne.tail))
	out = append(out, ne.head)
	out = append(out, ne.tail...)
	return out
}

// Clone returns a deep copy of the NonEmpty list.
func (ne NonEmpty[T]) Clone() NonEmpty[T] {
	return NonEmpty[T]{
		head: ne.head,
		tail: slices.Clone(ne.tail),
	}
}
