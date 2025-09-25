package lnwire

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/stretchr/testify/require"
)

// makeNonce creates a test Musig2Nonce with sequential byte values for testing
// TLV encoding/decoding.
func makeNonce() Musig2Nonce {
	var n Musig2Nonce
	for i := range n {
		n[i] = byte(i)
	}

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
