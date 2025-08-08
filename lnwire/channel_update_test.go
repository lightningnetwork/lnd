package lnwire

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestChannelUpdate1EncodeDecode tests that a raw byte stream can be
// decoded, then re-encoded to the same exact byte stream.
func TestChannelUpdate1EncodeDecode(t *testing.T) {
	t.Parallel()

	// Create a sample ChannelUpdate1 message.
	var rawBytes []byte

	// Signature
	rawBytes = append(rawBytes, make([]byte, 64)...)

	// ChainHash
	rawBytes = append(rawBytes, make([]byte, 32)...)

	// ShortChannelID
	rawBytes = append(rawBytes, []byte{0, 0, 0, 0, 0, 0, 0, 1}...)

	// Timestamp
	rawBytes = append(rawBytes, []byte{0, 0, 0, 2}...)

	// MessageFlags
	rawBytes = append(rawBytes, 1)

	// ChannelFlags
	rawBytes = append(rawBytes, 1)

	// TimeLockDelta
	rawBytes = append(rawBytes, []byte{0, 3}...)

	// HtlcMinimumMsat
	rawBytes = append(rawBytes, []byte{0, 0, 0, 0, 0, 0, 0, 4}...)

	// BaseFee
	rawBytes = append(rawBytes, []byte{0, 0, 0, 5}...)

	// FeeRate
	rawBytes = append(rawBytes, []byte{0, 0, 0, 6}...)

	// HtlcMaximumMsat
	rawBytes = append(rawBytes, []byte{0, 0, 0, 0, 0, 0, 0, 7}...)

	// Add TLV data, including known and unknown records.
	tlvData := []byte{
		// Unknown odd-type TLV record.
		0x3,        // type
		0x2,        // length
		0xab, 0xcd, // value

		// Another unknown odd-type TLV record at the end.
		0x6f,       // type
		0x2,        // length
		0x79, 0x79, // value

		// InboundFee (known, type 55555)
		0xfd, 0xd9, 0x03, // type
		8,          // length
		0x00, 0x00, 0x00, 0x0a, // BaseFee = 10
		0x00, 0x00, 0x00, 0x14, // FeeRate = 20
	}
	rawBytes = append(rawBytes, tlvData...)

	// Now, create a new empty message and decode the raw bytes into it.
	msg := &ChannelUpdate1{}
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
