package fn

// Either is a type that can be either left or right.
type Either[L any, R any] struct {
	isRight bool
	left    L
	right   R
}

// NewLeft returns an Either with a left value.
func NewLeft[L any, R any](l L) Either[L, R] {
	return Either[L, R]{left: l}
}

// NewRight returns an Either with a right value.
func NewRight[L any, R any](r R) Either[L, R] {
	return Either[L, R]{isRight: true, right: r}
}

// ElimEither is the universal Either eliminator. It can be used to safely
// handle all possible values inside the Either by supplying two continuations,
// one for each side of the Either.
func ElimEither[L, R, O any](e Either[L, R], f func(L) O, g func(R) O) O {
	if !e.isRight {
		return f(e.left)
	}

	return g(e.right)
}

// WhenLeft executes the given function if the Either is left.
func (e Either[L, R]) WhenLeft(f func(L)) {
	if !e.isRight {
		f(e.left)
	}
}

// WhenRight executes the given function if the Either is right.
func (e Either[L, R]) WhenRight(f func(R)) {
	if e.isRight {
		f(e.right)
	}
}

// IsLeft returns true if the Either is left.
func (e Either[L, R]) IsLeft() bool {
	return !e.isRight
}

// IsRight returns true if the Either is right.
func (e Either[L, R]) IsRight() bool {
	return e.isRight
}

// LeftToSome converts a Left value to an Option, returning None if the inner
// Either value is a Right value.
func (e Either[L, R]) LeftToSome() Option[L] {
	if e.isRight {
		return None[L]()
	}

	return Some(e.left)
}

// RightToSome converts a Right value to an Option, returning None if the
// inner Either value is a Left value.
func (e Either[L, R]) RightToSome() Option[R] {
	if !e.isRight {
		return None[R]()
	}

	return Some(e.right)
}

// UnwrapLeftOr will extract the Left value from the Either if it is present
// returning the supplied default if it is not.
func (e Either[L, R]) UnwrapLeftOr(l L) L {
	if e.isRight {
		return l
	}

	return e.left
}

// UnwrapRightOr will extract the Right value from the Either if it is present
// returning the supplied default if it is not.
func (e Either[L, R]) UnwrapRightOr(r R) R {
	if !e.isRight {
		return r
	}

	return e.right
}

// Swap reverses the type argument order. This can be useful as an adapter
// between APIs.
func (e Either[L, R]) Swap() Either[R, L] {
	return Either[R, L]{
		isRight: !e.isRight,
		left:    e.right,
		right:   e.left,
	}
}

// MapLeft maps the left value of the Either to a new value.
func MapLeft[L, R, O any](f func(L) O) func(Either[L, R]) Either[O, R] {
	return func(e Either[L, R]) Either[O, R] {
		if !e.isRight {
			return Either[O, R]{
				isRight: false,
				left:    f(e.left),
			}
		}

		return Either[O, R]{
			isRight: true,
			right:   e.right,
		}
	}
}

// MapRight maps the right value of the Either to a new value.
func MapRight[L, R, O any](f func(R) O) func(Either[L, R]) Either[L, O] {
	return func(e Either[L, R]) Either[L, O] {
		if e.isRight {
			return Either[L, O]{
				isRight: true,
				right:   f(e.right),
			}
		}

		return Either[L, O]{
			isRight: false,
			left:    e.left,
		}
	}
}
