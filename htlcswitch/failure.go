package htlcswitch

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ForwardingError wraps an lnwire.FailureMessage in a struct that also
// includes the source of the error.
type ForwardingError struct {
	// FailureSourceIdx is the index of the node that sent the failure. With
	// this information, the dispatcher of a payment can modify their set of
	// candidate routes in response to the type of failure extracted. Index
	// zero is the self node.
	FailureSourceIdx int

	// ExtraMsg is an additional error message that callers can provide in
	// order to provide context specific error details.
	ExtraMsg string

	lnwire.FailureMessage
}

// Error implements the built-in error interface. We use this method to allow
// the switch or any callers to insert additional context to the error message
// returned.
func (f *ForwardingError) Error() string {
	if f.ExtraMsg == "" {
		return f.FailureMessage.Error()
	}

	return fmt.Sprintf("%v: %v", f.FailureMessage.Error(), f.ExtraMsg)
}

// ErrorDecrypter is an interface that is used to decrypt the onion encrypted
// failure reason an extra out a well formed error.
type ErrorDecrypter interface {
	// DecryptError peels off each layer of onion encryption from the first
	// hop, to the source of the error. A fully populated
	// lnwire.FailureMessage is returned along with the source of the
	// error.
	DecryptError(lnwire.OpaqueReason) (*ForwardingError, error)
}

// EncrypterType establishes an enum used in serialization to indicate how to
// decode a concrete instance of the ErrorEncrypter interface.
type EncrypterType byte

const (
	// EncrypterTypeNone signals that no error encyrpter is present, this
	// can happen if the htlc is originates in the switch.
	EncrypterTypeNone EncrypterType = 0

	// EncrypterTypeSphinx is used to identify a sphinx onion error
	// encrypter instance.
	EncrypterTypeSphinx = 1

	// EncrypterTypeMock is used to identify a mock obfuscator instance.
	EncrypterTypeMock = 2
)

// UnknownEncrypterType is an error message used to signal that an unexpected
// EncrypterType was encountered during decoding.
type UnknownEncrypterType EncrypterType

// Error returns a formatted error indicating the invalid EncrypterType.
func (e UnknownEncrypterType) Error() string {
	return fmt.Sprintf("unknown error encrypter type: %d", e)
}

// ErrorEncrypterExtracter defines a function signature that extracts an
// ErrorEncrypter from an sphinx OnionPacket.
type ErrorEncrypterExtracter func(*btcec.PublicKey) (ErrorEncrypter,
	lnwire.FailCode)

// ErrorEncrypter is an interface that is used to encrypt HTLC related errors
// at the source of the error, and also at each intermediate hop all the way
// back to the source of the payment.
type ErrorEncrypter interface {
	// EncryptFirstHop transforms a concrete failure message into an
	// encrypted opaque failure reason. This method will be used at the
	// source that the error occurs. It differs from IntermediateEncrypt
	// slightly, in that it computes a proper MAC over the error.
	EncryptFirstHop(lnwire.FailureMessage) (lnwire.OpaqueReason, error)

	// EncryptMalformedError is similar to EncryptFirstHop (it adds the
	// MAC), but it accepts an opaque failure reason rather than a failure
	// message. This method is used when we receive an
	// UpdateFailMalformedHTLC from the remote peer and then need to
	// convert that into a proper error from only the raw bytes.
	EncryptMalformedError(lnwire.OpaqueReason) lnwire.OpaqueReason

	// IntermediateEncrypt wraps an already encrypted opaque reason error
	// in an additional layer of onion encryption. This process repeats
	// until the error arrives at the source of the payment.
	IntermediateEncrypt(lnwire.OpaqueReason) lnwire.OpaqueReason

	// Type returns an enum indicating the underlying concrete instance
	// backing this interface.
	Type() EncrypterType

	// Encode serializes the encrypter's ephemeral public key to the given
	// io.Writer.
	Encode(io.Writer) error

	// Decode deserializes the encrypter' ephemeral public key from the
	// given io.Reader.
	Decode(io.Reader) error

	// Reextract rederives the encrypter using the extracter, performing an
	// ECDH with the sphinx router's key and the ephemeral public key.
	//
	// NOTE: This should be called shortly after Decode to properly
	// reinitialize the error encrypter.
	Reextract(ErrorEncrypterExtracter) error
}

// SphinxErrorEncrypter is a concrete implementation of both the ErrorEncrypter
// interface backed by an implementation of the Sphinx packet format. As a
// result, all errors handled are themselves wrapped in layers of onion
// encryption and must be treated as such accordingly.
type SphinxErrorEncrypter struct {
	*sphinx.OnionErrorEncrypter

	EphemeralKey *btcec.PublicKey
}

// NewSphinxErrorEncrypter initializes a blank sphinx error encrypter, that
// should be used to deserialize an encoded SphinxErrorEncrypter. Since the
// actual encrypter is not stored in plaintext while at rest, reconstructing the
// error encrypter requires:
//   1) Decode: to deserialize the ephemeral public key.
//   2) Reextract: to "unlock" the actual error encrypter using an active
//        OnionProcessor.
func NewSphinxErrorEncrypter() *SphinxErrorEncrypter {
	return &SphinxErrorEncrypter{
		OnionErrorEncrypter: nil,
		EphemeralKey:        &btcec.PublicKey{},
	}
}

// EncryptFirstHop transforms a concrete failure message into an encrypted
// opaque failure reason. This method will be used at the source that the error
// occurs. It differs from BackwardObfuscate slightly, in that it computes a
// proper MAC over the error.
//
// NOTE: Part of the ErrorEncrypter interface.
func (s *SphinxErrorEncrypter) EncryptFirstHop(failure lnwire.FailureMessage) (lnwire.OpaqueReason, error) {
	var b bytes.Buffer
	if err := lnwire.EncodeFailure(&b, failure, 0); err != nil {
		return nil, err
	}

	// We pass a true as the first parameter to indicate that a MAC should
	// be added.
	return s.EncryptError(true, b.Bytes()), nil
}

// EncryptMalformedError is similar to EncryptFirstHop (it adds the MAC), but
// it accepts an opaque failure reason rather than a failure message. This
// method is used when we receive an UpdateFailMalformedHTLC from the remote
// peer and then need to convert that into an proper error from only the raw
// bytes.
//
// NOTE: Part of the ErrorEncrypter interface.
func (s *SphinxErrorEncrypter) EncryptMalformedError(reason lnwire.OpaqueReason) lnwire.OpaqueReason {
	return s.EncryptError(true, reason)
}

// IntermediateEncrypt wraps an already encrypted opaque reason error in an
// additional layer of onion encryption. This process repeats until the error
// arrives at the source of the payment. We re-encrypt the message on the
// backwards path to ensure that the error is indistinguishable from any other
// error seen.
//
// NOTE: Part of the ErrorEncrypter interface.
func (s *SphinxErrorEncrypter) IntermediateEncrypt(reason lnwire.OpaqueReason) lnwire.OpaqueReason {
	return s.EncryptError(false, reason)
}

// Type returns the identifier for a sphinx error encrypter.
func (s *SphinxErrorEncrypter) Type() EncrypterType {
	return EncrypterTypeSphinx
}

// Encode serializes the error encrypter' ephemeral public key to the provided
// io.Writer.
func (s *SphinxErrorEncrypter) Encode(w io.Writer) error {
	ephemeral := s.EphemeralKey.SerializeCompressed()
	_, err := w.Write(ephemeral)
	return err
}

// Decode reconstructs the error encrypter's ephemeral public key from the
// provided io.Reader.
func (s *SphinxErrorEncrypter) Decode(r io.Reader) error {
	var ephemeral [33]byte
	if _, err := io.ReadFull(r, ephemeral[:]); err != nil {
		return err
	}

	var err error
	s.EphemeralKey, err = btcec.ParsePubKey(ephemeral[:], btcec.S256())
	if err != nil {
		return err
	}

	return nil
}

// Reextract rederives the error encrypter from the currently held EphemeralKey.
// This intended to be used shortly after Decode, to fully initialize a
// SphinxErrorEncrypter.
func (s *SphinxErrorEncrypter) Reextract(
	extract ErrorEncrypterExtracter) error {

	obfuscator, failcode := extract(s.EphemeralKey)
	if failcode != lnwire.CodeNone {
		// This should never happen, since we already validated that
		// this obfuscator can be extracted when it was received in the
		// link.
		return fmt.Errorf("unable to reconstruct onion "+
			"obfuscator, got failcode: %d", failcode)
	}

	sphinxEncrypter, ok := obfuscator.(*SphinxErrorEncrypter)
	if !ok {
		return fmt.Errorf("incorrect onion error extracter")
	}

	// Copy the freshly extracted encrypter.
	s.OnionErrorEncrypter = sphinxEncrypter.OnionErrorEncrypter

	return nil

}

// A compile time check to ensure SphinxErrorEncrypter implements the
// ErrorEncrypter interface.
var _ ErrorEncrypter = (*SphinxErrorEncrypter)(nil)

// OnionErrorDecrypter is the interface that provides onion level error
// decryption.
type OnionErrorDecrypter interface {
	// DecryptError attempts to decrypt the passed encrypted error response.
	// The onion failure is encrypted in backward manner, starting from the
	// node where error have occurred. As a result, in order to decrypt the
	// error we need get all shared secret and apply decryption in the
	// reverse order.
	DecryptError(encryptedData []byte) (*sphinx.DecryptedError, error)
}

// SphinxErrorDecrypter wraps the sphinx data SphinxErrorDecrypter and maps the
// returned errors to concrete lnwire.FailureMessage instances.
type SphinxErrorDecrypter struct {
	OnionErrorDecrypter
}

// DecryptError peels off each layer of onion encryption from the first hop, to
// the source of the error. A fully populated lnwire.FailureMessage is returned
// along with the source of the error.
//
// NOTE: Part of the ErrorDecrypter interface.
func (s *SphinxErrorDecrypter) DecryptError(reason lnwire.OpaqueReason) (
	*ForwardingError, error) {

	failure, err := s.OnionErrorDecrypter.DecryptError(reason)
	if err != nil {
		return nil, err
	}

	r := bytes.NewReader(failure.Message)
	failureMsg, err := lnwire.DecodeFailure(r, 0)
	if err != nil {
		return nil, err
	}

	return &ForwardingError{
		FailureSourceIdx: failure.SenderIdx,
		FailureMessage:   failureMsg,
	}, nil
}

// A compile time check to ensure ErrorDecrypter implements the Deobfuscator
// interface.
var _ ErrorDecrypter = (*SphinxErrorDecrypter)(nil)
