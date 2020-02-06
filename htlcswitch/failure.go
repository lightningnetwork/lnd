package htlcswitch

import (
	"bytes"
	"fmt"

	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ClearTextError is an interface which is implemented by errors that occur
// when we know the underlying wire failure message. These errors are the
// opposite to opaque errors which are onion-encrypted blobs only understandable
// to the initiating node. ClearTextErrors are used when we fail a htlc at our
// node, or one of our initiated payments failed and we can decrypt the onion
// encrypted error fully.
type ClearTextError interface {
	error

	// WireMessage extracts a valid wire failure message from an internal
	// error which may contain additional metadata (which should not be
	// exposed to the network). This value may be nil in the case where
	// an unknown wire error is returned by one of our peers.
	WireMessage() lnwire.FailureMessage
}

// LinkError is an implementation of the ClearTextError interface which
// represents failures that occur on our incoming or outgoing link.
type LinkError struct {
	// msg returns the wire failure associated with the error.
	// This value should *not* be nil, because we should always
	// know the failure type for failures which occur at our own
	// node.
	msg lnwire.FailureMessage

	// FailureDetail enriches the wire error with additional information.
	FailureDetail
}

// NewLinkError returns a LinkError with the failure message provided.
// The failure message provided should *not* be nil, because we should
// always know the failure type for failures which occur at our own node.
func NewLinkError(msg lnwire.FailureMessage) *LinkError {
	return &LinkError{msg: msg}
}

// NewDetailedLinkError returns a link error that enriches a wire message with
// a failure detail.
func NewDetailedLinkError(msg lnwire.FailureMessage,
	detail FailureDetail) *LinkError {

	return &LinkError{
		msg:           msg,
		FailureDetail: detail,
	}
}

// WireMessage extracts a valid wire failure message from an internal
// error which may contain additional metadata (which should not be
// exposed to the network). This value should never be nil for LinkErrors,
// because we are the ones failing the htlc.
//
// Note this is part of the ClearTextError interface.
func (l *LinkError) WireMessage() lnwire.FailureMessage {
	return l.msg
}

// Error returns the string representation of a link error.
//
// Note this is part of the ClearTextError interface.
func (l *LinkError) Error() string {
	// If the link error has no failure detail, return the wire message's
	// error.
	if l.FailureDetail == nil {
		return l.msg.Error()
	}

	return l.FailureDetail.FailureString()
}

// ForwardingError wraps an lnwire.FailureMessage in a struct that also
// includes the source of the error.
type ForwardingError struct {
	// FailureSourceIdx is the index of the node that sent the failure. With
	// this information, the dispatcher of a payment can modify their set of
	// candidate routes in response to the type of failure extracted. Index
	// zero is the self node.
	FailureSourceIdx int

	// msg is the wire message associated with the error. This value may
	// be nil in the case where we fail to decode failure message sent by
	// a peer.
	msg lnwire.FailureMessage
}

// WireMessage extracts a valid wire failure message from an internal
// error which may contain additional metadata (which should not be
// exposed to the network). This value may be nil in the case where
// an unknown wire error is returned by one of our peers.
//
// Note this is part of the ClearTextError interface.
func (f *ForwardingError) WireMessage() lnwire.FailureMessage {
	return f.msg
}

// Error implements the built-in error interface. We use this method to allow
// the switch or any callers to insert additional context to the error message
// returned.
func (f *ForwardingError) Error() string {
	return fmt.Sprintf(
		"%v@%v", f.msg, f.FailureSourceIdx,
	)
}

// NewForwardingError creates a new payment error which wraps a wire error
// with additional metadata.
func NewForwardingError(failure lnwire.FailureMessage,
	index int) *ForwardingError {

	return &ForwardingError{
		FailureSourceIdx: index,
		msg:              failure,
	}
}

// NewUnknownForwardingError returns a forwarding error which has a nil failure
// message. This constructor should only be used in the case where we cannot
// decode the failure we have received from a peer.
func NewUnknownForwardingError(index int) *ForwardingError {
	return &ForwardingError{
		FailureSourceIdx: index,
	}
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
	*ForwardingError, error) {

	failure, err := s.OnionErrorDecrypter.DecryptError(reason)
	if err != nil {
		return nil, err
	}

	// Decode the failure. If an error occurs, we leave the failure message
	// field nil.
	r := bytes.NewReader(failure.Message)
	failureMsg, err := lnwire.DecodeFailure(r, 0)
	if err != nil {
		return NewUnknownForwardingError(failure.SenderIdx), nil
	}

	return NewForwardingError(failureMsg, failure.SenderIdx), nil
}

// A compile time check to ensure ErrorDecrypter implements the Deobfuscator
// interface.
var _ ErrorDecrypter = (*SphinxErrorDecrypter)(nil)
