package lnencrypt

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"golang.org/x/crypto/chacha20poly1305"
)

// baseEncryptionKeyLoc is the KeyLocator that we'll use to derive the base
// encryption key used for encrypting all payloads. We use this to then
// derive the actual key that we'll use for encryption. We do this
// rather than using the raw key, as we assume that we can't obtain the raw
// keys, and we don't want to require that the HSM know our target cipher for
// encryption.
//
// TODO(roasbeef): possibly unique encrypt?
var baseEncryptionKeyLoc = keychain.KeyLocator{
	Family: keychain.KeyFamilyBaseEncryption,
	Index:  0,
}

// EncrypterDecrypter is an interface representing an object that encrypts or
// decrypts data.
type EncrypterDecrypter interface {
	// EncryptPayloadToWriter attempts to write the set of provided bytes
	// into the passed io.Writer in an encrypted form.
	EncryptPayloadToWriter([]byte, io.Writer) error

	// DecryptPayloadFromReader attempts to decrypt the encrypted bytes
	// within the passed io.Reader instance using the key derived from
	// the passed keyRing.
	DecryptPayloadFromReader(io.Reader) ([]byte, error)
}

// Encrypter is a struct responsible for encrypting and decrypting data.
type Encrypter struct {
	encryptionKey []byte
}

// KeyRingEncrypter derives an encryption key to encrypt all our files that are
// written to disk and returns an Encrypter object holding the key.
//
// The key itself, is the sha2 of a base key that we get from the keyring. We
// derive the key this way as we don't force the HSM (or any future
// abstractions) to be able to derive and know of the cipher that we'll use
// within our protocol.
func KeyRingEncrypter(keyRing keychain.KeyRing) (*Encrypter, error) {
	//  key = SHA256(baseKey)
	baseKey, err := keyRing.DeriveKey(
		baseEncryptionKeyLoc,
	)
	if err != nil {
		return nil, err
	}

	encryptionKey := sha256.Sum256(
		baseKey.PubKey.SerializeCompressed(),
	)

	// TODO(roasbeef): throw back in ECDH?

	return &Encrypter{
		encryptionKey: encryptionKey[:],
	}, nil
}

// ECDHEncrypter derives an encryption key by performing an ECDH operation on
// the passed keys. The resulting key is used to encrypt or decrypt files with
// sensitive content.
func ECDHEncrypter(localKey *btcec.PrivateKey,
	remoteKey *btcec.PublicKey) (*Encrypter, error) {

	ecdh := keychain.PrivKeyECDH{
		PrivKey: localKey,
	}
	encryptionKey, err := ecdh.ECDH(remoteKey)
	if err != nil {
		return nil, fmt.Errorf("error deriving encryption key: %w", err)
	}

	return &Encrypter{
		encryptionKey: encryptionKey[:],
	}, nil
}

// EncryptPayloadToWriter attempts to write the set of provided bytes into the
// passed io.Writer in an encrypted form. We use a 24-byte chachapoly AEAD
// instance with a randomized nonce that's pre-pended to the final payload and
// used as associated data in the AEAD.
func (e Encrypter) EncryptPayloadToWriter(payload []byte,
	w io.Writer) error {

	// Before encryption, we'll initialize our cipher with the target
	// encryption key, and also read out our random 24-byte nonce we use
	// for encryption. Note that we use NewX, not New, as the latter
	// version requires a 12-byte nonce, not a 24-byte nonce.
	cipher, err := chacha20poly1305.NewX(e.encryptionKey)
	if err != nil {
		return err
	}
	var nonce [chacha20poly1305.NonceSizeX]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}

	// Finally, we encrypted the final payload, and write out our
	// ciphertext with nonce pre-pended.
	ciphertext := cipher.Seal(nil, nonce[:], payload, nonce[:])

	if _, err := w.Write(nonce[:]); err != nil {
		return err
	}
	if _, err := w.Write(ciphertext); err != nil {
		return err
	}

	return nil
}

// DecryptPayloadFromReader attempts to decrypt the encrypted bytes within the
// passed io.Reader instance using the key derived from the passed keyRing. For
// further details regarding the key derivation protocol, see the
// KeyRingEncrypter function.
func (e Encrypter) DecryptPayloadFromReader(payload io.Reader) ([]byte,
	error) {

	// Next, we'll read out the entire blob as we need to isolate the nonce
	// from the rest of the ciphertext.
	packedPayload, err := io.ReadAll(payload)
	if err != nil {
		return nil, err
	}
	if len(packedPayload) < chacha20poly1305.NonceSizeX {
		return nil, fmt.Errorf("payload size too small, must be at "+
			"least %v bytes", chacha20poly1305.NonceSizeX)
	}

	nonce := packedPayload[:chacha20poly1305.NonceSizeX]
	ciphertext := packedPayload[chacha20poly1305.NonceSizeX:]

	// Now that we have the cipher text and the nonce separated, we can go
	// ahead and decrypt the final blob so we can properly serialize.
	cipher, err := chacha20poly1305.NewX(e.encryptionKey)
	if err != nil {
		return nil, err
	}
	plaintext, err := cipher.Open(nil, nonce, ciphertext, nonce)
	if err != nil {
		return nil, err
	}

	return plaintext, nil
}
