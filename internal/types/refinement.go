package types

import "fmt"

// Predicate defines a decidable refinement predicate P over base type T
// (Freeman & Pfenning 1991; Liquid Types, Rondon et al. 2008).
type Predicate[T any] interface {
	Check(v T) error
}

// Refined represents the refinement type { v : T | P.Check(v) == nil }.
// The type parameter P acts as a phantom witness of the proven invariant.
// Because fields are unexported, Refined values can only be constructed by
// passing through Parse (which runs P.Check).
type Refined[T comparable, P Predicate[T]] struct {
	value T
	valid bool
}

// Parse validates raw against predicate P and returns a Refined[T, P].
func Parse[T comparable, P Predicate[T]](raw T) (Refined[T, P], error) {
	var pred P
	if err := pred.Check(raw); err != nil {
		var zero Refined[T, P]
		return zero, err
	}
	return Refined[T, P]{value: raw, valid: true}, nil
}

// Get returns the underlying validated value.
func (r Refined[T, P]) Get() T { return r.value }

// Valid reports whether r was constructed via Parse and satisfies predicate P.
func (r Refined[T, P]) Valid() bool {
	if !r.valid {
		return false
	}
	var pred P
	return pred.Check(r.value) == nil
}

func (r Refined[T, P]) String() string { return fmt.Sprint(r.value) }
