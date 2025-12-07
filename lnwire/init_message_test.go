package lnwire

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestInitEncodeDecode checks that we can encode and decode an Init message
// to and from a byte stream.
func TestInitEncodeDecode(t *testing.T) {
	t.Parallel()

	// These are the raw bytes that we expect to be generated from the
	// sample Init message.
	rawBytes := []byte{
		// GlobalFeatures
		0x00, 0x01, 0xc0,

		// Features
		0x00, 0x01, 0xc0,

		// ExtraData - unknown odd-type TLV record.
		0x6f,       // type (111)
		0x02,       // length
		0x79, 0x79, // value

		// ExtraData - custom TLV record.
		// TLV record for type 67676
		0xfe, 0x00, 0x01, 0x08, 0x6c, // type (67676)
		0x05,                         // length
		0x01, 0x02, 0x03, 0x04, 0x05, // value

		// ExtraData - custom TLV record.
		// TLV record for type 67777
		0xfe, 0x00, 0x01, 0x08, 0xc1, // type (67777)
		0x03,             // length
		0x01, 0x02, 0x03, // value
	}

	// Create a new empty message and decode the raw bytes into it.
	msg := &Init{}
	r := bytes.NewReader(rawBytes)
	err := msg.Decode(r, 0)
	require.NoError(t, err)

	require.NotNil(t, msg.GlobalFeatures)
	require.NotNil(t, msg.Features)
	require.NotNil(t, msg.CustomRecords)
	require.NotNil(t, msg.ExtraData)

	// Next, encode the message back into a new byte buffer.
	var b bytes.Buffer
	err = msg.Encode(&b, 0)
	require.NoError(t, err)

	// The re-encoded bytes should be exactly the same as the original raw
	// bytes.
	require.Equal(t, rawBytes, b.Bytes())
}
