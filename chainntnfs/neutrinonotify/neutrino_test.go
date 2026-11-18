package neutrinonotify

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestExtendHistoricalDispatch asserts that an initial scan catches up to the
// backend tip while a supplemental prefix retains its fixed boundary.
func TestExtendHistoricalDispatch(t *testing.T) {
	endHeight := uint32(100)
	extendHistoricalDispatch(&endHeight, false, 120)
	require.Equal(t, uint32(120), endHeight)

	endHeight = 100
	extendHistoricalDispatch(&endHeight, true, 120)
	require.Equal(t, uint32(100), endHeight)
}
