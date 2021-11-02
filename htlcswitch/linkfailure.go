package htlcswitch

import (
	"fmt"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrLinkShuttingDown signals that the link is shutting down.
	ErrLinkShuttingDown = errors.New("link shutting down")

	// ErrLinkFailedShutdown signals that a requested shutdown failed.
	ErrLinkFailedShutdown = errors.New("link failed to shutdown")

	ErrRemoteUnresponsive = errors.New("remote unresponsive")
)

// LinkFailureError encapsulates an error that will make us fail the current
// link. It contains the necessary information needed to determine if we should
// force close the channel in the process, and if any error data should be sent
// to the peer.
type LinkFailureError struct {
	// failure is the error this LinkFailureError encapsulates.
	failure error

	// ForceClose indicates whether we should force close the channel
	// because of this error.
	ForceClose bool

	// PermanentFailure indicates whether this failure is permanent, and
	// the channel should not be attempted loaded again.
	PermanentFailure bool
}

// A compile time check to ensure LinkFailureError implements the error
// interface.
var _ error = (*LinkFailureError)(nil)

// Error returns a generic error for the LinkFailureError.
//
// NOTE: Part of the error interface.
func (e LinkFailureError) Error() string {
	return e.failure.Error()
}

// WireError returns a boolean indicating whether we should send an error to
// our peer and an appropriate wire error.
func (e LinkFailureError) WireError(chanID lnwire.ChannelID) (*lnwire.Error,
	bool) {

	// If our failure is an extended wire error, we want to send it to our
	// peer.
	extended, isExtended := e.failure.(lnwire.ExtendedError)
	if isExtended {
		wireError, err := lnwire.WireErrorFromExtended(
			extended, chanID,
		)
		if err != nil {
			panic(fmt.Sprintf("could not get wire error: %v", err))
		}

		return wireError, true
	}

	return nil, false
}
