package chanbackup

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/keychain"
)

var (
	testWalletPrivKey = []byte{
		0x2b, 0xd8, 0x06, 0xc9, 0x7f, 0x0e, 0x00, 0xaf,
		0x1a, 0x1f, 0xc3, 0x32, 0x8f, 0xa7, 0x63, 0xa9,
		0x26, 0x97, 0x23, 0xc8, 0xdb, 0x8f, 0xac, 0x4f,
		0x93, 0xaf, 0x71, 0xdb, 0x18, 0x6d, 0x6e, 0x90,
	}
)

type mockKeyRing struct {
	fail bool
}

func (m *mockKeyRing) DeriveNextKey(keyFam keychain.KeyFamily) (keychain.KeyDescriptor, error) {
	return keychain.KeyDescriptor{}, nil
}
func (m *mockKeyRing) DeriveKey(keyLoc keychain.KeyLocator) (keychain.KeyDescriptor, error) {
	if m.fail {
		return keychain.KeyDescriptor{}, fmt.Errorf("fail")
	}

	_, pub := btcec.PrivKeyFromBytes(btcec.S256(), testWalletPrivKey)
	return keychain.KeyDescriptor{
		PubKey: pub,
	}, nil
}

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

	keyRing := &mockKeyRing{}

	for i, payloadCase := range payloadCases {
		var cipherBuffer bytes.Buffer

		// First, we'll encrypt the passed payload with our scheme.
		payloadReader := bytes.NewBuffer(payloadCase.plaintext)
		err := encryptPayloadToWriter(
			*payloadReader, &cipherBuffer, keyRing,
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

		plaintext, err := decryptPayloadFromReader(&cipherBuffer, keyRing)

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

// TestInvalidKeyEncryption tests that encryption fails if we're unable to
// obtain a valid key.
func TestInvalidKeyEncryption(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	err := encryptPayloadToWriter(b, &b, &mockKeyRing{true})
	if err == nil {
		t.Fatalf("expected error due to fail key gen")
	}
}

// TestInvalidKeyDecrytion tests that decryption fails if we're unable to
// obtain a valid key.
func TestInvalidKeyDecrytion(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	_, err := decryptPayloadFromReader(&b, &mockKeyRing{true})
	if err == nil {
		t.Fatalf("expected error due to fail key gen")
	}
}
