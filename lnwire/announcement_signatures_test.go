package lnwire

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAnnounceSignatures1EncodeDecode tests the encode and decode methods for
// the AnnounceSignatures1 message.
func TestAnnounceSignatures1EncodeDecode(t *testing.T) {
	t.Parallel()

	// We'll create a raw byte stream that represents a valid
	// AnnounceSignatures1 message. This includes the fixed-size fields and
	// a TLV stream with both known and unknown records.
	var rawBytes []byte

	// ChannelID.
	rawBytes = append(rawBytes, make([]byte, 32)...)

	// ShortChannelID.
	rawBytes = append(rawBytes, []byte{0, 0, 1, 0, 0, 2, 0, 3}...)

	// NodeSignature.
	rawBytes = append(rawBytes, make([]byte, 64)...)

	// BitcoinSignature.
	rawBytes = append(rawBytes, make([]byte, 64)...)

	// Extra Opaque Data.
	rawBytes = append(rawBytes, []byte{
		// Unknown odd-type TLV record.
		0x3,        // type
		0x2,        // length
		0xab, 0xcd, // value
	}...)

	// Extra Opaque Data.
	rawBytes = append(rawBytes, []byte{
		// Unknown odd-type TLV record.
		0x6f,       // type
		0x2,        // length
		0x79, 0x79, // value
	}...)

	// Now, create a new empty message and decode the raw bytes into it.
	msg := &AnnounceSignatures1{}
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
