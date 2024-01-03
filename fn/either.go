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

// MapLeft maps the left value of the Either to a new value.
func MapLeft[L any, R any, O any](f func(L) O) func(Either[L, R]) Option[O] {
	return func(e Either[L, R]) Option[O] {
		if e.IsLeft() {
			return MapOption(f)(e.left)
		}

		return None[O]()
	}
}
