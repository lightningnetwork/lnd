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
