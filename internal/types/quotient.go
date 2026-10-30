package types

import "slices"

// KeyExtractor defines a total order key extraction and strict order relation
// for elements of a canonical SortedSet[T, K, Ord].
type KeyExtractor[T any, K comparable] interface {
	Key(elem T) K
	Less(a, b K) bool
}

// SortedSet is a Quotient Container (Hofmann 1995) representing a finite set/map
// whose elements are guaranteed by construction to be strictly sorted by their
// unique key K with no duplicate keys.
//
// Because two sets are extensionally equal if and only if their underlying
// canonical slices are element-wise identical, SortedSet eliminates state-space
// explosion and nondeterministic iteration in formal model checking.
type SortedSet[T any, K comparable, Ord KeyExtractor[T, K]] struct {
	items []T
}

// EmptySet returns an empty canonical SortedSet.
func EmptySet[T any, K comparable, Ord KeyExtractor[T, K]]() SortedSet[T, K, Ord] {
	return SortedSet[T, K, Ord]{}
}

// BuildSet normalizes an arbitrary slice into a canonical SortedSet (sorted by
// Ord.Less, with later elements overwriting earlier elements of the same key).
func BuildSet[T any, K comparable, Ord KeyExtractor[T, K]](raw []T) SortedSet[T, K, Ord] {
	s := EmptySet[T, K, Ord]()
	for _, item := range raw {
		s = s.Upsert(item)
	}
	return s
}

// Len returns the number of elements in the set.
func (s SortedSet[T, K, Ord]) Len() int { return len(s.items) }

// All returns a read-only clone of the canonically ordered elements.
func (s SortedSet[T, K, Ord]) All() []T {
	if len(s.items) == 0 {
		return nil
	}
	return slices.Clone(s.items)
}

// Clone returns a copy of the SortedSet with a cloned backing slice.
func (s SortedSet[T, K, Ord]) Clone() SortedSet[T, K, Ord] {
	if len(s.items) == 0 {
		return SortedSet[T, K, Ord]{}
	}
	return SortedSet[T, K, Ord]{items: slices.Clone(s.items)}
}

// Lookup finds the element with the given key K.
func (s SortedSet[T, K, Ord]) Lookup(key K) (T, bool) {
	var ord Ord
	for _, item := range s.items {
		if ord.Key(item) == key {
			return item, true
		}
	}
	var zero T
	return zero, false
}

// Contains reports whether an element with key K exists in the set.
func (s SortedSet[T, K, Ord]) Contains(key K) bool {
	_, ok := s.Lookup(key)
	return ok
}

// Upsert inserts elem or replaces the existing element with the same key,
// preserving strict canonical key ordering.
func (s SortedSet[T, K, Ord]) Upsert(elem T) SortedSet[T, K, Ord] {
	var ord Ord
	k := ord.Key(elem)
	out := make([]T, 0, len(s.items)+1)
	inserted := false
	for _, existing := range s.items {
		ek := ord.Key(existing)
		if !inserted {
			if ek == k {
				out = append(out, elem)
				inserted = true
				continue
			}
			if ord.Less(k, ek) {
				out = append(out, elem)
				inserted = true
			}
		}
		out = append(out, existing)
	}
	if !inserted {
		out = append(out, elem)
	}
	return SortedSet[T, K, Ord]{items: out}
}

// Remove deletes the element with key K, if present.
func (s SortedSet[T, K, Ord]) Remove(key K) SortedSet[T, K, Ord] {
	var ord Ord
	for i, existing := range s.items {
		if ord.Key(existing) == key {
			out := slices.Clone(s.items)
			out = slices.Delete(out, i, i+1)
			return SortedSet[T, K, Ord]{items: out}
		}
	}
	return s.Clone()
}

// Canonical reports whether s satisfies the strict canonical ordering invariant
// (always true for sets built through EmptySet/BuildSet/Upsert/Remove).
func (s SortedSet[T, K, Ord]) Canonical() bool {
	var ord Ord
	for i := range s.items {
		if i > 0 {
			prevK := ord.Key(s.items[i-1])
			currK := ord.Key(s.items[i])
			if !ord.Less(prevK, currK) {
				return false
			}
		}
	}
	return true
}
