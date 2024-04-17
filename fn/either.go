package fn

// Either is a type that can be either left or right.
type Either[L any, R any] struct {
	left  Option[L]
	right Option[R]
}

// NewLeft returns an Either with a left value.
func NewLeft[L any, R any](l L) Either[L, R] {
	return Either[L, R]{left: Some(l), right: None[R]()}
}

// NewRight returns an Either with a right value.
func NewRight[L any, R any](r R) Either[L, R] {
	return Either[L, R]{left: None[L](), right: Some(r)}
}

// ElimEither is the universal Either eliminator. It can be used to safely
// handle all possible values inside the Either by supplying two continuations,
// one for each side of the Either.
func ElimEither[L, R, O any](f func(L) O, g func(R) O, e Either[L, R]) O {
	if e.left.IsSome() {
		return f(e.left.some)
	}

	return g(e.right.some)
}

// WhenLeft executes the given function if the Either is left.
func (e Either[L, R]) WhenLeft(f func(L)) {
	e.left.WhenSome(f)
}

// WhenRight executes the given function if the Either is right.
func (e Either[L, R]) WhenRight(f func(R)) {
	e.right.WhenSome(f)
}

// IsLeft returns true if the Either is left.
func (e Either[L, R]) IsLeft() bool {
	return e.left.IsSome()
}

// IsRight returns true if the Either is right.
func (e Either[L, R]) IsRight() bool {
	return e.right.IsSome()
}

// LeftToOption converts a Left value to an Option, returning None if the inner
// Either value is a Right value.
func (e Either[L, R]) LeftToOption() Option[L] {
	return e.left
}

// RightToOption converts a Right value to an Option, returning None if the
// inner Either value is a Left value.
func (e Either[L, R]) RightToOption() Option[R] {
	return e.right
}

// UnwrapLeftOr will extract the Left value from the Either if it is present
// returning the supplied default if it is not.
func (e Either[L, R]) UnwrapLeftOr(l L) L {
	return e.left.UnwrapOr(l)
}

// UnwrapRightOr will extract the Right value from the Either if it is present
// returning the supplied default if it is not.
func (e Either[L, R]) UnwrapRightOr(r R) R {
	return e.right.UnwrapOr(r)
}

// Swap reverses the type argument order. This can be useful as an adapter
// between APIs.
func (e Either[L, R]) Swap() Either[R, L] {
	return Either[R, L]{
		left:  e.right,
		right: e.left,
	}
}

// MapLeft maps the left value of the Either to a new value.
func MapLeft[L, R, O any](f func(L) O) func(Either[L, R]) Either[O, R] {
	return func(e Either[L, R]) Either[O, R] {
		if e.IsLeft() {
			return Either[O, R]{
				left:  MapOption(f)(e.left),
				right: None[R](),
			}
		}

		return Either[O, R]{
			left:  None[O](),
			right: e.right,
		}
	}
}

// MapRight maps the right value of the Either to a new value.
func MapRight[L, R, O any](f func(R) O) func(Either[L, R]) Either[L, O] {
	return func(e Either[L, R]) Either[L, O] {
		if e.IsRight() {
			return Either[L, O]{
				left:  None[L](),
				right: MapOption(f)(e.right),
			}
		}

		return Either[L, O]{
			left:  e.left,
			right: None[O](),
		}
	}
}
