package lnwire

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDynProposeEncodeDecode checks that the Encode and Decode methods work as
// expected.
func TestDynProposeEncodeDecode(t *testing.T) {
	t.Parallel()

	// Generate random channel ID.
	chanIDBytes, err := generateRandomBytes(32)
	require.NoError(t, err)

	var chanID ChannelID
	copy(chanID[:], chanIDBytes)

	// Create test data for the TLVs. The actual value doesn't matter, as we
	// only care about that the raw bytes can be decoded into a msg, and the
	// msg can be encoded into the exact same raw bytes.
	testTlvData := []byte{
		// DustLimit tlv.
		0x0,                        // type.
		0x5,                        // length.
		0xfe, 0x0, 0xf, 0x42, 0x40, // value (BigSize:  1_000_000).

		// ExtraData - unknown tlv record.
		//
		// NOTE: This record is optional and occupies the type 1.
		0x1,        // type.
		0x2,        // length.
		0x79, 0x79, // value.

		// MaxValueInFlight tlv.
		0x2,                        // type.
		0x5,                        // length.
		0xfe, 0x0, 0xf, 0x42, 0x40, // value (BigSize: 1_000_000).

		// HtlcMinimum tlv.
		0x4,                        // type.
		0x5,                        // length.
		0xfe, 0x0, 0xf, 0x42, 0x40, // value (BigSize: 1_000_000).
		//
		// ChannelReserve tlv.
		0x6,                        // type.
		0x5,                        // length.
		0xfe, 0x0, 0xf, 0x42, 0x40, // value (BigSize: 1_000_000).

		// CsvDelay tlv.
		0x8,      // type.
		0x2,      // length.
		0x0, 0x8, // value.

		// MaxAcceptedHTLCs tlv.
		0xa,      // type.
		0x2,      // length.
		0x0, 0x8, // value.

		// ChannelType tlv is empty.
		//
		// ExtraData - unknown tlv record.
		0x6f,       // type.
		0x2,        // length.
		0x79, 0x79, // value.
	}

	msg := &DynPropose{}

	rawBytes := make([]byte, 0, len(chanIDBytes)+len(testTlvData))
	rawBytes = append(rawBytes, chanIDBytes...)
	rawBytes = append(rawBytes, testTlvData...)

	r := bytes.NewBuffer(rawBytes)
	err = msg.Decode(r, 0)
	require.NoError(t, err)

	w := new(bytes.Buffer)
	err = msg.Encode(w, 0)
	require.NoError(t, err)

	require.Equal(t, rawBytes, w.Bytes())
}
