package fn

// ResultOpt represents an operation that may either fail (with an error) or
// succeed with an optional final value.
type ResultOpt[T any] struct {
	Result[Option[T]]
}

// OkOpt constructs a successful ResultOpt with a present value.
func OkOpt[T any](val T) ResultOpt[T] {
	return ResultOpt[T]{Ok(Some(val))}
}

// NoneOpt constructs a successful ResultOpt with no final value.
func NoneOpt[T any]() ResultOpt[T] {
	return ResultOpt[T]{Ok(None[T]())}
}

// ErrOpt constructs a failed ResultOpt with the provided error.
func ErrOpt[T any](err error) ResultOpt[T] {
	return ResultOpt[T]{Err[Option[T]](err)}
}

// MapResultOpt applies a function to the final value of a successful operation.
func MapResultOpt[T, U any](ro ResultOpt[T], f func(T) U) ResultOpt[U] {
	if ro.IsErr() {
		return ErrOpt[U](ro.Err())
	}
	opt, _ := ro.Unpack()
	return ResultOpt[U]{Ok(MapOption(f)(opt))}
}

// AndThenResultOpt applies a function to the final value of a successful
// operation.
func AndThenResultOpt[T, U any](ro ResultOpt[T],
	f func(T) ResultOpt[U]) ResultOpt[U] {

	if ro.IsErr() {
		return ErrOpt[U](ro.Err())
	}
	opt, _ := ro.Unpack()
	if opt.IsNone() {
		return NoneOpt[U]()
	}
	return f(opt.some)
}

// IsSome returns true if the operation succeeded and contains a final value.
func (ro ResultOpt[T]) IsSome() bool {
	if ro.IsErr() {
		return false
	}
	opt, _ := ro.Unpack()
	return opt.IsSome()
}

// IsNone returns true if the operation succeeded but no final value is present.
func (ro ResultOpt[T]) IsNone() bool {
	if ro.IsErr() {
		return false
	}
	opt, _ := ro.Unpack()
	return opt.IsNone()
}
