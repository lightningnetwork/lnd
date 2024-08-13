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
