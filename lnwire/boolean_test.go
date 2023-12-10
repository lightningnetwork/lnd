package lnwire

import (
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestBooleanRecord tests the encoding and decoding of a boolean tlv record.
func TestBooleanRecord(t *testing.T) {
	t.Parallel()

	const recordType = tlv.Type(0)

	b1 := BooleanRecordProducer{
		Bool: false,
		Type: recordType,
	}

	var extraData ExtraOpaqueData

	// A false boolean should not be encoded.
	require.ErrorContains(t, extraData.PackRecordsFromProducers(&b1),
		"a boolean record should only be encoded if the value of "+
			"the boolean is true")

	b2 := BooleanRecordProducer{
		Bool: true,
		Type: recordType,
	}
	require.NoError(t, extraData.PackRecordsFromProducers(&b2))

	b3 := NewBooleanRecordProducer(recordType)
	tlvs, err := extraData.ExtractRecordsFromProducers(b3)
	require.NoError(t, err)

	require.Contains(t, tlvs, recordType)
	require.Equal(t, b2.Bool, b3.Bool)
}
