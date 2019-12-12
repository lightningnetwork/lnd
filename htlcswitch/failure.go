package htlcswitch

import (
	"bytes"
	"fmt"

	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lnwire"
)

// SwitchError is an interface implemented by internal errors that wrap a wire
// message along with additional metadata. These errors originated from within
// our node.
type SwitchError interface {
	GetWireFailure() lnwire.FailureMessage
	error
}

// ForwardingError wraps an switch error with a failure index. It does not
// directly embed the lnwire.FailureMessage interface because doing so embeds the
// lnwire.FailureMessage functionality but obscures whether the wire message is
// Serializable, because this interface is not implemented on the ForwardingError
// itself.
type ForwardingError struct {
	// FailureSourceIdx is the index of the node that sent the failure. With
	// this information, the dispatcher of a payment can modify their set of
	// candidate routes in response to the type of failure extracted. Index
	// zero is the self node.
	FailureSourceIdx int

	SwitchError
}

// Error implements the built-in error interface. We use this method to allow
// the switch or any callers to insert additional context to the error message
// returned.
func (f *ForwardingError) Error() string {
	return fmt.Sprintf(
		"%v@%v", f.GetWireFailure().Error(), f.FailureSourceIdx,
	)
}

// ErrorDecrypter is an interface that is used to decrypt the onion encrypted
// failure reason an extra out a well formed error.
type ErrorDecrypter interface {
	// DecryptError peels off each layer of onion encryption from the first
	// hop, to the source of the error. A fully populated
	// lnwire.FailureMessage is returned along with the source of the
	// error.
	DecryptError(lnwire.OpaqueReason) (SwitchError, error)
}

// UnknownEncrypterType is an error message used to signal that an unexpected
// EncrypterType was encountered during decoding.
type UnknownEncrypterType hop.EncrypterType

// Error returns a formatted error indicating the invalid EncrypterType.
func (e UnknownEncrypterType) Error() string {
	return fmt.Sprintf("unknown error encrypter type: %d", e)
}

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
	SwitchError, error) {

	failure, err := s.OnionErrorDecrypter.DecryptError(reason)
	if err != nil {
		return nil, err
	}

	// Decode the failure. If an error occurs, we leave the failure message
	// field nil.
	r := bytes.NewReader(failure.Message)
	failureMsg, err := lnwire.DecodeFailure(r, 0)
	if err != nil {
		return &ForwardingError{
			FailureSourceIdx: failure.SenderIdx,
		}, nil
	}

	return &ForwardingError{
		FailureSourceIdx: failure.SenderIdx,
		SwitchError:      newSwitchError(failureMsg),
	}, nil
}

// A compile time check to ensure ErrorDecrypter implements the Deobfuscator
// interface.
var _ ErrorDecrypter = (*SphinxErrorDecrypter)(nil)
