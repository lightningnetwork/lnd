package lnwire

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestChannelAnnouncement1EncodeDecode asserts that a raw byte stream can be
// decoded, then re-encoded to the same exact byte stream.
func TestChannelAnnouncement1EncodeDecode(t *testing.T) {
	t.Parallel()

	// We'll create a raw byte stream that represents a valid
	// ChannelAnnouncement1 message. This includes valid values for all
	// fields and some extra opaque data to test for forward-compatibility.
	// The exact values are not important, only that they are of the
	// correct size.
	var rawBytes []byte
	rawBytes = append(rawBytes, make([]byte, 64)...) // NodeSig1
	rawBytes = append(rawBytes, make([]byte, 64)...) // NodeSig2
	rawBytes = append(rawBytes, make([]byte, 64)...) // BitcoinSig1
	rawBytes = append(rawBytes, make([]byte, 64)...) // BitcoinSig2
	rawBytes = append(rawBytes, []byte{
		0x0, 0x1, 0x2, // Features
	}...)
	rawBytes = append(rawBytes, make([]byte, 32)...) // ChainHash
	rawBytes = append(rawBytes, []byte{
		0x0, 0x0, 0x1, // BlockHeight
		0x0, 0x0, 0x2, // TxIndex
		0x0, 0x3, // TxPosition
	}...)
	rawBytes = append(rawBytes, make([]byte, 33)...) // NodeID1
	rawBytes = append(rawBytes, make([]byte, 33)...) // NodeID2
	rawBytes = append(rawBytes, make([]byte, 33)...) // BitcoinKey1
	rawBytes = append(rawBytes, make([]byte, 33)...) // BitcoinKey2
	rawBytes = append(rawBytes, []byte{
		// Extra Opaque Data - Unknown Record 1
		0x6d,       // type
		0x2,        // length
		0xab, 0xcd, // value

		// Extra Opaque Data - Unknown Record 2
		0x6f,       // type
		0x2,        // length
		0x79, 0x79, // value
	}...)

	// Now, create a new empty message and decode the raw bytes into it.
	msg := &ChannelAnnouncement1{}
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
