package lntypes

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMin tests getting correct minimal numbers.
func TestMin(t *testing.T) {
	t.Parallel()

	require.Equal(t, 1, Min(1, 1))
	require.Equal(t, 1, Min(1, 2))
	require.Equal(t, 1.5, Min(1.5, 2.5))
}

// TestMax tests getting correct maximal numbers.
func TestMax(t *testing.T) {
	t.Parallel()

	require.Equal(t, 1, Max(1, 1))
	require.Equal(t, 2, Max(1, 2))
	require.Equal(t, 2.5, Max(1.5, 2.5))
}
