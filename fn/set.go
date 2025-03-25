package fn

import (
	"maps"
	"slices"
)

// Set is a generic set using type params that supports the following
// operations: diff, union, intersection, and subset.
type Set[T comparable] map[T]struct{}

// NewSet returns a new set with the given elements.
func NewSet[T comparable](elems ...T) Set[T] {
	s := make(Set[T])
	for _, e := range elems {
		s.Add(e)
	}
	return s
}

// Add adds an element to the set.
func (s Set[T]) Add(e T) {
	s[e] = struct{}{}
}

// Remove removes an element from the set.
func (s Set[T]) Remove(e T) {
	delete(s, e)
}

// Contains returns true if the set contains the element.
func (s Set[T]) Contains(e T) bool {
	_, ok := s[e]
	return ok
}

// IsEmpty returns true if the set is empty.
func (s Set[T]) IsEmpty() bool {
	return len(s) == 0
}

// Size returns the number of elements in the set.
func (s Set[T]) Size() uint {
	return uint(len(s))
}

// Diff returns the difference between two sets.
func (s Set[T]) Diff(other Set[T]) Set[T] {
	diff := make(Set[T])
	for e := range s {
		if !other.Contains(e) {
			diff.Add(e)
		}
	}
	return diff
}

// Union returns the union of two sets.
func (s Set[T]) Union(other Set[T]) Set[T] {
	union := make(Set[T])
	for e := range s {
		union.Add(e)
	}
	for e := range other {
		union.Add(e)
	}
	return union
}

// Intersect returns the intersection of two sets.
func (s Set[T]) Intersect(other Set[T]) Set[T] {
	intersect := make(Set[T])
	for e := range s {
		if other.Contains(e) {
			intersect.Add(e)
		}
	}
	return intersect
}

// Subset returns true if the set is a subset of the other set.
func (s Set[T]) Subset(other Set[T]) bool {
	for e := range s {
		if !other.Contains(e) {
			return false
		}
	}
	return true
}

// Equal returns true if the set is equal to the other set.
func (s Set[T]) Equal(other Set[T]) bool {
	return s.Subset(other) && other.Subset(s)
}

// ToSlice returns the set as a slice.
func (s Set[T]) ToSlice() []T {
	return slices.Collect(maps.Keys(s))
}

// Copy copies s and returns the result.
func (s Set[T]) Copy() Set[T] {
	copy := make(Set[T])
	for e := range s {
		copy.Add(e)
	}
	return copy
}

// SetDiff returns all the items that are in the first set but not in the
// second.
func SetDiff[T comparable](a, b []T) []T {
	setA := NewSet(a...)
	setB := NewSet(b...)

	return setA.Diff(setB).ToSlice()
}
