package lnwire

import (
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/maps"
)

// TestFilteredCustomRecords tests that we can separate TLV types into custom
// and standard records.
func TestFilteredCustomRecords(t *testing.T) {
	// Create a new tlv.TypeMap with some records, both in the standard and
	// in the custom range.
	typeMap := make(tlv.TypeMap)
	typeMap[45] = []byte{1, 2, 3}
	typeMap[55] = []byte{4, 5, 6}
	typeMap[65] = []byte{7, 8, 9}
	typeMap[65536] = []byte{11, 22, 33}
	typeMap[65537] = []byte{44, 55, 66}
	typeMap[65538] = []byte{77, 88, 99}

	customRecords, remainder, err := FilteredCustomRecords(typeMap)
	require.NoError(t, err)

	require.Len(t, customRecords, 3)
	require.Len(t, remainder, 3)

	require.Contains(t, maps.Keys(customRecords), uint64(65536))
	require.Contains(t, maps.Keys(customRecords), uint64(65537))
	require.Contains(t, maps.Keys(customRecords), uint64(65538))

	require.Contains(t, maps.Keys(remainder), tlv.Type(45))
	require.Contains(t, maps.Keys(remainder), tlv.Type(55))
	require.Contains(t, maps.Keys(remainder), tlv.Type(65))
}
