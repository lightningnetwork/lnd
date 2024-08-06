package fn

import (
	"errors"
	"testing"

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
