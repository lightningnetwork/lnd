package fn

import (
	"fmt"
	"testing"
)

// Result represents a value that can either be a success (T) or an error.
type Result[T any] struct {
	Either[T, error]
}

// NewResult creates a new result from a (value, error) tuple.
func NewResult[T any](val T, err error) Result[T] {
	if err != nil {
		return Err[T](err)
	}

	return Ok(val)
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

	if r.IsErr() {
		return zero, r.right
	}

	return r.left, nil
}

// Err exposes the underlying error of the result type as a normal error type.
func (r Result[T]) Err() error {
	return r.right
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
	return Result[T]{
		MapLeft[T, error](f)(r.Either),
	}
}

// MapErr applies a function to the error value if it exists.
func (r Result[T]) MapErr(f func(error) error) Result[T] {
	return Result[T]{
		MapRight[T](f)(r.Either),
	}
}

// Option returns the success value as an Option.
func (r Result[T]) Option() Option[T] {
	return r.Either.LeftToOption()
}

// WhenResult executes the given function if the Result is a success.
func (r Result[T]) WhenResult(f func(T)) {
	r.WhenLeft(f)
}

// WhenErr executes the given function if the Result is an error.
func (r Result[T]) WhenErr(f func(error)) {
	r.WhenRight(f)
}

// UnwrapOr returns the success value or a default value if it's an error.
func (r Result[T]) UnwrapOr(defaultValue T) T {
	if r.IsErr() {
		return defaultValue
	}

	return r.left
}

// UnwrapOrElse returns the success value or computes a value from a function
// if it's an error.
func (r Result[T]) UnwrapOrElse(f func() T) T {
	if r.IsErr() {
		return f()
	}

	return r.left
}

// UnwrapOrFail returns the success value or fails the test if it's an error.
func (r Result[T]) UnwrapOrFail(t *testing.T) T {
	t.Helper()

	if r.IsErr() {
		t.Fatalf("Result[%T] contained error: %v", r.left, r.right)
	}

	var zero T

	return zero
}

// FlatMap applies a function that returns a Result to the success value if it
// exists.
func (r Result[T]) FlatMap(f func(T) Result[T]) Result[T] {
	if r.IsOk() {
		return r
	}

	return f(r.left)
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
		return f(r.left)
	}

	return Err[B](r.right)
}

// AndThen is an alias for FlatMap. This along with OrElse can be used to
// Railway Oriented Programming (ROP).
func AndThen[A, B any](r Result[A], f func(A) Result[B]) Result[B] {
	return FlatMap(r, f)
}

// AndThen2 applies a function that returns a Result[C] to the success values
// of two Result types if both exist.
func AndThen2[A, B, C any](ra Result[A], rb Result[B],
	f func(A, B) Result[C]) Result[C] {

	return AndThen(ra, func(a A) Result[C] {
		return AndThen(rb, func(b B) Result[C] {
			return f(a, b)
		})
	})
}
