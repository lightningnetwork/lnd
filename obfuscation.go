package sphinx

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"io"

	"github.com/roasbeef/btcd/btcec"
)

// onionObfuscation obfuscates the data with compliance with BOLT#4.
// In context of Lightning Network this function is used by sender to obfuscate
// the onion failure and by receiver to unwrap the failure data.
func onionObfuscation(sharedSecret [sha256.Size]byte,
	data []byte) []byte {
	obfuscatedData := make([]byte, len(data))
	ammagKey := generateKey("ammag", sharedSecret)
	streamBytes := generateCipherStream(ammagKey, uint(len(data)))
	xor(obfuscatedData, data, streamBytes)
	return obfuscatedData
}

// OnionObfuscator represent serializable object which is able to convert the
// data to the obfuscated blob.
// In context of Lightning Network the obfuscated data is usually a failure
// which will be propagated back to payment sender, and obfuscated by the
// forwarding nodes.
type OnionObfuscator struct {
	sharedSecret [sha256.Size]byte
}

// NewOnionObfuscator creates new instance of onion obfuscator.
func NewOnionObfuscator(router *Router, ephemeralKey *btcec.PublicKey) (*OnionObfuscator,
	error) {

	sharedSecret, err := router.generateSharedSecret(ephemeralKey)
	if err != nil {
		return nil, err
	}

	return &OnionObfuscator{
		sharedSecret: sharedSecret,
	}, nil
}

// Obfuscate is used to make data obfuscation.
// In context of Lightning Network is either used by the nodes in order to
// make initial obfuscation with the creation of the hmac or by the forwarding
// nodes for backward failure obfuscation of the onion failure blob. By
// obfuscating the onion failure on every node in the path we are adding
// additional step of the security and barrier for malware nodes to retrieve
// valuable information. The reason for using onion obfuscation is to not give
// away to the nodes in the payment path the information about the exact failure
// and its origin.
func (o *OnionObfuscator) Obfuscate(initial bool, data []byte) []byte {
	if initial {
		umKey := generateKey("um", o.sharedSecret)
		hash := hmac.New(sha256.New, umKey[:])
		hash.Write(data)
		h := hash.Sum(nil)
		data = append(h, data...)
	}

	return onionObfuscation(o.sharedSecret, data)
}

// Decode initializes the obfuscator from the byte stream.
func (o *OnionObfuscator) Decode(r io.Reader) error {
	_, err := r.Read(o.sharedSecret[:])
	return err
}

// Encode writes converted obfuscator in the byte stream.
func (o *OnionObfuscator) Encode(w io.Writer) error {
	_, err := w.Write(o.sharedSecret[:])
	return err
}

// OnionDeobfuscator represents the serializable object which encapsulate the
// all necessary data to properly de-obfuscate previously obfuscated data.
// In context of Lightning Network the data which have to be deobfuscated
// usually is onion failure.
type OnionDeobfuscator struct {
	sessionKey  *btcec.PrivateKey
	paymentPath []*btcec.PublicKey
}

// NewOnionDeobfuscator creates new instance of onion deobfuscator.
func NewOnionDeobfuscator(sessionKey *btcec.PrivateKey,
	paymentPath []*btcec.PublicKey) (*OnionDeobfuscator, error) {
	return &OnionDeobfuscator{
		sessionKey:  sessionKey,
		paymentPath: paymentPath,
	}, nil
}

// Deobfuscate makes data deobfuscation. The onion failure is obfuscated in
// backward manner, starting from the node where error have occurred, so in
// order to deobfuscate the error we need get all shared secret and apply
// obfuscation in reverse order.
func (o *OnionDeobfuscator) Deobfuscate(obfuscatedData []byte) (*btcec.PublicKey,
	[]byte, error) {
	for i, sharedSecret := range generateSharedSecrets(o.paymentPath,
		o.sessionKey) {
		obfuscatedData = onionObfuscation(sharedSecret, obfuscatedData)
		umKey := generateKey("um", sharedSecret)

		// Split the data and hmac.
		expectedMac := obfuscatedData[:sha256.Size]
		data := obfuscatedData[sha256.Size:]

		// Calculate the real hmac.
		h := hmac.New(sha256.New, umKey[:])
		h.Write(data)
		realMac := h.Sum(nil)

		if bytes.Equal(realMac, expectedMac) {
			return o.paymentPath[i], data, nil
		}
	}

	return nil, nil, errors.New("unable to retrieve onion failure")
}

// Decode initializes the deobfuscator from the byte stream.
func (o *OnionDeobfuscator) Decode(r io.Reader) error {
	var keyLength [1]byte
	if _, err := r.Read(keyLength[:]); err != nil {
		return err
	}

	sessionKeyData := make([]byte, uint8(keyLength[0]))
	if _, err := r.Read(sessionKeyData[:]); err != nil {
		return err
	}

	o.sessionKey, _ = btcec.PrivKeyFromBytes(btcec.S256(), sessionKeyData)
	var pathLength [1]byte
	if _, err := r.Read(pathLength[:]); err != nil {
		return err
	}
	o.paymentPath = make([]*btcec.PublicKey, uint8(pathLength[0]))

	for i := 0; i < len(o.paymentPath); i++ {
		var pubKeyData [btcec.PubKeyBytesLenCompressed]byte
		if _, err := r.Read(pubKeyData[:]); err != nil {
			return err
		}

		pubKey, err := btcec.ParsePubKey(pubKeyData[:], btcec.S256())
		if err != nil {
			return err
		}
		o.paymentPath[i] = pubKey
	}

	return nil
}

// Encode writes converted deobfuscator in the byte stream.
func (o *OnionDeobfuscator) Encode(w io.Writer) error {
	var keyLength [1]byte
	keyLength[0] = uint8(len(o.sessionKey.Serialize()))
	if _, err := w.Write(keyLength[:]); err != nil {
		return err
	}

	if _, err := w.Write(o.sessionKey.Serialize()); err != nil {
		return err
	}

	var pathLength [1]byte
	pathLength[0] = uint8(len(o.paymentPath))
	if _, err := w.Write(pathLength[:]); err != nil {
		return err
	}

	for _, pubKey := range o.paymentPath {
		if _, err := w.Write(pubKey.SerializeCompressed()); err != nil {
			return err
		}
	}

	return nil
}
