package fn

import (
	"errors"
	"fmt"
	"testing"

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
