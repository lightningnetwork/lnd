package fn

import (
	"errors"
	"fmt"
	"testing"
	"testing/quick"

	"github.com/stretchr/testify/require"
)

func TestOptionUnwrapOrFail(t *testing.T) {
	require.Equal(t, Some(1).UnwrapOrFail(t), 1)
}

func TestSomeToOk(t *testing.T) {
	err := errors.New("err")
	require.Equal(t, Some(1).SomeToOk(err), Ok(1))
	require.Equal(t, None[uint8]().SomeToOk(err), Err[uint8](err))
}

func TestSomeToOkf(t *testing.T) {
	errStr := "err"
	require.Equal(t, Some(1).SomeToOkf(errStr), Ok(1))
	require.Equal(
		t, None[uint8]().SomeToOkf(errStr),
		Err[uint8](fmt.Errorf(errStr)),
	)
}

func TestPropTransposeOptResInverts(t *testing.T) {
	f := func(i uint) bool {
		var o Option[Result[uint]]
		switch i % 3 {
		case 0:
			o = Some(Ok(i))
		case 1:
			o = Some(Errf[uint]("error"))
		case 2:
			o = None[Result[uint]]()
		default:
			return false
		}

		odd := TransposeOptRes(o) ==
			TransposeOptRes(TransposeResOpt(TransposeOptRes(o)))
		even := TransposeResOpt(TransposeOptRes(o)) == o

		return odd && even
	}

	require.NoError(t, quick.Check(f, nil))
}
