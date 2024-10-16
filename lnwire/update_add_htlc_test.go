package lnwire

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// testCase is a test case for the UpdateAddHTLC message.
type testCase struct {
	// Msg is the message to be encoded and decoded.
	Msg UpdateAddHTLC

	// ExpectEncodeError is a flag that indicates whether we expect the
	// encoding of the message to fail.
	ExpectEncodeError bool
}

// generateTestCases generates a set of UpdateAddHTLC message test cases.
func generateTestCases(t *testing.T) []testCase {
	// Firstly, we'll set basic values for the message fields.
	//
	// Generate random channel ID.
	chanIDBytes, err := generateRandomBytes(32)
	require.NoError(t, err)

	var chanID ChannelID
	copy(chanID[:], chanIDBytes)

	// Generate random payment hash.
	paymentHashBytes, err := generateRandomBytes(32)
	require.NoError(t, err)

	var paymentHash [32]byte
	copy(paymentHash[:], paymentHashBytes)

	// Generate random onion blob.
	onionBlobBytes, err := generateRandomBytes(OnionPacketSize)
	require.NoError(t, err)

	var onionBlob [OnionPacketSize]byte
	copy(onionBlob[:], onionBlobBytes)

	// Define the blinding point.
	blinding, err := pubkeyFromHex(
		"0228f2af0abe322403480fb3ee172f7f1601e67d1da6cad40b54c4468d4" +
			"8236c39",
	)
	require.NoError(t, err)

	blindingPoint := tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[BlindingPointTlvType](blinding),
	)

	// Define custom records.
	recordKey1 := uint64(MinCustomRecordsTlvType + 1)
	recordValue1, err := generateRandomBytes(10)
	require.NoError(t, err)

	recordKey2 := uint64(MinCustomRecordsTlvType + 2)
	recordValue2, err := generateRandomBytes(10)
	require.NoError(t, err)

	customRecords := CustomRecords{
		recordKey1: recordValue1,
		recordKey2: recordValue2,
	}

	// Construct an instance of extra data that contains records with TLV
	// types below the minimum custom records threshold and that lack
	// corresponding fields in the message struct. Content should persist in
	// the extra data field after encoding and decoding.
	var (
		recordBytes45 = []byte("recordBytes45")
		tlvRecord45   = tlv.NewPrimitiveRecord[tlv.TlvType45](
			recordBytes45,
		)

		recordBytes55 = []byte("recordBytes55")
		tlvRecord55   = tlv.NewPrimitiveRecord[tlv.TlvType55](
			recordBytes55,
		)
	)

	var extraData ExtraOpaqueData
	err = extraData.PackRecords(
		[]tlv.RecordProducer{&tlvRecord45, &tlvRecord55}...,
	)
	require.NoError(t, err)

	invalidCustomRecords := CustomRecords{
		MinCustomRecordsTlvType - 1: recordValue1,
	}

	return []testCase{
		{
			Msg: UpdateAddHTLC{
				ChanID:        chanID,
				ID:            42,
				Amount:        MilliSatoshi(1000),
				PaymentHash:   paymentHash,
				Expiry:        43,
				OnionBlob:     onionBlob,
				BlindingPoint: blindingPoint,
				CustomRecords: customRecords,
				ExtraData:     extraData,
			},
		},
		// Add a test case where the blinding point field is not
		// populated.
		{
			Msg: UpdateAddHTLC{
				ChanID:        chanID,
				ID:            42,
				Amount:        MilliSatoshi(1000),
				PaymentHash:   paymentHash,
				Expiry:        43,
				OnionBlob:     onionBlob,
				CustomRecords: customRecords,
			},
		},
		// Add a test case where the custom records field is not
		// populated.
		{
			Msg: UpdateAddHTLC{
				ChanID:        chanID,
				ID:            42,
				Amount:        MilliSatoshi(1000),
				PaymentHash:   paymentHash,
				Expiry:        43,
				OnionBlob:     onionBlob,
				BlindingPoint: blindingPoint,
			},
		},
		// Add a case where the custom records are invalid.
		{
			Msg: UpdateAddHTLC{
				ChanID:        chanID,
				ID:            42,
				Amount:        MilliSatoshi(1000),
				PaymentHash:   paymentHash,
				Expiry:        43,
				OnionBlob:     onionBlob,
				BlindingPoint: blindingPoint,
				CustomRecords: invalidCustomRecords,
			},
			ExpectEncodeError: true,
		},
	}
}

// TestUpdateAddHtlcEncodeDecode tests UpdateAddHTLC message encoding and
// decoding for all supported field values.
func TestUpdateAddHtlcEncodeDecode(t *testing.T) {
	t.Parallel()

	// Generate test cases.
	testCases := generateTestCases(t)

	// Execute test cases.
	for tcIdx, tc := range testCases {
		t.Run(fmt.Sprintf("testcase-%d", tcIdx), func(t *testing.T) {
			// Encode test case message.
			var buf bytes.Buffer
			err := tc.Msg.Encode(&buf, 0)

			// Check if we expect an encoding error.
			if tc.ExpectEncodeError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)

			// Decode the encoded message bytes message.
			var actualMsg UpdateAddHTLC
			decodeReader := bytes.NewReader(buf.Bytes())
			err = actualMsg.Decode(decodeReader, 0)
			require.NoError(t, err)

			// Compare the two messages to ensure equality.
			require.Equal(t, tc.Msg, actualMsg)
		})
	}
}
