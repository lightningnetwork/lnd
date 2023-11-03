package routing

import (
	"testing"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

var (
	dummyPayLoadSize uint64              = 100
	dummyAmt         lnwire.MilliSatoshi = 1
	dummyCltvDelta   uint32              = 1
	dummyChannelID   uint64
)

// Define a mockPayloadSizeFunc for testing.
func mockPayloadSizeFunc(amount lnwire.MilliSatoshi, expiry uint32,
	legacy bool, channelID uint64) uint64 {
	return uint64(dummyPayLoadSize)
}

// TestDirectedEdge tests that all pointer receivers of the type DirectedEdge
// return the expected results.
func TestAdditionalEdge(t *testing.T) {

	mockPolicy := &channeldb.CachedEdgePolicy{}

	additionalEdge := AdditionalEdge{
		policy:          mockPolicy,
		payloadSizeFunc: mockPayloadSizeFunc,
	}

	require.Equal(t, mockPolicy, additionalEdge.edgePolicy())
	payloadSize, err := additionalEdge.hopPayloadSize()
	require.NoError(t, err)

	hopSize := payloadSize(
		dummyAmt, dummyCltvDelta, false, dummyChannelID)

	require.Equal(t, dummyPayLoadSize, hopSize)

	// replace the payloadSizeFunc with nil and expect an error when
	// trying to use it.
	additionalEdge.payloadSizeFunc = nil

	payloadSize, err = additionalEdge.hopPayloadSize()

	require.ErrorIs(t, err, ErrNoPayLoadSizeFunc)
}
