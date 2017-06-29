package htlcswitch

import (
	"bytes"

	"github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
)

// Deobfuscator entity which is used to de-obfuscate the onion opaque reason and
// extract failure.
type Deobfuscator interface {
	// Deobfuscate function decodes the onion error failure.
	Deobfuscate(lnwire.OpaqueReason) (lnwire.FailureMessage, error)
}

// Obfuscator entity which is used to do the initial and backward onion
// failure obfuscation.
type Obfuscator interface {
	// InitialObfuscate is used to convert the failure into opaque
	// reason.
	InitialObfuscate(lnwire.FailureMessage) (lnwire.OpaqueReason, error)

	// BackwardObfuscate is used to make the processing over onion error
	// when it moves backward to the htlc sender.
	BackwardObfuscate(lnwire.OpaqueReason) lnwire.OpaqueReason
}

// FailureObfuscator is used to obfuscate the onion failure.
type FailureObfuscator struct {
	*sphinx.OnionObfuscator
}

// InitialObfuscate is used by the failure sender to decode the failure and
// make the initial failure obfuscation with addition of the failure data hmac.
//
// NOTE: Part of the Obfuscator interface.
func (o *FailureObfuscator) InitialObfuscate(failure lnwire.FailureMessage) (
	lnwire.OpaqueReason, error) {
	var b bytes.Buffer
	if err := lnwire.EncodeFailure(&b, failure, 0); err != nil {
		return nil, err
	}

	// Make the initial obfuscation with appending hmac.
	return o.OnionObfuscator.Obfuscate(true, b.Bytes()), nil
}

// BackwardObfuscate is used by the forwarding nodes in order to obfuscate the
// already obfuscated onion failure blob with the stream which have been
// generated with our shared secret. The reason we re-encrypt the message on the
// backwards path is to ensure that the error is computationally
// indistinguishable from any other error seen.
//
// NOTE: Part of the Obfuscator interface.
func (o *FailureObfuscator) BackwardObfuscate(
	reason lnwire.OpaqueReason) lnwire.OpaqueReason {
	return o.OnionObfuscator.Obfuscate(false, reason)
}

// A compile time check to ensure FailureObfuscator implements the
// Obfuscator interface.
var _ Obfuscator = (*FailureObfuscator)(nil)

// FailureDeobfuscator wraps the sphinx data obfuscator and adds awareness of
// the lnwire onion failure messages to it.
type FailureDeobfuscator struct {
	*sphinx.OnionDeobfuscator
}

// Deobfuscate decodes the obfuscated onion failure.
//
// NOTE: Part of the Obfuscator interface.
func (o *FailureDeobfuscator) Deobfuscate(reason lnwire.OpaqueReason) (lnwire.FailureMessage,
	error) {
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
