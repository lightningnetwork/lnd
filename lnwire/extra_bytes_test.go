package lnwire

import (
	"bytes"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

var (
	tlvType1 tlv.TlvType1
	tlvType2 tlv.TlvType2
	tlvType3 tlv.TlvType3
)

// TestExtraOpaqueDataEncodeDecode tests that we're able to encode/decode
// arbitrary payloads.
func TestExtraOpaqueDataEncodeDecode(t *testing.T) {
	t.Parallel()

	type testCase struct {
		// emptyBytes indicates if we should try to encode empty bytes
		// or not.
		emptyBytes bool

		// inputBytes if emptyBytes is false, then we'll read in this
		// set of bytes instead.
		inputBytes []byte
	}

	// We should be able to read in an arbitrary set of bytes as an
	// ExtraOpaqueData, then encode those new bytes into a new instance.
	// The final two instances should be identical.
	scenario := func(test testCase) bool {
		var (
			extraData ExtraOpaqueData
			b         bytes.Buffer
		)

		copy(extraData[:], test.inputBytes)

		if err := extraData.Encode(&b); err != nil {
			t.Fatalf("unable to encode extra data: %v", err)
			return false
		}

		var newBytes ExtraOpaqueData
		if err := newBytes.Decode(&b); err != nil {
			t.Fatalf("unable to decode extra bytes: %v", err)
			return false
		}

		if !bytes.Equal(extraData[:], newBytes[:]) {
			t.Fatalf("expected %x, got %x", extraData,
				newBytes)
			return false
		}

		return true
	}

	// We'll make a function to generate random test data. Half of the
	// time, we'll actually feed in blank bytes.
	quickCfg := &quick.Config{
		Values: func(v []reflect.Value, r *rand.Rand) {
			var newTestCase testCase
			if r.Int31()%2 == 0 {
				newTestCase.emptyBytes = true
			}

			if !newTestCase.emptyBytes {
				numBytes := r.Int31n(1000)
				newTestCase.inputBytes = make([]byte, numBytes)

				_, err := r.Read(newTestCase.inputBytes)
				require.NoError(t, err)
			}

			v[0] = reflect.ValueOf(newTestCase)
		},
	}

	if err := quick.Check(scenario, quickCfg); err != nil {
		t.Fatalf("encode+decode test failed: %v", err)
	}
}

// TestExtraOpaqueDataPackUnpackRecords tests that we're able to pack a set of
// tlv.Records into a stream, and unpack them on the other side to obtain the
// same set of records.
func TestExtraOpaqueDataPackUnpackRecords(t *testing.T) {
	t.Parallel()

	var (
		type1 tlv.Type = 1
		type2 tlv.Type = 2

		channelType1 uint8 = 2
		channelType2 uint8

		hop1 uint32 = 99
		hop2 uint32
	)
	testRecordsProducers := []tlv.RecordProducer{
		&recordProducer{tlv.MakePrimitiveRecord(type1, &channelType1)},
		&recordProducer{tlv.MakePrimitiveRecord(type2, &hop1)},
	}

	// Now that we have our set of sample records and types, we'll encode
	// them into the passed ExtraOpaqueData instance.
	var extraBytes ExtraOpaqueData
	if err := extraBytes.PackRecords(testRecordsProducers...); err != nil {
		t.Fatalf("unable to pack records: %v", err)
	}

	// We'll now simulate decoding these types _back_ into records on the
	// other side.
	newRecords := []tlv.RecordProducer{
		&recordProducer{tlv.MakePrimitiveRecord(type1, &channelType2)},
		&recordProducer{tlv.MakePrimitiveRecord(type2, &hop2)},
	}
	typeMap, err := extraBytes.ExtractRecords(newRecords...)
	require.NoError(t, err, "unable to extract record")

	// We should find that the new backing values have been populated with
	// the proper value.
	switch {
	case channelType1 != channelType2:
		t.Fatalf("wrong record for channel type: expected %v, got %v",
			channelType1, channelType2)

	case hop1 != hop2:
		t.Fatalf("wrong record for hop: expected %v, got %v", hop1,
			hop2)
	}

	// Both types we created above should be found in the type map.
	if _, ok := typeMap[type1]; !ok {
		t.Fatalf("type1 not found in typeMap")
	}
	if _, ok := typeMap[type2]; !ok {
		t.Fatalf("type2 not found in typeMap")
	}
}

// TestPackRecords tests that we're able to pack a set of records into an
// ExtraOpaqueData instance, and then extract them back out. Crucially, we'll
// ensure that records can be packed in any order, and we'll ensure that the
// unpacked records are valid.
func TestPackRecords(t *testing.T) {
	t.Parallel()

	// Create an empty ExtraOpaqueData instance.
	extraBytes := ExtraOpaqueData{}

	var (
		// Record type 1.
		recordBytes1 = []byte("recordBytes1")
		tlvRecord1   = tlv.NewPrimitiveRecord[tlv.TlvType1](
			recordBytes1,
		)

		// Record type 2.
		recordBytes2 = []byte("recordBytes2")
		tlvRecord2   = tlv.NewPrimitiveRecord[tlv.TlvType2](
			recordBytes2,
		)

		// Record type 3.
		recordBytes3 = []byte("recordBytes3")
		tlvRecord3   = tlv.NewPrimitiveRecord[tlv.TlvType3](
			recordBytes3,
		)
	)

	// Pack records 1 and 2 into the ExtraOpaqueData instance.
	err := extraBytes.PackRecords(
		[]tlv.RecordProducer{&tlvRecord1, &tlvRecord2}...,
	)
	require.NoError(t, err)

	// Examine the records that were packed into the ExtraOpaqueData.
	extractedRecords, err := extraBytes.ExtractRecords()
	require.NoError(t, err)

	require.Equal(t, 2, len(extractedRecords))
	require.Equal(t, recordBytes1, extractedRecords[tlvType1.TypeVal()])
	require.Equal(t, recordBytes2, extractedRecords[tlvType2.TypeVal()])

	// Pack records 1, 2, and 3 into the ExtraOpaqueData instance.
	err = extraBytes.PackRecords(
		[]tlv.RecordProducer{&tlvRecord3, &tlvRecord1, &tlvRecord2}...,
	)
	require.NoError(t, err)

	// Examine the records that were packed into the ExtraOpaqueData.
	extractedRecords, err = extraBytes.ExtractRecords()
	require.NoError(t, err)

	require.Equal(t, 3, len(extractedRecords))
	require.Equal(t, recordBytes1, extractedRecords[tlvType1.TypeVal()])
	require.Equal(t, recordBytes2, extractedRecords[tlvType2.TypeVal()])
	require.Equal(t, recordBytes3, extractedRecords[tlvType3.TypeVal()])
}

type dummyRecordProducer struct {
	typ           tlv.Type
	scratchValue  []byte
	expectedValue []byte
}

func (d *dummyRecordProducer) Record() tlv.Record {
	return tlv.MakePrimitiveRecord(d.typ, &d.scratchValue)
}

// TestExtraOpaqueData tests that we're able to properly encode/decode an
// ExtraOpaqueData instance.
func TestExtraOpaqueData(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		types         tlv.TypeMap
		expectedData  ExtraOpaqueData
		expectedTypes tlv.TypeMap
		decoders      []tlv.RecordProducer
	}{
		{
			name:          "empty map",
			expectedTypes: tlv.TypeMap{},
			expectedData:  make([]byte, 0),
		},
		{
			name: "single record",
			types: tlv.TypeMap{
				tlvType1.TypeVal(): []byte{1, 2, 3},
			},
			expectedData: ExtraOpaqueData{
				0x01, 0x03, 1, 2, 3,
			},
			expectedTypes: tlv.TypeMap{
				tlvType1.TypeVal(): []byte{1, 2, 3},
			},
			decoders: []tlv.RecordProducer{
				&dummyRecordProducer{
					typ:           tlvType1.TypeVal(),
					expectedValue: []byte{1, 2, 3},
				},
			},
		},
		{
			name: "multiple records",
			types: tlv.TypeMap{
				tlvType2.TypeVal(): []byte{4, 5, 6},
				tlvType1.TypeVal(): []byte{1, 2, 3},
			},
			expectedData: ExtraOpaqueData{
				0x01, 0x03, 1, 2, 3,
				0x02, 0x03, 4, 5, 6,
			},
			expectedTypes: tlv.TypeMap{
				tlvType1.TypeVal(): []byte{1, 2, 3},
				tlvType2.TypeVal(): []byte{4, 5, 6},
			},
			decoders: []tlv.RecordProducer{
				&dummyRecordProducer{
					typ:           tlvType1.TypeVal(),
					expectedValue: []byte{1, 2, 3},
				},
				&dummyRecordProducer{
					typ:           tlvType2.TypeVal(),
					expectedValue: []byte{4, 5, 6},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// First, test the constructor.
			opaqueData, err := NewExtraOpaqueData(tc.types)
			require.NoError(t, err)

			require.Equal(t, tc.expectedData, opaqueData)

			// Now encode/decode.
			var b bytes.Buffer
			err = opaqueData.Encode(&b)
			require.NoError(t, err)

			var decoded ExtraOpaqueData
			err = decoded.Decode(&b)
			require.NoError(t, err)

			require.Equal(t, opaqueData, decoded)

			// Now RecordProducers/PackRecords.
			producers, err := opaqueData.RecordProducers()
			require.NoError(t, err)

			var packed ExtraOpaqueData
			err = packed.PackRecords(producers...)
			require.NoError(t, err)

			// PackRecords returns nil vs. an empty slice if there
			// are no records. We need to handle this case
			// separately.
			if len(producers) == 0 {
				// Make sure the packed data is empty.
				require.Empty(t, packed)

				// Now change it to an empty slice for the
				// comparison below.
				packed = make([]byte, 0)
			}
			require.Equal(t, opaqueData, packed)

			// ExtractRecords with an empty set of record producers
			// should return the original type map.
			extracted, err := opaqueData.ExtractRecords()
			require.NoError(t, err)

			require.Equal(t, tc.expectedTypes, extracted)

			if len(tc.decoders) == 0 {
				return
			}

			// ExtractRecords with a set of record producers should
			// only return the types that weren't in the passed-in
			// set of producers.
			extracted, err = opaqueData.ExtractRecords(
				tc.decoders...,
			)
			require.NoError(t, err)

			for parsedType := range tc.expectedTypes {
				remainder, ok := extracted[parsedType]
				require.True(t, ok)
				require.Nil(t, remainder)
			}

			for _, dec := range tc.decoders {
				//nolint:forcetypeassert
				dec := dec.(*dummyRecordProducer)
				require.Equal(
					t, dec.expectedValue, dec.scratchValue,
				)
			}
		})
	}
}

// TestExtractAndMerge tests that the ParseAndExtractCustomRecords and
// MergeAndEncode functions work as expected.
func TestExtractAndMerge(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		knownRecords  []tlv.RecordProducer
		extraData     ExtraOpaqueData
		customRecords CustomRecords
		expectedErr   string
		expectEncoded []byte
	}{
		{
			name: "invalid custom record",
			customRecords: CustomRecords{
				123: []byte("invalid"),
			},
			expectedErr: "custom records validation error",
		},
		{
			name: "empty everything",
		},
		{
			name: "just extra data",
			extraData: ExtraOpaqueData{
				0x01, 0x03, 1, 2, 3,
				0x02, 0x03, 4, 5, 6,
			},
			expectEncoded: []byte{
				0x01, 0x03, 1, 2, 3,
				0x02, 0x03, 4, 5, 6,
			},
		},
		{
			name: "extra data with known record",
			extraData: ExtraOpaqueData{
				0x04, 0x03, 4, 4, 4,
				0x05, 0x03, 5, 5, 5,
			},
			knownRecords: []tlv.RecordProducer{
				&dummyRecordProducer{
					typ:           tlvType1.TypeVal(),
					scratchValue:  []byte{1, 2, 3},
					expectedValue: []byte{1, 2, 3},
				},
				&dummyRecordProducer{
					typ:           tlvType2.TypeVal(),
					scratchValue:  []byte{4, 5, 6},
					expectedValue: []byte{4, 5, 6},
				},
			},
			expectEncoded: []byte{
				0x01, 0x03, 1, 2, 3,
				0x02, 0x03, 4, 5, 6,
				0x04, 0x03, 4, 4, 4,
				0x05, 0x03, 5, 5, 5,
			},
		},
		{
			name: "extra data and custom records with known record",
			extraData: ExtraOpaqueData{
				0x04, 0x03, 4, 4, 4,
				0x05, 0x03, 5, 5, 5,
			},
			customRecords: CustomRecords{
				MinCustomRecordsTlvType + 1: []byte{99, 99, 99},
			},
			knownRecords: []tlv.RecordProducer{
				&dummyRecordProducer{
					typ:           tlvType1.TypeVal(),
					scratchValue:  []byte{1, 2, 3},
					expectedValue: []byte{1, 2, 3},
				},
				&dummyRecordProducer{
					typ:           tlvType2.TypeVal(),
					scratchValue:  []byte{4, 5, 6},
					expectedValue: []byte{4, 5, 6},
				},
			},
			expectEncoded: []byte{
				0x01, 0x03, 1, 2, 3,
				0x02, 0x03, 4, 5, 6,
				0x04, 0x03, 4, 4, 4,
				0x05, 0x03, 5, 5, 5,
				0xfe, 0x0, 0x1, 0x0, 0x1, 0x3, 0x63, 0x63, 0x63,
			},
		},
		{
			name: "duplicate records",
			extraData: ExtraOpaqueData{
				0x01, 0x03, 4, 4, 4,
				0x05, 0x03, 5, 5, 5,
			},
			customRecords: CustomRecords{
				MinCustomRecordsTlvType + 1: []byte{99, 99, 99},
			},
			knownRecords: []tlv.RecordProducer{
				&dummyRecordProducer{
					typ:           tlvType1.TypeVal(),
					scratchValue:  []byte{1, 2, 3},
					expectedValue: []byte{1, 2, 3},
				},
				&dummyRecordProducer{
					typ:           tlvType2.TypeVal(),
					scratchValue:  []byte{4, 5, 6},
					expectedValue: []byte{4, 5, 6},
				},
			},
			expectedErr: "duplicate record type: 1",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := MergeAndEncode(
				tc.knownRecords, tc.extraData, tc.customRecords,
			)

			if tc.expectedErr != "" {
				require.ErrorContains(t, err, tc.expectedErr)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expectEncoded, encoded)

			// Clear all the scratch values, to make sure they're
			// decoded from the data again.
			for _, dec := range tc.knownRecords {
				//nolint:forcetypeassert
				dec := dec.(*dummyRecordProducer)
				dec.scratchValue = nil
			}

			pCR, pKR, pED, err := ParseAndExtractCustomRecords(
				encoded, tc.knownRecords...,
			)
			require.NoError(t, err)

			require.Equal(t, tc.customRecords, pCR)
			require.Equal(t, tc.extraData, pED)

			for _, dec := range tc.knownRecords {
				//nolint:forcetypeassert
				dec := dec.(*dummyRecordProducer)
				require.Equal(
					t, dec.expectedValue, dec.scratchValue,
				)

				require.Contains(t, pKR, dec.typ)
			}
		})
	}
}
