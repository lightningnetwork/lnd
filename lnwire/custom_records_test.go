package lnwire

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCustomRecords tests the custom records serialization and deserialization,
// as well as copying and producing records.
func TestCustomRecords(t *testing.T) {
	testCases := []struct {
		name            string
		customTypes     tlv.TypeMap
		expectedRecords CustomRecords
		expectedErr     string
	}{
		{
			name:            "empty custom records",
			customTypes:     tlv.TypeMap{},
			expectedRecords: nil,
		},
		{
			name: "custom record with invalid type",
			customTypes: tlv.TypeMap{
				123: []byte{1, 2, 3},
			},
			expectedErr: "TLV type below min: 65536",
		},
		{
			name: "valid custom record",
			customTypes: tlv.TypeMap{
				65536: []byte{1, 2, 3},
			},
			expectedRecords: map[uint64][]byte{
				65536: {1, 2, 3},
			},
		},
		{
			name: "valid custom records, wrong order",
			customTypes: tlv.TypeMap{
				65537: []byte{3, 4, 5},
				65536: []byte{1, 2, 3},
			},
			expectedRecords: map[uint64][]byte{
				65536: {1, 2, 3},
				65537: {3, 4, 5},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			records, err := NewCustomRecords(tc.customTypes)

			if tc.expectedErr != "" {
				require.ErrorContains(t, err, tc.expectedErr)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expectedRecords, records)

			// Serialize, then parse the records again.
			blob, err := records.Serialize()
			require.NoError(t, err)

			parsedRecords, err := ParseCustomRecords(blob)
			require.NoError(t, err)

			require.Equal(t, tc.expectedRecords, parsedRecords)

			// Copy() should also return the same records.
			require.Equal(
				t, tc.expectedRecords, parsedRecords.Copy(),
			)

			// RecordProducers() should also allow us to serialize
			// the records again.
			serializedProducers := serializeRecordProducers(
				t, parsedRecords.RecordProducers(),
			)

			require.Equal(t, blob, serializedProducers)
		})
	}
}

// TestCustomRecordsExtendRecordProducers tests that we can extend a slice of
// record producers with custom records.
func TestCustomRecordsExtendRecordProducers(t *testing.T) {
	testCases := []struct {
		name           string
		existingTypes  map[uint64][]byte
		customRecords  CustomRecords
		expectedResult tlv.TypeMap
		expectedErr    string
	}{
		{
			name: "normal merge",
			existingTypes: map[uint64][]byte{
				123: {3, 4, 5},
				345: {1, 2, 3},
			},
			customRecords: CustomRecords{
				65536: {1, 2, 3},
			},
			expectedResult: tlv.TypeMap{
				123:   {3, 4, 5},
				345:   {1, 2, 3},
				65536: {1, 2, 3},
			},
		},
		{
			name: "duplicates",
			existingTypes: map[uint64][]byte{
				123:   {3, 4, 5},
				345:   {1, 2, 3},
				65536: {1, 2, 3},
			},
			customRecords: CustomRecords{
				65536: {1, 2, 3},
			},
			expectedErr: "contains a TLV type that is already " +
				"present in the existing records: 65536",
		},
		{
			name: "non custom type in custom records",
			existingTypes: map[uint64][]byte{
				123:   {3, 4, 5},
				345:   {1, 2, 3},
				65536: {1, 2, 3},
			},
			customRecords: CustomRecords{
				123: {1, 2, 3},
			},
			expectedErr: "TLV type below min: 65536",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			nonCustomRecords := tlv.MapToRecords(tc.existingTypes)
			nonCustomProducers := RecordsAsProducers(
				nonCustomRecords,
			)

			combined, err := tc.customRecords.ExtendRecordProducers(
				nonCustomProducers,
			)

			if tc.expectedErr != "" {
				require.ErrorContains(t, err, tc.expectedErr)
				return
			}

			require.NoError(t, err)

			serializedProducers := serializeRecordProducers(
				t, combined,
			)

			stream, err := tlv.NewStream()
			require.NoError(t, err)

			parsedMap, err := stream.DecodeWithParsedTypes(
				bytes.NewReader(serializedProducers),
			)
			require.NoError(t, err)

			require.Equal(t, tc.expectedResult, parsedMap)
		})
	}
}

// serializeRecordProducers is a helper function that serializes a slice of
// record producers into a byte slice.
func serializeRecordProducers(t *testing.T,
	producers []tlv.RecordProducer) []byte {

	tlvRecords := fn.Map(
		producers,
		func(p tlv.RecordProducer) tlv.Record {
			return p.Record()
		},
	)

	stream, err := tlv.NewStream(tlvRecords...)
	require.NoError(t, err)

	var b bytes.Buffer
	err = stream.Encode(&b)
	require.NoError(t, err)

	return b.Bytes()
}

func TestCustomRecordsMergedCopy(t *testing.T) {
	tests := []struct {
		name  string
		c     CustomRecords
		other CustomRecords
		want  CustomRecords
	}{
		{
			name: "nil records",
			want: make(CustomRecords),
		},
		{
			name:  "empty records",
			c:     make(CustomRecords),
			other: make(CustomRecords),
			want:  make(CustomRecords),
		},
		{
			name: "distinct records",
			c: CustomRecords{
				1: {1, 2, 3},
			},
			other: CustomRecords{
				2: {4, 5, 6},
			},
			want: CustomRecords{
				1: {1, 2, 3},
				2: {4, 5, 6},
			},
		},
		{
			name: "same records, different values",
			c: CustomRecords{
				1: {1, 2, 3},
			},
			other: CustomRecords{
				1: {4, 5, 6},
			},
			want: CustomRecords{
				1: {4, 5, 6},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.c.MergedCopy(tt.other)
			assert.Equal(t, tt.want, result)
		})
	}
}
