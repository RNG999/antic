// Package types provides pure, domain-independent type combinators for antic:
//
//  1. adt.go:        Algebraic Data Types (Option[T], NonEmpty[T])
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
