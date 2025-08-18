package lnwire

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// TestChannelReestablishEncodeDecode tests that a raw byte stream can be
// decoded, then re-encoded to the same exact byte stream.
func TestChannelReestablishEncodeDecode(t *testing.T) {
	t.Parallel()

	// Create a new private key and its corresponding public key.
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pk := priv.PubKey()

	// Create a sample ChannelReestablish message.
	var rawBytes []byte

	// ChanID
	rawBytes = append(rawBytes, make([]byte, 32)...)

	// NextLocalCommitHeight
	rawBytes = append(rawBytes, []byte{0, 0, 0, 0, 0, 0, 0, 1}...)

	// RemoteCommitTailHeight
	rawBytes = append(rawBytes, []byte{0, 0, 0, 0, 0, 0, 0, 2}...)

	// LastRemoteCommitSecret
	rawBytes = append(rawBytes, make([]byte, 32)...)

	// LocalUnrevokedCommitPoint
	rawBytes = append(rawBytes, pk.SerializeCompressed()...)

	// Add TLV data, including known and unknown records.
	tlvData := []byte{
		// Unknown odd-type TLV record.
		0x3,        // type
		0x2,        // length
		0xab, 0xcd, // value

		// LocalNonce (type 4).
		0x04, // type
		0x42, // length (66)
		// value (66 bytes)
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00,

		// DynHeight (type 20).
		0x14,                                           // type
		0x08,                                           // length
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, // value

		// Another unknown odd-type TLV record at the end.
		0x6f,       // type
		0x2,        // length
		0x79, 0x79, // value
	}
	rawBytes = append(rawBytes, tlvData...)

	// Now, create a new empty message and decode the raw bytes into it.
	msg := &ChannelReestablish{}
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
