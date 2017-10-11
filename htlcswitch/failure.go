package htlcswitch

import (
	"bytes"

	"github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
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
// failure reason an extra out a well formed error.
type Deobfuscator interface {
	// Deobfuscate peels off each layer of onion encryption from the first
	// hop, to the source of the error. A fully populated
	// lnwire.FailureMessage is returned.
	Deobfuscate(lnwire.OpaqueReason) (lnwire.FailureMessage, error)
}

// Obfuscator is an interface that is used to encrypt HTLC related errors at
// the source of the error, and also at each intermediate hop all the way back
// to the source of the payment.
type Obfuscator interface {
	// InitialObfuscate is used to convert the failure into opaque
	// reason.

	// InitialObfuscate transforms a concrete failure message into an
	// encrypted opaque failure reason. This method will be used at the
	// source that the error occurs. It differs from BackwardObfuscate
	// slightly, in that it computes a proper MAC over the error.
	InitialObfuscate(lnwire.FailureMessage) (lnwire.OpaqueReason, error)

	// BackwardObfuscate wraps an already encrypted opaque reason error in
	// an additional layer of onion encryption. This process repeats until
	// the error arrives at the source of the payment.
	BackwardObfuscate(lnwire.OpaqueReason) lnwire.OpaqueReason
}

// FailureObfuscator is used to obfuscate the onion failure.
type FailureObfuscator struct {
	*sphinx.OnionObfuscator
}

// InitialObfuscate transforms a concrete failure message into an encrypted
// opaque failure reason. This method will be used at the source that the error
// occurs. It differs from BackwardObfuscate slightly, in that it computes a
// proper MAC over the error.
//
// NOTE: Part of the Obfuscator interface.
func (o *FailureObfuscator) InitialObfuscate(failure lnwire.FailureMessage) (lnwire.OpaqueReason, error) {
	var b bytes.Buffer
	if err := lnwire.EncodeFailure(&b, failure, 0); err != nil {
		return nil, err
	}

	// We pass a true as the first parameter to indicate that a MAC should
	// be added.
	return o.OnionObfuscator.Obfuscate(true, b.Bytes()), nil
}

// BackwardObfuscate wraps an already encrypted opaque reason error in an
// additional layer of onion encryption. This process repeats until the error
// arrives at the source of the payment. We re-encrypt the message on the
// backwards path to ensure that the error is indistinguishable from any other
// error seen.
//
// NOTE: Part of the Obfuscator interface.
func (o *FailureObfuscator) BackwardObfuscate(reason lnwire.OpaqueReason) lnwire.OpaqueReason {
	return o.OnionObfuscator.Obfuscate(false, reason)
}

// A compile time check to ensure FailureObfuscator implements the Obfuscator
// interface.
var _ Obfuscator = (*FailureObfuscator)(nil)

// FailureDeobfuscator wraps the sphinx data obfuscator and adds awareness of
// the lnwire onion failure messages to it.
type FailureDeobfuscator struct {
	*sphinx.OnionDeobfuscator
}

// Deobfuscate peels off each layer of onion encryption from the first hop, to
// the source of the error. A fully populated lnwire.FailureMessage is
// returned.
//
// NOTE: Part of the Obfuscator interface.
func (o *FailureDeobfuscator) Deobfuscate(reason lnwire.OpaqueReason) (lnwire.FailureMessage, error) {

	_, failureData, err := o.OnionDeobfuscator.Deobfuscate(reason)
	if err != nil {
		return nil, err
	}

	r := bytes.NewReader(failureData)
	return lnwire.DecodeFailure(r, 0)
}

// A compile time check to ensure FailureDeobfuscator implements the
// Deobfuscator interface.
var _ Deobfuscator = (*FailureDeobfuscator)(nil)
