package lnwire

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/stretchr/testify/require"
)

// makeNonce creates a test Musig2Nonce containing two valid compressed public
// keys for testing TLV encoding/decoding.
func makeNonce() Musig2Nonce {
	_, pub1 := btcec.PrivKeyFromBytes([]byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
		0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
	})
	_, pub2 := btcec.PrivKeyFromBytes([]byte{
		0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28,
		0x29, 0x2a, 0x2b, 0x2c, 0x2d, 0x2e, 0x2f, 0x30,
		0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38,
		0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e, 0x3f, 0x40,
	})

	var n Musig2Nonce
	copy(n[:33], pub1.SerializeCompressed())
	copy(n[33:], pub2.SerializeCompressed())

	return n
}

// TestMusig2NonceEncodeDecode tests that we're able to properly encode and
// decode Musig2Nonce within TLV streams.
func TestMusig2NonceEncodeDecode(t *testing.T) {
	t.Parallel()

	nonce := makeNonce()

	var extraData ExtraOpaqueData
	require.NoError(t, extraData.PackRecords(&nonce))

	var extractedNonce Musig2Nonce
	_, err := extraData.ExtractRecords(&extractedNonce)
	require.NoError(t, err)

	require.Equal(t, nonce, extractedNonce)
}

// TestMusig2NonceTypeDecodeInvalidLength ensures that decoding a Musig2Nonce
// TLV with an invalid length (anything other than 66 bytes) fails with an
// error.
func TestMusig2NonceTypeDecodeInvalidLength(t *testing.T) {
	t.Parallel()

	nonce := makeNonce()

	var extraData ExtraOpaqueData
	require.NoError(t, extraData.PackRecords(&nonce))

	// Corrupt the TLV length field to simulate malformed input.
	// Byte 1 contains the varint size encoding. Since 66 bytes fits into
	// a single varint byte, we can directly modify extraData[1].
	extraData[1] = musig2.PubNonceSize + 1

	var out Musig2Nonce
	_, err := extraData.ExtractRecords(&out)
	require.Error(t, err)

	extraData[1] = musig2.PubNonceSize - 1

	_, err = extraData.ExtractRecords(&out)
	require.Error(t, err)
}
