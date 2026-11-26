package switchrpc

import (
	"bytes"
	"fmt"

	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwire"
)

// UnmarshallForwardingError converts a protobuf ForwardingFailure message into
// an htlcswitch.ForwardingError.
func UnmarshallForwardingError(f *ForwardingFailure) (
	*htlcswitch.ForwardingError, error) {

	if f == nil {
		return nil, fmt.Errorf("cannot parse nil ForwardingFailure")
	}

	wireMsg, err := UnmarshallFailureMessage(f.WireMessage)
	if err != nil {
		return nil, fmt.Errorf("failed to decode wire message: %w", err)
	}

	return htlcswitch.NewForwardingError(
		wireMsg, int(f.FailureSourceIndex),
	), nil
}

// UnmarshallLinkError converts a protobuf LocalFailure message into an
// htlcswitch.LinkError.
func UnmarshallLinkError(f *LocalFailure) (*htlcswitch.LinkError, error) {
	if f == nil {
		return nil, fmt.Errorf("cannot parse nil LocalFailure")
	}

	wireMsg, err := UnmarshallFailureMessage(f.WireMessage)
	if err != nil {
		return nil, fmt.Errorf("failed to decode wire message: %w", err)
	}

	return htlcswitch.NewLinkError(wireMsg), nil
}

// UnmarshallFailureMessage decodes a raw wire message byte slice into a rich
// lnwire.FailureMessage object.
func UnmarshallFailureMessage(wireMsg []byte) (lnwire.FailureMessage, error) {
	r := bytes.NewReader(wireMsg)

	return lnwire.DecodeFailure(r, 0)
}
