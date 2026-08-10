package graphdb

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lightningnetwork/lnd/lnwire"
)

// TestChannelUpdateInfoFreshness tests that concrete freshness values preserve
// their version-specific timestamp representation at the API boundary.
func TestChannelUpdateInfoFreshness(t *testing.T) {
	t.Parallel()

	scid := lnwire.NewShortChanIDFromInt(1)
	v1Time := time.Unix(123, 0)
	v1 := NewV1ChannelUpdateInfo(scid, v1Time, time.Time{})

	require.Equal(
		t, lnwire.UnixTimestamp(123), v1.Node1FreshnessTimestamp(),
	)
	require.Equal(t, v1Time, v1.Node1FreshnessTime())

	v2 := NewV2ChannelUpdateInfo(scid, 456, 0)
	require.Equal(
		t, lnwire.BlockHeightTimestamp(456),
		v2.Node1FreshnessTimestamp(),
	)
	require.True(t, v2.Node1FreshnessTime().IsZero())
}
