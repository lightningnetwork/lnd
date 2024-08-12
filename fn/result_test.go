package fn

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResultUnwrapOrFail(t *testing.T) {
	require.Equal(t, Ok(1).UnwrapOrFail(t), 1)
}
