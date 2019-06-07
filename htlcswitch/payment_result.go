package htlcswitch

import (
	"errors"
	"io"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrPaymentIDNotFound is an error returned if the given paymentID is
	// not found.
	ErrPaymentIDNotFound = errors.New("paymentID not found")

	// ErrPaymentIDAlreadyExists is returned if we try to write a pending
	// payment whose paymentID already exists.
	ErrPaymentIDAlreadyExists = errors.New("paymentID already exists")
)

// PaymentResult wraps a decoded result received from the network after a
// payment attempt was made. This is what is eventually handed to the router
// for processing.
type PaymentResult struct {
	// Preimage is set by the switch in case a sent HTLC was settled.
	Preimage [32]byte

	// Error is non-nil in case a HTLC send failed, and the HTLC is now
	// irrevocably cancelled. If the payment failed during forwarding, this
	// error will be a *ForwardingError.
	Error error
}

// networkResult is the raw result received from the network after a payment
// attempt has been made. Since the switch doesn't always have the necessary
// data to decode the raw message, we store it together with some meta data,
// and decode it when the router query for the final result.
type networkResult struct {
	// msg is the received result. This should be of type UpdateFulfillHTLC
	// or UpdateFailHTLC.
	msg lnwire.Message

	// unencrypted indicates whether the failure encoded in the message is
	// unencrypted, and hence doesn't need to be decrypted.
	unencrypted bool

	// isResolution indicates whether this is a resolution message, in
	// which the failure reason might not be included.
	isResolution bool
}

// serializeNetworkResult serializes the networkResult.
func serializeNetworkResult(w io.Writer, n *networkResult) error {
	if _, err := lnwire.WriteMessage(w, n.msg, 0); err != nil {
		return err
	}

	return channeldb.WriteElements(w, n.unencrypted, n.isResolution)
}

// deserializeNetworkResult deserializes the networkResult.
func deserializeNetworkResult(r io.Reader) (*networkResult, error) {
	var (
		err error
	)

	n := &networkResult{}

	n.msg, err = lnwire.ReadMessage(r, 0)
	if err != nil {
		return nil, err
	}

	if err := channeldb.ReadElements(r,
		&n.unencrypted, &n.isResolution,
	); err != nil {
		return nil, err
	}

	return n, nil
}
