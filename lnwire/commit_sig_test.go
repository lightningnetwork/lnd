package lnwire

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// testCase is a test case for the CommitSig message.
type commitSigTestCase struct {
	// Msg is the message to be encoded and decoded.
	Msg CommitSig

	// ExpectEncodeError is a flag that indicates whether we expect the
	// encoding of the message to fail.
	ExpectEncodeError bool
}

// generateCommitSigTestCases generates a set of CommitSig message test cases.
func generateCommitSigTestCases(t *testing.T) []commitSigTestCase {
	// Firstly, we'll set basic values for the message fields.
	//
	// Generate random channel ID.
	chanIDBytes, err := generateRandomBytes(32)
	require.NoError(t, err)

	var chanID ChannelID
	copy(chanID[:], chanIDBytes)

	// Generate random commit sig.
	commitSigBytes, err := generateRandomBytes(64)
	require.NoError(t, err)

	sig, err := NewSigFromSchnorrRawSignature(commitSigBytes)
	require.NoError(t, err)

	sigScalar := new(btcec.ModNScalar)
	sigScalar.SetByteSlice(sig.RawBytes())

	var nonce [musig2.PubNonceSize]byte
	copy(nonce[:], commitSigBytes)

	sigWithNonce := NewPartialSigWithNonce(nonce, *sigScalar)
	partialSig := MaybePartialSigWithNonce(sigWithNonce)

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

	return []commitSigTestCase{
		{
			Msg: CommitSig{
				ChanID:        chanID,
				CommitSig:     sig,
				PartialSig:    partialSig,
				CustomRecords: customRecords,
				ExtraData:     extraData,
			},
		},
		// Add a test case where the blinding point field is not
		// populated.
		{
			Msg: CommitSig{
				ChanID:        chanID,
				CommitSig:     sig,
				CustomRecords: customRecords,
			},
		},
		// Add a test case where the custom records field is not
		// populated.
		{
			Msg: CommitSig{
				ChanID:     chanID,
				CommitSig:  sig,
				PartialSig: partialSig,
			},
		},
		// Add a case where the custom records are invalid.
		{
			Msg: CommitSig{
				ChanID:        chanID,
				CommitSig:     sig,
				PartialSig:    partialSig,
				CustomRecords: invalidCustomRecords,
			},
			ExpectEncodeError: true,
		},
	}
}

// TestCommitSigEncodeDecode tests CommitSig message encoding and decoding for
// all supported field values.
func TestCommitSigEncodeDecodeFields(t *testing.T) {
	t.Parallel()

	// Generate test cases.
	testCases := generateCommitSigTestCases(t)

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
			var actualMsg CommitSig
			decodeReader := bytes.NewReader(buf.Bytes())
			err = actualMsg.Decode(decodeReader, 0)
			require.NoError(t, err)

			// The signature type isn't serialized.
			actualMsg.CommitSig.ForceSchnorr()

			// Compare the two messages to ensure equality.
			require.Equal(t, tc.Msg, actualMsg)
		})
	}
}

// TestCommitSigEncodeDecode tests that a raw byte stream can be decoded, then
// re-encoded to the same exact byte stream.
func TestCommitSigEncodeDecode(t *testing.T) {
	t.Parallel()

	// We'll create a raw byte stream that represents a valid CommitSig
	// message. This includes the fixed-size fields and a TLV stream with
	// both known and unknown records.
	var rawBytes []byte

	// ChanID
	rawBytes = append(rawBytes, make([]byte, 32)...)

	// CommitSig
	rawBytes = append(rawBytes, make([]byte, 64)...)

	// HtlcSigs
	rawBytes = append(rawBytes, []byte{0, 0}...)

	// Add TLV data, including known and unknown records.
	tlvData := []byte{
		// PartialSig record.
		2, 98,
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10,
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10,
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10,
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10,
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 1, 2, 3, 4, 5, 6, 7, 8,

		// Unknown odd record: Type=3, Length=1, Value=0.
		3, 1, 0,

		// CustomRecords: Type=65536, Length=1, Value=0.
		0xfe, 0x00, 0x01, 0x00, 0x00, 1, 0,
	}
	rawBytes = append(rawBytes, tlvData...)

	// Now, create a new empty message and decode the raw bytes into it.
	msg := &CommitSig{}
	r := bytes.NewReader(rawBytes)
	err := msg.Decode(r, 0)
	require.NoError(t, err)

	// Next, encode the message back into a new byte buffer.
	var b bytes.Buffer
	err = msg.Encode(&b, 0)
	require.NoError(t, err)

	// The re-encoded bytes should be exactly the same as the original raw
	// bytes.
	require.Equal(t, rawBytes, b.Bytes())
}
