package lnwire

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestChannelTypeEncodeDecode tests that we're able to properly encode and
// decode channel types within TLV streams.
func TestChannelTypeEncodeDecode(t *testing.T) {
	t.Parallel()

	record1 := RawFeatureVectorRecordProducer{
		RawFeatureVector: *NewRawFeatureVector(
			StaticRemoteKeyRequired,
			AnchorsZeroFeeHtlcTxRequired,
		),
		Type: ChannelTypeRecordType,
	}

	var extraData ExtraOpaqueData
	require.NoError(t, extraData.PackRecordsFromProducers(&record1))

	record2 := NewRawFeatureVectorRecordProducer(ChannelTypeRecordType)
	tlvs, err := extraData.ExtractRecordsFromProducers(record2)
	require.NoError(t, err)

	require.Contains(t, tlvs, ChannelTypeRecordType)
	require.Equal(t, record1.RawFeatureVector, record2.RawFeatureVector)
}
