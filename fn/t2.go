package fn

// T2 is the simplest 2-tuple type. It is useful for capturing ad hoc
// type conjunctions in a single value that can be easily dot-chained.
type T2[A, B any] struct {
	first  A
	second B
}

// NewT2 is the canonical constructor for a T2. We include it because the fields
// themselves are unexported.
func NewT2[A, B any](a A, b B) T2[A, B] {
	return T2[A, B]{
		first:  a,
		second: b,
	}
}

// First returns the first value in the T2.
func (t2 T2[A, B]) First() A {
	return t2.first
}

// Second returns the second value in the T2.
func (t2 T2[A, B]) Second() B {
	return t2.second
}

// Unpack ejects the 2-tuple's members into the multiple return values that
// are customary in go idiom.
func (t2 T2[A, B]) Unpack() (A, B) {
	return t2.first, t2.second
}

// Pair takes two functions that share the same argument type and runs them
// both and produces a 2-tuple of the results.
func Pair[A, B, C any](f func(A) B, g func(A) C) func(A) T2[B, C] {
	return func(a A) T2[B, C] {
		return NewT2[B, C](f(a), g(a))
	}
}

// MapFirst lifts the argument function into one that applies to the first
// element of a 2-tuple.
func MapFirst[A, B, C any](f func(A) B) func(T2[A, C]) T2[B, C] {
	return func(t2 T2[A, C]) T2[B, C] {
		return NewT2[B, C](f(t2.First()), t2.Second())
	}
}

// MapSecond lifts the argument function into one that applies to the second
// element of a 2-tuple.
func MapSecond[A, B, C any](f func(A) B) func(T2[C, A]) T2[C, B] {
	return func(t2 T2[C, A]) T2[C, B] {
		return NewT2[C, B](t2.First(), f(t2.Second()))
	}
}
