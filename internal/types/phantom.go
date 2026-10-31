package types

import "fmt"

// Phantom wraps a value of representation type T with a compile-time phantom
// type parameter Tag (Leijen & Meijer 1999) so values from distinct semantic
// domains cannot be mixed up even when they share the same underlying type.
type Phantom[T comparable, Tag any] struct {
	value T
}

// NewPhantom wraps v in a Phantom[T, Tag].
func NewPhantom[T comparable, Tag any](v T) Phantom[T, Tag] {
	return Phantom[T, Tag]{value: v}
}

// Get returns the underlying value.
func (p Phantom[T, Tag]) Get() T { return p.value }

func (p Phantom[T, Tag]) String() string { return fmt.Sprint(p.value) }

// Indexed wraps a value T with a compile-time Typestate index State
// (Strom & Yemini 1986; Aldrich et al. 2009), encoding protocol or lifecycle
// state transitions at the type level: Indexed[T, S1] -> Indexed[T, S2].
type Indexed[T comparable, State any] struct {
	value T
}

// NewIndexed constructs a typestate-indexed value in state State.
func NewIndexed[T comparable, State any](v T) Indexed[T, State] {
	return Indexed[T, State]{value: v}
}

// TransitionIndexed transitions an Indexed value from typestate From to To.
func TransitionIndexed[T comparable, From, To any](x Indexed[T, From]) Indexed[T, To] {
	return Indexed[T, To](x) // same representation; only the typestate changes
}

// Get returns the underlying value.
func (ix Indexed[T, State]) Get() T { return ix.value }

func (ix Indexed[T, State]) String() string { return fmt.Sprint(ix.value) }
