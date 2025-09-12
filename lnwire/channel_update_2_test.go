package lnwire

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestChanUpdate2EncodeDecode tests the encoding and decoding of the
// ChannelUpdate2 message using hardcoded byte slices.
func TestChanUpdate2EncodeDecode(t *testing.T) {
	t.Parallel()

	// We'll create a raw byte stream that represents a valid ChannelUpdate2
	// message. This includes the signature and a TLV stream with both known
	// and unknown records.
	rawBytes := []byte{
		// ChainHash record (optional, not mainnet).
		0x0,  // type.
		0x20, // length.
		0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1,
		0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1,
		0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1, 0x1,

		// ShortChannelID record.
		0x2,                                    // type.
		0x8,                                    // length.
		0x0, 0x0, 0x1, 0x0, 0x0, 0x2, 0x0, 0x3, // value.

		// BlockHeight record.
		0x4,                // type.
		0x4,                // length.
		0x0, 0x0, 0x1, 0x0, // value.

		// DisabledFlags record.
		0x6, // type.
		0x1, // length.
		0x1, // value.

		// SecondPeer record.
		0x8, // type.
		0x0, // length.

		// Unknown odd-type TLV record.
		0x9,        // type.
		0x2,        // length.
		0xab, 0xcd, // value.

		// CLTVExpiryDelta record.
		0xa,       // type.
		0x2,       // length.
		0x0, 0x10, // value.

		// HTLCMinimumMsat record.
		0xc,                        // type.
		0x5,                        // length.
		0xfe, 0x0, 0xf, 0x42, 0x40, // value (BigSize: 1_000_000).

		// HTLCMaximumMsat record.
		0xe,                        // type.
		0x5,                        // length.
		0xfe, 0x0, 0xf, 0x42, 0x40, // value (BigSize: 1_000_000).

		// FeeBaseMsat record.
		0x10,               // type.
		0x4,                // length.
		0x0, 0x0, 0x1, 0x0, // value.

		// FeeProportionalMillionths record.
		0x12,               // type.
		0x4,                // length.
		0x0, 0x0, 0x1, 0x0, // value.

		// Extra Opaque Data - Unknown Record.
		0x14,       // type.
		0x2,        // length.
		0x79, 0x79, // value.

		// Signature.
		0xa0, // type.
		0x40, // length.
		0x0, 0x1, 0x2, 0x3, 0x4, 0x5, 0x6, 0x7, 0x8, 0x9, 0xa, 0xb,
		0xc, 0xd, 0xe, 0xf, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16,
		0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
		0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x29, 0x2a,
		0x2b, 0x2c, 0x2d, 0x2e, 0x2f, 0x30, 0x31, 0x32, 0x33, 0x34,
		0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e,
		0x3f, // value
	}

	secondSignedRangeType := new(bytes.Buffer)
	var buf [8]byte
	err := tlv.WriteVarInt(
		secondSignedRangeType, pureTLVSignedSecondRangeStart+1, &buf,
	)
	require.NoError(t, err)
	rawBytes = append(rawBytes, secondSignedRangeType.Bytes()...) // type.
	rawBytes = append(rawBytes, []byte{
		0x02,       // length.
		0x79, 0x79, // value.
	}...)

	// Now, create a new empty message and decode the raw bytes into it.
	msg := &ChannelUpdate2{}
	r := bytes.NewReader(rawBytes)
	err = msg.Decode(r, 0)
	require.NoError(t, err)

	// Next, encode the message back into a new byte buffer.
	var b bytes.Buffer
	err = msg.Encode(&b, 0)
	require.NoError(t, err)

	// The re-encoded bytes should be exactly the same as the original raw
	// bytes.
	require.Equal(t, rawBytes, b.Bytes())
}
