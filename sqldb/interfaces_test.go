package sqldb

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRandRetryDelay verifies that retry delays stay within their jitter range
// and are capped without overflowing for large retry settings.
func TestRandRetryDelay(t *testing.T) {
	t.Parallel()

	t.Run("initial jitter", func(t *testing.T) {
		delay := randRetryDelay(
			DefaultRetryDelay, DefaultMaxRetryDelay, 0,
		)

		require.GreaterOrEqual(
			t, delay, DefaultRetryDelay/2,
		)
		require.LessOrEqual(
			t, delay, DefaultRetryDelay*3/2,
		)
	})

	t.Run("uncapped exponential backoff", func(t *testing.T) {
		delay := randRetryDelay(
			DefaultRetryDelay, DefaultMaxRetryDelay, 2,
		)

		require.GreaterOrEqual(t, delay, DefaultRetryDelay*2)
		require.Less(t, delay, DefaultRetryDelay*6)
	})

	t.Run("exponential backoff reaches maximum", func(t *testing.T) {
		delay := randRetryDelay(
			DefaultRetryDelay, DefaultMaxRetryDelay, 6,
		)

		require.Equal(t, DefaultMaxRetryDelay, delay)
	})

	t.Run("large initial delay is capped", func(t *testing.T) {
		delay := randRetryDelay(
			DefaultMaxRetryDelay*4, DefaultMaxRetryDelay, 0,
		)

		require.Equal(t, DefaultMaxRetryDelay, delay)
	})

	t.Run("large retry count does not overflow", func(t *testing.T) {
		delay := randRetryDelay(
			DefaultMaxRetryDelay, DefaultMaxRetryDelay, 32,
		)

		require.Equal(t, DefaultMaxRetryDelay, delay)
	})

	t.Run("jitter addition does not overflow", func(t *testing.T) {
		delay := randRetryDelay(
			time.Duration(math.MaxInt64), DefaultMaxRetryDelay, 0,
		)

		require.Equal(t, DefaultMaxRetryDelay, delay)
	})
}
