package lnwire

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRouteBlindingPointEncodeDecode tests that we're able to properly
// encode and decode a BlindingPoint type within TLV streams.
func TestRouteBlindingPointEncodeDecode(t *testing.T) {
	t.Parallel()

	pubKey, _ := randPubKey()
	blindingPoint := BlindingPoint(*pubKey)

	var extraData ExtraOpaqueData
	require.NoError(t, extraData.PackRecords(&blindingPoint))

	var blindingPoint2 BlindingPoint
	tlvs, err := extraData.ExtractRecords(&blindingPoint2)
	require.NoError(t, err)

	require.Contains(t, tlvs, BlindingPointRecordType)
	require.Equal(t, blindingPoint, blindingPoint2)
}
