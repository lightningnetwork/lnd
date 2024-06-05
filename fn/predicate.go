package fn

// Pred[A] is a type alias for a predicate operating over type A.
type Pred[A any] func(A) bool

// PredAnd is a lifted version of the && operation that operates over functions
// producing a boolean value from some type A.
func PredAnd[A any](p0 Pred[A], p1 Pred[A]) Pred[A] {
	return func(a A) bool {
		return p0(a) && p1(a)
	}
}

// PredOr is a lifted version of the || operation that operates over functions
// producing a boolean value from some type A.
func PredOr[A any](p0 Pred[A], p1 Pred[A]) Pred[A] {
	return func(a A) bool {
		return p0(a) || p1(a)
	}
}
