package lnwire

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestAnnSigs2EncodeDecode tests the encoding and decoding of the
// AnnounceSignatures2 message using hardcoded byte slices.
func TestAnnSigs2EncodeDecode(t *testing.T) {
	t.Parallel()

	// We'll create a raw byte stream that represents a valid
	// AnnounceSignatures2 message with various known and unknown fields in
	// the signed TLV ranges.
	var rawBytes []byte

	// ChannelID.
	rawBytes = append(rawBytes, []byte{
		0x00, // type
		0x20, // length
	}...)
	rawBytes = append(rawBytes, make([]byte, 32)...) // value

	// ShortChannelID.
	rawBytes = append(rawBytes, []byte{
		0x02,                   // type
		0x08,                   // length
		0, 0, 1, 0, 0, 2, 0, 3, // value
	}...)

	// PartialSignature.
	rawBytes = append(rawBytes, []byte{
		0x04, // type
		0x20, // length
	}...)
	rawBytes = append(rawBytes, make([]byte, 32)...) // value

	// Extra field in the first signed range.
	rawBytes = append(rawBytes, []byte{
		0x30,       // type
		0x02,       // length
		0xab, 0xcd, // value
	}...)

	w := new(bytes.Buffer)
	var buf [8]byte
	err := tlv.WriteVarInt(w, pureTLVSignedSecondRangeStart+1, &buf)
	require.NoError(t, err)

	// Extra field in the second signed range.
	rawBytes = append(rawBytes, w.Bytes()...) // type
	rawBytes = append(rawBytes, []byte{
		0x02,       // length
		0x79, 0x79, // value
	}...)

	// Now, create a new empty message and decode the raw bytes into it.
	msg := &AnnounceSignatures2{}
	r := bytes.NewReader(rawBytes)
	err = msg.Decode(r, 0)
	require.NoError(t, err)

	// At this point, we expect 2 extra signed fields.
	require.Len(t, msg.ExtraSignedFields, 2)

	// Next, encode the message back into a new byte buffer.
	var b bytes.Buffer
	err = msg.Encode(&b, 0)
	require.NoError(t, err)

	// The re-encoded bytes should be exactly the same as the original raw
	// bytes.
	require.Equal(t, rawBytes, b.Bytes())
}
