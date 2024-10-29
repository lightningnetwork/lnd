package fn

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOption(t *testing.T) {
	t.Run("UnwrapOrFail", func(t *testing.T) {
		// Test Some case with real t.
		opt := Some(123)
		n := opt.UnwrapOrFail(t)
		require.Equal(t, 123, n)

		// Test Some case with mock t.
		mockT := &testingMock{}
		n = opt.UnwrapOrFail(mockT)
		require.Equal(t, 123, n)
		require.True(t, mockT.helperCalled)
		require.False(t, mockT.errorfCalled)
		require.False(t, mockT.failNowCalled)

		// Make sure it works with assert.CollectT.
		require.EventuallyWithT(t, func(t *assert.CollectT) {
			n = opt.UnwrapOrFail(t)
			require.Equal(t, 123, n)
		}, time.Second, time.Millisecond)

		// Test None case with mock t.
		opt = None[int]()
		mockT = &testingMock{}
		_ = opt.UnwrapOrFail(mockT)
		require.True(t, mockT.helperCalled)
		require.True(t, mockT.errorfCalled)
		require.True(t, mockT.failNowCalled)
	})
}
