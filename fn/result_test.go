package fn

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResult(t *testing.T) {
	t.Run("UnwrapOrFail", func(t *testing.T) {
		// Test Ok case with real t.
		opt := Ok(123)
		n := opt.UnwrapOrFail(t)
		require.Equal(t, 123, n)

		// Test Ok case with mock t.
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
		opt = Err[int](errors.New("test error"))
		mockT = &testingMock{}
		_ = opt.UnwrapOrFail(mockT)
		require.True(t, mockT.helperCalled)
		require.True(t, mockT.errorfCalled)
		require.True(t, mockT.failNowCalled)
	})
}
