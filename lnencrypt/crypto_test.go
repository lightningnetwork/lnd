package lnencrypt

import (
	"bytes"
	"testing"

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
				// Flip a byte in the payload to render it invalid.
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

	for i, payloadCase := range payloadCases {
		var cipherBuffer bytes.Buffer
		encrypter, err := KeyRingEncrypter(keyRing)
		require.NoError(t, err)

		// First, we'll encrypt the passed payload with our scheme.
		err = encrypter.EncryptPayloadToWriter(
			payloadCase.plaintext, &cipherBuffer,
		)
		if err != nil {
			t.Fatalf("unable encrypt paylaod: %v", err)
		}

		// If we have a mutator, then we'll wrong the mutator over the
		// cipher text, then reset the main buffer and re-write the new
		// cipher text.
		if payloadCase.mutator != nil {
			cipherText := cipherBuffer.Bytes()

			payloadCase.mutator(&cipherText)

			cipherBuffer.Reset()
			cipherBuffer.Write(cipherText)
		}

		plaintext, err := encrypter.DecryptPayloadFromReader(
			&cipherBuffer,
		)

		switch {
		// If this was meant to be a valid decryption, but we failed,
		// then we'll return an error.
		case err != nil && payloadCase.valid:
			t.Fatalf("unable to decrypt valid payload case %v", i)

		// If this was meant to be an invalid decryption, and we didn't
		// fail, then we'll return an error.
		case err == nil && !payloadCase.valid:
			t.Fatalf("payload was invalid yet was able to decrypt")
		}

		// Only if this case was mean to be valid will we ensure the
		// resulting decrypted plaintext matches the original input.
		if payloadCase.valid &&
			!bytes.Equal(plaintext, payloadCase.plaintext) {
			t.Fatalf("#%v: expected %v, got %v: ", i,
				payloadCase.plaintext, plaintext)
		}
	}
}

// TestInvalidKeyGeneration tests that key generation fails when deriving the
// key fails.
func TestInvalidKeyGeneration(t *testing.T) {
	t.Parallel()

	_, err := KeyRingEncrypter(&MockKeyRing{true})
	if err == nil {
		t.Fatal("expected error due to fail key gen")
	}
}
