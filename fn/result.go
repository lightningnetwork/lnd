package fn

import (
	"fmt"
	"testing"
)

// Result represents a value that can either be a success (T) or an error.
type Result[T any] struct {
	Either[T, error]
}

// Ok creates a new Result with a success value.
func Ok[T any](val T) Result[T] {
	return Result[T]{Either: NewLeft[T, error](val)}
}

// Err creates a new Result with an error.
func Err[T any](err error) Result[T] {
	return Result[T]{Either: NewRight[T, error](err)}
}

// Errf creates a new Result with a new formatted error string.
func Errf[T any](errString string, args ...any) Result[T] {
	return Result[T]{
		Either: NewRight[T, error](fmt.Errorf(errString, args...)),
	}
}

// Unpack extracts the value or error from the Result.
func (r Result[T]) Unpack() (T, error) {
	var zero T
	return r.left.UnwrapOr(zero), r.right.UnwrapOr(nil)
}

// IsOk returns true if the Result is a success value.
func (r Result[T]) IsOk() bool {
	return r.IsLeft()
}

// IsErr returns true if the Result is an error.
func (r Result[T]) IsErr() bool {
	return r.IsRight()
}

// Map applies a function to the success value if it exists.
func (r Result[T]) Map(f func(T) T) Result[T] {
	if r.IsOk() {
		return Ok(f(r.left.some))
	}

	return r
}

// MapErr applies a function to the error value if it exists.
func (r Result[T]) MapErr(f func(error) error) Result[T] {
	if r.IsErr() {
		return Err[T](f(r.right.some))
	}

	return r
}

// Option returns the success value as an Option.
func (r Result[T]) Option() Option[T] {
	return r.left
}

// WhenResult executes the given function if the Result is a success.
func (r Result[T]) WhenResult(f func(T)) {
	r.left.WhenSome(func(t T) {
		f(t)
	})
}

// WhenErr executes the given function if the Result is an error.
func (r Result[T]) WhenErr(f func(error)) {
	r.right.WhenSome(func(e error) {
		f(e)
	})
}

// UnwrapOr returns the success value or a default value if it's an error.
func (r Result[T]) UnwrapOr(defaultValue T) T {
	return r.left.UnwrapOr(defaultValue)
}

// UnwrapOrElse returns the success value or computes a value from a function
// if it's an error.
func (r Result[T]) UnwrapOrElse(f func() T) T {
	return r.left.UnwrapOrFunc(f)
}

// UnwrapOrFail returns the success value or fails the test if it's an error.
func (r Result[T]) UnwrapOrFail(t *testing.T) T {
	t.Helper()

	return r.left.UnwrapOrFail(t)
}

// FlatMap applies a function that returns a Result to the success value if it
// exists.
func (r Result[T]) FlatMap(f func(T) Result[T]) Result[T] {
	if r.IsOk() {
		return f(r.left.some)
	}
	return r
}

// AndThen is an alias for FlatMap. This along with OrElse can be used to
// Railway Oriented Programming (ROP) by chaining successive computational
// operations from a single result type.
func (r Result[T]) AndThen(f func(T) Result[T]) Result[T] {
	return r.FlatMap(f)
}

// OrElse returns the original Result if it is a success, otherwise it returns
// the provided alternative Result. This along with AndThen can be used to
// Railway Oriented Programming (ROP).
func (r Result[T]) OrElse(f func() Result[T]) Result[T] {
	if r.IsOk() {
		return r
	}

	return f()
}

// FlatMap applies a function that returns a Result[B] to the success value if
// it exists.
func FlatMap[A, B any](r Result[A], f func(A) Result[B]) Result[B] {
	if r.IsOk() {
		return f(r.left.some)
	}

	return Err[B](r.right.some)
}

// AndThen is an alias for FlatMap. This along with OrElse can be used to
// Railway Oriented Programming (ROP).
func AndThen[A, B any](r Result[A], f func(A) Result[B]) Result[B] {
	return FlatMap(r, f)
}
