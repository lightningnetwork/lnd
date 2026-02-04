package fn

import (
	"errors"
	"fmt"
	"testing"
	"testing/quick"

	"github.com/stretchr/testify/require"
)

func TestResultUnwrapOrFail(t *testing.T) {
	require.Equal(t, Ok(1).UnwrapOrFail(t), 1)
}

func TestOkToSome(t *testing.T) {
	require.Equal(t, Ok(1).OkToSome(), Some(1))
	require.Equal(
		t, Err[uint8](errors.New("err")).OkToSome(), None[uint8](),
	)
}

func TestMapOk(t *testing.T) {
	inc := func(i int) int {
		return i + 1
	}
	f := func(i int) bool {
		ok := Ok(i)
		return MapOk(inc)(ok) == Ok(inc(i))
	}

	require.NoError(t, quick.Check(f, nil))
}

func TestFlattenResult(t *testing.T) {
	f := func(i int) bool {
		e := fmt.Errorf("error")

		x := FlattenResult(Ok(Ok(i))) == Ok(i)
		y := FlattenResult(Ok(Err[int](e))) == Err[int](e)
		z := FlattenResult(Err[Result[int]](e)) == Err[int](e)

		return x && y && z
	}

	require.NoError(t, quick.Check(f, nil))
}

func TestPropTransposeResOptInverts(t *testing.T) {
	f := func(i uint) bool {
		var r Result[Option[uint]]
		switch i % 3 {
		case 0:
			r = Ok(Some(i))
		case 1:
			r = Ok(None[uint]())
		case 2:
			r = Errf[Option[uint]]("error")
		default:
			return false
		}

		odd := TransposeResOpt(TransposeOptRes(TransposeResOpt(r))) ==
			TransposeResOpt(r)

		even := TransposeOptRes(TransposeResOpt(r)) == r

		return odd && even
	}

	require.NoError(t, quick.Check(f, nil))
}

func TestSinkOnErrNoContinutationCall(t *testing.T) {
	called := false
	res := Err[uint8](errors.New("err")).Sink(
		func(a uint8) error {
			called = true
			return nil
		},
	)

	require.False(t, called)
	require.NotNil(t, res)
}

func TestSinkOnOkContinuationCall(t *testing.T) {
	called := false
	res := Ok(uint8(1)).Sink(
		func(a uint8) error {
			called = true
			return nil
		},
	)

	require.True(t, called)
	require.Nil(t, res)
}

var errFlatMap = errors.New("fail")
var errFlatMapOrig = errors.New("original")

var flatMapTestCases = []struct {
	name      string
	input     Result[int]
	fnA       func(int) Result[int]
	fnB       func(int) Result[string]
	expectedA Result[int]
	expectedB Result[string]
}{
	{
		name:  "Ok to Ok",
		input: Ok(1),
		fnA:   func(i int) Result[int] { return Ok(i + 1) },
		fnB: func(i int) Result[string] {
			return Ok(fmt.Sprintf("%d", i+1))
		},
		expectedA: Ok(2),
		expectedB: Ok("2"),
	},
	{
		name:  "Ok to Err",
		input: Ok(1),
		fnA: func(i int) Result[int] {
			return Err[int](errFlatMap)
		},
		fnB: func(i int) Result[string] {
			return Err[string](errFlatMap)
		},
		expectedA: Err[int](errFlatMap),
		expectedB: Err[string](errFlatMap),
	},
	{
		name:  "Err to Err (function not called)",
		input: Err[int](errFlatMapOrig),
		fnA:   func(i int) Result[int] { return Ok(i + 1) },
		fnB: func(i int) Result[string] {
			return Ok("should not happen")
		},
		expectedA: Err[int](errFlatMapOrig),
		expectedB: Err[string](errFlatMapOrig),
	},
}

var orElseTestCases = []struct {
	name     string
	input    Result[int]
	fn       func(error) Result[int]
	expected Result[int]
}{
	{
		name:     "Ok to Ok (function not called)",
		input:    Ok(1),
		fn:       func(err error) Result[int] { return Ok(2) },
		expected: Ok(1),
	},
	{
		name:     "Err to Ok",
		input:    Err[int](errFlatMapOrig),
		fn:       func(err error) Result[int] { return Ok(2) },
		expected: Ok(2),
	},
	{
		name:  "Err to Err",
		input: Err[int](errFlatMapOrig),
		fn: func(err error) Result[int] {
			return Err[int](errFlatMap)
		},
		expected: Err[int](errFlatMap),
	},
}

func TestFlatMap(t *testing.T) {
	for _, tc := range flatMapTestCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			actual := tc.input.FlatMap(tc.fnA)
			require.Equal(t, tc.expectedA, actual)
		})
	}
}

func TestAndThenMethod(t *testing.T) {
	// Since AndThen is just an alias for FlatMap, we can reuse the same
	// test cases.
	for _, tc := range flatMapTestCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			actual := tc.input.AndThen(tc.fnA)
			require.Equal(t, tc.expectedA, actual)
		})
	}
}

func TestOrElseMethod(t *testing.T) {
	for _, tc := range orElseTestCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			actual := tc.input.OrElse(tc.fn)
			require.Equal(t, tc.expected, actual)
		})
	}
}

func TestFlatMapResult(t *testing.T) {
	for _, tc := range flatMapTestCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			actual := FlatMapResult(tc.input, tc.fnB)
			require.Equal(t, tc.expectedB, actual)
		})
	}
}

func TestAndThenFunc(t *testing.T) {
	// Since AndThen is just an alias for FlatMapResult, we can reuse the
	// same test cases.
	for _, tc := range flatMapTestCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			actual := AndThen(tc.input, tc.fnB)
			require.Equal(t, tc.expectedB, actual)
		})
	}
}
