package lnwire

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUnixTimestamp tests the IsZero and Cmp methods of UnixTimestamp.
func TestUnixTimestamp(t *testing.T) {
	t.Parallel()

	t.Run("IsZero", func(t *testing.T) {
		t.Parallel()

		require.True(t, UnixTimestamp(0).IsZero())
		require.False(t, UnixTimestamp(1).IsZero())
		require.False(t, UnixTimestamp(1_000_000).IsZero())
	})

	t.Run("Cmp", func(t *testing.T) {
		t.Parallel()

		a := UnixTimestamp(100)
		b := UnixTimestamp(200)

		result, err := a.Cmp(b)
		require.NoError(t, err)
		require.Equal(t, LessThan, result)

		result, err = b.Cmp(a)
		require.NoError(t, err)
		require.Equal(t, GreaterThan, result)

		c := UnixTimestamp(100)
		result, err = a.Cmp(c)
		require.NoError(t, err)
		require.Equal(t, EqualTo, result)
	})

	t.Run("Cmp wrong type", func(t *testing.T) {
		t.Parallel()

		_, err := UnixTimestamp(1).Cmp(BlockHeightTimestamp(1))
		require.ErrorContains(t, err, "expected UnixTimestamp")
	})
}

// TestBlockHeightTimestamp tests the IsZero and Cmp methods of
// BlockHeightTimestamp.
func TestBlockHeightTimestamp(t *testing.T) {
	t.Parallel()

	t.Run("IsZero", func(t *testing.T) {
		t.Parallel()

		require.True(t, BlockHeightTimestamp(0).IsZero())
		require.False(t, BlockHeightTimestamp(1).IsZero())
		require.False(t, BlockHeightTimestamp(800_000).IsZero())
	})

	t.Run("Cmp", func(t *testing.T) {
		t.Parallel()

		a := BlockHeightTimestamp(500)
		b := BlockHeightTimestamp(800)

		result, err := a.Cmp(b)
		require.NoError(t, err)
		require.Equal(t, LessThan, result)

		result, err = b.Cmp(a)
		require.NoError(t, err)
		require.Equal(t, GreaterThan, result)

		c := BlockHeightTimestamp(500)
		result, err = a.Cmp(c)
		require.NoError(t, err)
		require.Equal(t, EqualTo, result)
	})

	t.Run("Cmp wrong type", func(t *testing.T) {
		t.Parallel()

		_, err := BlockHeightTimestamp(1).Cmp(UnixTimestamp(1))
		require.ErrorContains(t, err, "expected BlockHeightTimestamp")
	})
}
