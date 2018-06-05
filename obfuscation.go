package sphinx

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"io"

	"github.com/btcsuite/btcd/btcec"
)

// onionEncrypt obfuscates the data with compliance with BOLT#4. As we use a
// stream cipher, calling onionEncrypt on an already encrypted piece of data
// will decrypt it.
func onionEncrypt(sharedSecret *Hash256, data []byte) []byte {

	p := make([]byte, len(data))

	ammagKey := generateKey("ammag", sharedSecret)
	streamBytes := generateCipherStream(ammagKey, uint(len(data)))
	xor(p, data, streamBytes)

	return p
}

// OnionErrorEncrypter is a struct that's used to implement onion error
// encryption as defined within BOLT0004.
type OnionErrorEncrypter struct {
	sharedSecret Hash256
}

// NewOnionErrorEncrypter creates new instance of the onion encrypter backed by
// the passed router, with encryption to be doing using the passed
// ephemeralKey.
func NewOnionErrorEncrypter(router *Router,
	ephemeralKey *btcec.PublicKey) (*OnionErrorEncrypter, error) {

	sharedSecret, err := router.generateSharedSecret(ephemeralKey)
	if err != nil {
		return nil, err
	}

	return &OnionErrorEncrypter{
		sharedSecret: sharedSecret,
	}, nil
}

// EncryptError is used to make data obfuscation using the generated shared
// secret.
//
// In context of Lightning Network is either used by the nodes in order to make
// initial obfuscation with the creation of the hmac or by the forwarding nodes
// for backward failure obfuscation of the onion failure blob. By obfuscating
// the onion failure on every node in the path we are adding additional step of
// the security and barrier for malware nodes to retrieve valuable information.
// The reason for using onion obfuscation is to not give
// away to the nodes in the payment path the information about the exact
// failure and its origin.
func (o *OnionErrorEncrypter) EncryptError(initial bool, data []byte) []byte {
	if initial {
		umKey := generateKey("um", &o.sharedSecret)
		hash := hmac.New(sha256.New, umKey[:])
		hash.Write(data)
		h := hash.Sum(nil)
		data = append(h, data...)
	}

	return onionEncrypt(&o.sharedSecret, data)
}

// Encode writes the encrypter's shared secret to the provided io.Writer.
func (o *OnionErrorEncrypter) Encode(w io.Writer) error {
	_, err := w.Write(o.sharedSecret[:])
	return err
}

// Decode restores the encrypter's share secret from the provided io.Reader.
func (o *OnionErrorEncrypter) Decode(r io.Reader) error {
	_, err := io.ReadFull(r, o.sharedSecret[:])
	return err
}

// Circuit is used encapsulate the data which is needed for data deobfuscation.
type Circuit struct {
	// SessionKey is the key which have been used during generation of the
	// shared secrets.
	SessionKey *btcec.PrivateKey

	// PaymentPath is the pub keys of the nodes in the payment path.
	PaymentPath []*btcec.PublicKey
}

// Decode initializes the circuit from the byte stream.
func (c *Circuit) Decode(r io.Reader) error {
	var keyLength [1]byte
	if _, err := r.Read(keyLength[:]); err != nil {
		return err
	}

	sessionKeyData := make([]byte, uint8(keyLength[0]))
	if _, err := r.Read(sessionKeyData[:]); err != nil {
		return err
	}

	c.SessionKey, _ = btcec.PrivKeyFromBytes(btcec.S256(), sessionKeyData)
	var pathLength [1]byte
	if _, err := r.Read(pathLength[:]); err != nil {
		return err
	}
	c.PaymentPath = make([]*btcec.PublicKey, uint8(pathLength[0]))

	for i := 0; i < len(c.PaymentPath); i++ {
		var pubKeyData [btcec.PubKeyBytesLenCompressed]byte
		if _, err := r.Read(pubKeyData[:]); err != nil {
			return err
		}

		pubKey, err := btcec.ParsePubKey(pubKeyData[:], btcec.S256())
		if err != nil {
			return err
		}
		c.PaymentPath[i] = pubKey
	}

	return nil
}

// Encode writes converted circuit in the byte stream.
func (c *Circuit) Encode(w io.Writer) error {
	var keyLength [1]byte
	keyLength[0] = uint8(len(c.SessionKey.Serialize()))
	if _, err := w.Write(keyLength[:]); err != nil {
		return err
	}

	if _, err := w.Write(c.SessionKey.Serialize()); err != nil {
		return err
	}

	var pathLength [1]byte
	pathLength[0] = uint8(len(c.PaymentPath))
	if _, err := w.Write(pathLength[:]); err != nil {
		return err
	}

	for _, pubKey := range c.PaymentPath {
		if _, err := w.Write(pubKey.SerializeCompressed()); err != nil {
			return err
		}
	}

	return nil
}

// OnionErrorDecrypter is a struct that's used to decrypt onion errors in
// response to failed HTLC routing attempts according to BOLT#4.
type OnionErrorDecrypter struct {
	circuit *Circuit
}

// NewOnionErrorDecrypter creates new instance of onion decrypter.
func NewOnionErrorDecrypter(circuit *Circuit) *OnionErrorDecrypter {
	return &OnionErrorDecrypter{
		circuit: circuit,
	}
}

// DecryptError attempts to decrypt the passed encrypted error response. The
// onion failure is encrypted in backward manner, starting from the node where
// error have occurred. As a result, in order to decrypt the error we need get
// all shared secret and apply decryption in the reverse order.
func (o *OnionErrorDecrypter) DecryptError(encryptedData []byte) (*btcec.PublicKey, []byte, error) {

	sharedSecrets := generateSharedSecrets(
		o.circuit.PaymentPath,
		o.circuit.SessionKey,
	)

	var (
		sender      *btcec.PublicKey
		msg         []byte
		dummySecret Hash256
	)
	copy(dummySecret[:], bytes.Repeat([]byte{1}, 32))

	// We'll iterate a constant amount of hops to ensure that we don't give
	// away an timing information pertaining to the position in the route
	// that the error emanated from.
	for i := 0; i < NumMaxHops; i++ {
		var sharedSecret Hash256

		// If we've already found the sender, then we'll use our dummy
		// secret to continue decryption attempts to fill out the rest
		// of the loop. Otherwise, we'll use the next shared secret in
		// line.
		if sender != nil || i > len(sharedSecrets)-1 {
			sharedSecret = dummySecret
		} else {
			sharedSecret = sharedSecrets[i]
		}

		// With the shared secret, we'll now strip off a layer of
		// encryption from the encrypted error payload.
		encryptedData = onionEncrypt(&sharedSecret, encryptedData)

		// Next, we'll need to separate the data, from the MAC itself
		// so we can reconstruct and verify it.
		expectedMac := encryptedData[:sha256.Size]
		data := encryptedData[sha256.Size:]

		// With the data split, we'll now re-generate the MAC using its
		// specified key.
		umKey := generateKey("um", &sharedSecret)
		h := hmac.New(sha256.New, umKey[:])
		h.Write(data)

		// If the MAC matches up, then we've found the sender of the
		// error and have also obtained the fully decrypted message.
		realMac := h.Sum(nil)
		if hmac.Equal(realMac, expectedMac) && sender == nil {
			sender = o.circuit.PaymentPath[i]
			msg = data
		}
	}

	// If the sender pointer is still nil, then we haven't found the
	// sender, meaning we've failed to decrypt.
	if sender == nil {
		return nil, nil, errors.New("unable to retrieve onion failure")
	}

	return sender, msg, nil
}
