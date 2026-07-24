package lnwire

import (
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestAttrDataRoundTrip tests that attribution data survives a round-trip
// through AttrDataToExtraData and ExtraDataToAttrData.
func TestAttrDataRoundTrip(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		attrData []byte
	}{
		{
			name:     "nil attribution data",
			attrData: nil,
		},
		{
			name:     "empty attribution data",
			attrData: []byte{},
		},
		{
			name:     "small attribution data",
			attrData: []byte{0x01, 0x02, 0x03},
		},
		{
			name:     "realistic size attribution data",
			attrData: make([]byte, 1200),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			extraData, err := AttrDataToExtraData(tc.attrData)
			require.NoError(t, err)

			recovered, err := ExtraDataToAttrData(extraData)
			require.NoError(t, err)

			// Both nil and empty should round-trip to equivalent
			// "no data" values.
			if len(tc.attrData) == 0 {
				require.Empty(t, recovered)
				return
			}

			require.Equal(t, tc.attrData, recovered)
		})
	}
}

// TestExtraDataToAttrDataNoRecord tests that ExtraDataToAttrData returns nil
// when the extra data does not contain an attribution record.
func TestExtraDataToAttrDataNoRecord(t *testing.T) {
	t.Parallel()

	// Build extra data with a different TLV type (not the attribution
	// data type).
	otherType := tlv.Type(200)
	records := make(tlv.TypeMap)
	records[otherType] = []byte{0xaa, 0xbb}
	extraData, err := NewExtraOpaqueData(records)
	require.NoError(t, err)

	attrData, err := ExtraDataToAttrData(extraData)
	require.NoError(t, err)
	require.Nil(t, attrData)
}

// TestAttrDataPreservesOtherRecords verifies that encoding attribution data
// into ExtraOpaqueData produces a valid TLV stream with the correct type.
func TestAttrDataTlvType(t *testing.T) {
	t.Parallel()

	payload := []byte{0xde, 0xad, 0xbe, 0xef}
	extraData, err := AttrDataToExtraData(payload)
	require.NoError(t, err)

	// Extract all records and verify the attribution type is present.
	records, err := extraData.ExtractRecords()
	require.NoError(t, err)

	value, ok := records[AttrDataTlvType().TypeVal()]
	require.True(t, ok, "expected attribution TLV type %d in records",
		AttrDataTlvType().TypeVal())
	require.Equal(t, payload, value)
}

// TestExtraDataToAttrDataEmpty tests that an empty ExtraOpaqueData returns nil
// attribution data without error.
func TestExtraDataToAttrDataEmpty(t *testing.T) {
	t.Parallel()

	var empty ExtraOpaqueData
	attrData, err := ExtraDataToAttrData(empty)
	require.NoError(t, err)
	require.Nil(t, attrData)
}
