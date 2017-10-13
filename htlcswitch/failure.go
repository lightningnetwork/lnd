package htlcswitch

import (
	"bytes"

	"github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
)

// ForwardingError wraps an lnwire.FailureMessage in a struct that also
// includes the source of the error.
type ForwardingError struct {
	// ErrorSource is the public key of the node that sent the error. With
	// this information, the dispatcher of a payment can modify their set
	// of candidate routes in response to the type of error extracted.
	ErrorSource *btcec.PublicKey

	lnwire.FailureMessage
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

// ErrorEncrypter is an interface that is used to encrypt HTLC related errors
// at the source of the error, and also at each intermediate hop all the way
// back to the source of the payment.
type ErrorEncrypter interface {
	// EncryptFirstHop transforms a concrete failure message into an
	// encrypted opaque failure reason. This method will be used at the
	// source that the error occurs. It differs from IntermediateEncrypt
	// slightly, in that it computes a proper MAC over the error.
	EncryptFirstHop(lnwire.FailureMessage) (lnwire.OpaqueReason, error)

	// IntermediateEncrypt wraps an already encrypted opaque reason error
	// in an additional layer of onion encryption. This process repeats
	// until the error arrives at the source of the payment.
	IntermediateEncrypt(lnwire.OpaqueReason) lnwire.OpaqueReason
}

// SphinxErrorEncrypter is a concrete implementation of both the ErrorEncrypter
// interface backed by an implementation of the Sphinx packet format. As a
// result, all errors handled are themselves wrapped in layers of onion
// encryption and must be treated as such accordingly.
type SphinxErrorEncrypter struct {
	*sphinx.OnionErrorEncrypter
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

// A compile time check to ensure SphinxErrorEncrypter implements the
// ErrorEncrypter interface.
var _ ErrorEncrypter = (*SphinxErrorEncrypter)(nil)

// SphinxErrorDecrypter wraps the sphinx data SphinxErrorDecrypter and maps the
// returned errors to concrete lnwire.FailureMessage instances.
type SphinxErrorDecrypter struct {
	*sphinx.OnionErrorDecrypter
}

// DecryptError peels off each layer of onion encryption from the first hop, to
// the source of the error. A fully populated lnwire.FailureMessage is returned
// along with the source of the error.
//
// NOTE: Part of the ErrorDecrypter interface.
func (s *SphinxErrorDecrypter) DecryptError(reason lnwire.OpaqueReason) (*ForwardingError, error) {

	source, failureData, err := s.OnionErrorDecrypter.DecryptError(reason)
	if err != nil {
		return nil, err
	}

	r := bytes.NewReader(failureData)
	failureMsg, err := lnwire.DecodeFailure(r, 0)
	if err != nil {
		return nil, err
	}

	return &ForwardingError{
		ErrorSource:    source,
		FailureMessage: failureMsg,
	}, nil
}

// A compile time check to ensure ErrorDecrypter implements the Deobfuscator
// interface.
var _ ErrorDecrypter = (*SphinxErrorDecrypter)(nil)
