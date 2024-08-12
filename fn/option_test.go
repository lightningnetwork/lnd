package fn

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOptionUnwrapOrFail(t *testing.T) {
	require.Equal(t, Some(1).UnwrapOrFail(t), 1)
}
