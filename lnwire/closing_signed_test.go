package lnwire

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestClosingSignedEncodeDecode tests that a raw byte stream can be
// decoded, then re-encoded to the same exact byte stream.
func TestClosingSignedEncodeDecode(t *testing.T) {
	t.Parallel()

	// Create a sample ClosingSigned message.
	var rawBytes []byte

	// ChannelID
	rawBytes = append(rawBytes, make([]byte, 32)...)

	// FeeSatoshis
	rawBytes = append(rawBytes, []byte{0, 0, 0, 0, 0, 0, 0, 1}...)

	// Signature
	rawBytes = append(rawBytes, make([]byte, 64)...)

	// Add TLV data, including known and unknown records.
	tlvData := []byte{
		// Unknown odd-type TLV record.
		0x3,        // type
		0x2,        // length
		0xab, 0xcd, // value

		// PartialSig (known, type 6)
		6,  // type
		32, // length
		// 32 bytes of dummy signature data
		0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44,
		0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44,
		0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44,
		0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44,

		// Another unknown odd-type TLV record at the end.
		0x6f,       // type
		0x2,        // length
		0x79, 0x79, // value
	}
	rawBytes = append(rawBytes, tlvData...)

	// Now, create a new empty message and decode the raw bytes into it.
	msg := &ClosingSigned{}
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
