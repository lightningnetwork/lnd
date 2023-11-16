package lnencrypt

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// TestEncryptDecryptPayload tests that given a static key, we're able to
// properly decrypt and encrypted payload. We also test that we'll reject a
// ciphertext that has been modified.
func TestEncryptDecryptPayload(t *testing.T) {
	t.Parallel()

	payloadCases := []struct {
		// plaintext is the string that we'll be encrypting.
		plaintext []byte

		// mutator allows a test case to modify the ciphertext before
		// we attempt to decrypt it.
		mutator func(*[]byte)

		// valid indicates if this test should pass or fail.
		valid bool
	}{
		// Proper payload, should decrypt.
		{
			plaintext: []byte("payload test plain text"),
			mutator:   nil,
			valid:     true,
		},

		// Mutator modifies cipher text, shouldn't decrypt.
		{
			plaintext: []byte("payload test plain text"),
			mutator: func(p *[]byte) {
				// Flip a byte in the payload to render it
				// invalid.
				(*p)[0] ^= 1
			},
			valid: false,
		},

		// Cipher text is too small, shouldn't decrypt.
		{
			plaintext: []byte("payload test plain text"),
			mutator: func(p *[]byte) {
				// Modify the cipher text to be zero length.
				*p = []byte{}
			},
			valid: false,
		},
	}

	keyRing := &MockKeyRing{}
	keyRingEnc, err := KeyRingEncrypter(keyRing)
	require.NoError(t, err)

	_, pubKey := btcec.PrivKeyFromBytes([]byte{0x01, 0x02, 0x03, 0x04})

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	privKeyEnc, err := ECDHEncrypter(privKey, pubKey)
	require.NoError(t, err)

	for _, payloadCase := range payloadCases {
		payloadCase := payloadCase
		for _, enc := range []*Encrypter{keyRingEnc, privKeyEnc} {
			enc := enc

			// First, we'll encrypt the passed payload with our
			// scheme.
			var cipherBuffer bytes.Buffer
			err = enc.EncryptPayloadToWriter(
				payloadCase.plaintext, &cipherBuffer,
			)
			require.NoError(t, err)

			// If we have a mutator, then we'll wrong the mutator
			// over the cipher text, then reset the main buffer and
			// re-write the new cipher text.
			if payloadCase.mutator != nil {
				cipherText := cipherBuffer.Bytes()

				payloadCase.mutator(&cipherText)

				cipherBuffer.Reset()
				cipherBuffer.Write(cipherText)
			}

			plaintext, err := enc.DecryptPayloadFromReader(
				&cipherBuffer,
			)

			if !payloadCase.valid {
				require.Error(t, err)

				continue
			}

			require.NoError(t, err)
			require.Equal(
				t, plaintext, payloadCase.plaintext,
			)
		}
	}
}

// TestInvalidKeyGeneration tests that key generation fails when deriving the
// key fails.
func TestInvalidKeyGeneration(t *testing.T) {
	t.Parallel()

	_, err := KeyRingEncrypter(&MockKeyRing{true})
	require.Error(t, err)
}
