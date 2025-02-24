package fn

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOkOpt(t *testing.T) {
	value := 42
	resOpt := OkOpt(value)
	opt, err := resOpt.Unpack()
	require.NoError(t, err)
	require.True(t, opt.IsSome(), "expected Option to be Some")
	require.Equal(t, value, opt.UnsafeFromSome())
	require.True(t, resOpt.IsSome())
	require.False(t, resOpt.IsNone())
}

func TestNoneOpt(t *testing.T) {
	resOpt := NoneOpt[int]()
	opt, err := resOpt.Unpack()
	require.NoError(t, err)
	require.True(t, opt.IsNone(), "expected Option to be None")
	require.True(t, resOpt.IsNone())
	require.False(t, resOpt.IsSome())
}

func TestErrOpt(t *testing.T) {
	errMsg := "some error"
	resOpt := ErrOpt[int](errors.New(errMsg))
	_, err := resOpt.Unpack()
	require.Error(t, err)
	require.EqualError(t, err, errMsg)
	require.False(t, resOpt.IsSome())
	require.False(t, resOpt.IsNone())
}

func TestMapResultOptOk(t *testing.T) {
	value := 10
	resOpt := OkOpt(value)
	mapped := MapResultOpt(resOpt, func(i int) int {
		return i * 3
	})
	opt, err := mapped.Unpack()
	require.NoError(t, err)
	require.True(t, opt.IsSome(), "expected mapped Option to be Some")
	require.Equal(t, 30, opt.UnsafeFromSome())
}

func TestMapResultOptNone(t *testing.T) {
	resOpt := NoneOpt[int]()
	mapped := MapResultOpt(resOpt, func(i int) int {
		return i * 3
	})
	opt, err := mapped.Unpack()
	require.NoError(t, err)
	require.True(t, opt.IsNone(), "expected mapped Option to remain None")
}

func TestMapResultOptErr(t *testing.T) {
	errMsg := "error mapping"
	resOpt := ErrOpt[int](errors.New(errMsg))
	mapped := MapResultOpt(resOpt, func(i int) int {
		return i * 3
	})
	_, err := mapped.Unpack()
	require.Error(t, err)
	require.EqualError(t, err, errMsg)
}

func incrementOpt(x int) ResultOpt[int] {
	return OkOpt(x + 1)
}

func TestAndThenResultOptOk(t *testing.T) {
	resOpt := OkOpt(5)
	chained := AndThenResultOpt(resOpt, incrementOpt)
	opt, err := chained.Unpack()
	require.NoError(t, err)
	require.True(t, opt.IsSome(), "expected chained Option to be Some")
	require.Equal(t, 6, opt.UnsafeFromSome())
}

func TestAndThenResultOptNone(t *testing.T) {
	resOpt := NoneOpt[int]()
	chained := AndThenResultOpt(resOpt, incrementOpt)
	opt, err := chained.Unpack()
	require.NoError(t, err)
	require.True(t, opt.IsNone(), "expected chained result to remain None")
}

func TestAndThenResultOptErr(t *testing.T) {
	errMsg := "error in initial result"
	resOpt := ErrOpt[int](errors.New(errMsg))
	chained := AndThenResultOpt(resOpt, incrementOpt)
	_, err := chained.Unpack()
	require.Error(t, err)
	require.EqualError(t, err, errMsg)
}

func maybeEvenOpt(x int) ResultOpt[int] {
	if x%2 == 0 {
		return OkOpt(x / 2)
	}
	return NoneOpt[int]()
}

func TestAndThenResultOptProducesNone(t *testing.T) {
	// Given an odd number, maybeEvenOpt returns None.
	resOpt := OkOpt(5)
	chained := AndThenResultOpt(resOpt, maybeEvenOpt)
	opt, err := chained.Unpack()
	require.NoError(t, err)
	require.True(t, opt.IsNone(), "expected chained result to be None")
}

func TestMapAndThenIntegration(t *testing.T) {
	resOpt := OkOpt(2)
	chained := MapResultOpt(
		AndThenResultOpt(resOpt, func(x int) ResultOpt[int] {
			return OkOpt(x + 3)
		}),
		func(y int) int {
			return y * 2
		},
	)
	opt, err := chained.Unpack()
	require.NoError(t, err)
	require.True(
		t, opt.IsSome(), "expected integrated mapping and "+
			"chaining to produce Some",
	)
	require.Equal(t, 10, opt.UnsafeFromSome())
}
