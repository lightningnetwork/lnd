package fn

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
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

// MapOk applies an endomorphic function to the success value if it exists.
func (r Result[T]) MapOk(f func(T) T) Result[T] {
	return Result[T]{
		MapLeft[T, error](f)(r.Either),
	}
}

// MapErr applies an endomorphic function to the error value if it exists.
func (r Result[T]) MapErr(f func(error) error) Result[T] {
	return Result[T]{
		MapRight[T](f)(r.Either),
	}
}

// MapOk applies a non-endomorphic function to the success value if it exists
// and returns a Result of the new type.
func MapOk[A, B any](f func(A) B) func(Result[A]) Result[B] {
	return func(r Result[A]) Result[B] {
		return Result[B]{MapLeft[A, error](f)(r.Either)}
	}
}

// OkToSome mutes the error value of the result.
func (r Result[T]) OkToSome() Option[T] {
	return r.Either.LeftToSome()
}

// WhenOk executes the given function if the Result is a success.
func (r Result[T]) WhenOk(f func(T)) {
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

	require.True(
		t, r.IsOk(), "Result[%T] contained error: %v", r.left, r.right,
	)

	return r.left
}

// FlattenResult takes a nested Result and joins the two functor layers into
// one.
func FlattenResult[A any](r Result[Result[A]]) Result[A] {
	if r.IsErr() {
		return Err[A](r.right)
	}

	if r.left.IsErr() {
		return Err[A](r.left.right)
	}

	return r.left
}

// FlatMap applies a kleisli endomorphic function that returns a Result to the
// success value if it exists.
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

// Sink consumes a Result, either propagating its error or processing its
// success value with a function that can fail.
func (r Result[A]) Sink(f func(A) error) error {
	if r.IsErr() {
		return r.right
	}

	return f(r.left)
}

// TransposeResOpt transposes the Result[Option[A]] into a Option[Result[A]].
// This has the effect of leaving an A value alone while inverting the Result
// and Option layers. If there is no internal A value, it will convert the
// non-success value to the proper one in the transposition.
func TransposeResOpt[A any](r Result[Option[A]]) Option[Result[A]] {
	if r.IsErr() {
		return Some(Err[A](r.right))
	}

	return MapOption(Ok[A])(r.left)
}
