package lnwire

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// testCaseUpdateFulfill is a test case for the UpdateFulfillHTLC message.
type testCaseUpdateFulfill struct {
	// Msg is the message to be encoded and decoded.
	Msg UpdateFulfillHTLC

	// ExpectEncodeError is a flag that indicates whether we expect the
	// encoding of the message to fail.
	ExpectEncodeError bool
}

// generateTestCases generates a set of UpdateFulfillHTLC message test cases.
func generateUpdateFulfillTestCases(t *testing.T) []testCaseUpdateFulfill {
	// Firstly, we'll set basic values for the message fields.
	//
	// Generate random channel ID.
	chanIDBytes, err := generateRandomBytes(32)
	require.NoError(t, err)

	var chanID ChannelID
	copy(chanID[:], chanIDBytes)

	// Generate random payment preimage.
	paymentPreimageBytes, err := generateRandomBytes(32)
	require.NoError(t, err)

	var paymentPreimage [32]byte
	copy(paymentPreimage[:], paymentPreimageBytes)

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

	return []testCaseUpdateFulfill{
		{
			Msg: UpdateFulfillHTLC{
				ChanID:          chanID,
				ID:              42,
				PaymentPreimage: paymentPreimage,
			},
		},
		{
			Msg: UpdateFulfillHTLC{
				ChanID:          chanID,
				ID:              42,
				PaymentPreimage: paymentPreimage,
				CustomRecords:   customRecords,
				ExtraData:       extraData,
			},
		},
	}
}

// TestUpdateFulfillHtlcEncodeDecode tests UpdateFulfillHTLC message encoding
// and decoding for all supported field values.
func TestUpdateFulfillHtlcEncodeDecode(t *testing.T) {
	t.Parallel()

	// Generate test cases.
	testCases := generateUpdateFulfillTestCases(t)

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
			var actualMsg UpdateFulfillHTLC
			decodeReader := bytes.NewReader(buf.Bytes())
			err = actualMsg.Decode(decodeReader, 0)
			require.NoError(t, err)

			// Compare the two messages to ensure equality.
			require.Equal(t, tc.Msg, actualMsg)
		})
	}
}
