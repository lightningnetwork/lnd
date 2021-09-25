package htlcswitch

import "github.com/go-errors/errors"

var (
	// ErrLinkShuttingDown signals that the link is shutting down.
	ErrLinkShuttingDown = errors.New("link shutting down")

	// ErrLinkFailedShutdown signals that a requested shutdown failed.
	ErrLinkFailedShutdown = errors.New("link failed to shutdown")
)

// errorCode encodes the possible types of errors that will make us fail the
// current link.
type errorCode uint8

const (
	// ErrInternalError indicates that something internal in the link
	// failed. In this case we will send a generic error to our peer.
	ErrInternalError errorCode = iota

	// ErrRemoteError indicates that our peer sent an error, prompting up
	// to fail the link.
	ErrRemoteError

	// ErrRemoteUnresponsive indicates that our peer took too long to
	// complete a commitment dance.
	ErrRemoteUnresponsive

	// ErrSyncError indicates that we failed synchronizing the state of the
	// channel with our peer.
	ErrSyncError

	// ErrInvalidUpdate indicates that the peer send us an invalid update.
	ErrInvalidUpdate

	// ErrInvalidCommitment indicates that the remote peer sent us an
	// invalid commitment signature.
	ErrInvalidCommitment

	// ErrInvalidRevocation indicates that the remote peer send us an
	// invalid revocation message.
	ErrInvalidRevocation

	// ErrRecoveryError the channel was unable to be resumed, we need the
	// remote party to force close the channel out on chain now as a
	// result.
	ErrRecoveryError
)

// LinkFailureError encapsulates an error that will make us fail the current
// link. It contains the necessary information needed to determine if we should
// force close the channel in the process, and if any error data should be sent
// to the peer.
type LinkFailureError struct {
	// code is the type of error this LinkFailureError encapsulates.
	code errorCode

	// ForceClose indicates whether we should force close the channel
	// because of this error.
	ForceClose bool

	// PermanentFailure indicates whether this failure is permanent, and
	// the channel should not be attempted loaded again.
	PermanentFailure bool

	// SendData is a byte slice that will be sent to the peer. If nil a
	// generic error will be sent.
	SendData []byte
}

// A compile time check to ensure LinkFailureError implements the error
// interface.
var _ error = (*LinkFailureError)(nil)

// Error returns a generic error for the LinkFailureError.
//
// NOTE: Part of the error interface.
func (e LinkFailureError) Error() string {
	switch e.code {
	case ErrInternalError:
		return "internal error"
	case ErrRemoteError:
		return "remote error"
	case ErrRemoteUnresponsive:
		return "remote unresponsive"
	case ErrSyncError:
		return "sync error"
	case ErrInvalidUpdate:
		return "invalid update"
	case ErrInvalidCommitment:
		return "invalid commitment"
	case ErrInvalidRevocation:
		return "invalid revocation"
	case ErrRecoveryError:
		return "unable to resume channel, recovery required"
	default:
		return "unknown error"
	}
}

// ShouldSendToPeer indicates whether we should send an error to the peer if
// the link fails with this LinkFailureError.
func (e LinkFailureError) ShouldSendToPeer() bool {
	switch e.code {

	// Since sending an error can lead some nodes to force close the
	// channel, create a whitelist of the failures we want to send so that
	// newly added error codes aren't automatically sent to the remote peer.
	case
		ErrInternalError,
		ErrRemoteError,
		ErrSyncError,
		ErrInvalidUpdate,
		ErrInvalidCommitment,
		ErrInvalidRevocation,
		ErrRecoveryError:

		return true

	// In all other cases we will not attempt to send our peer an error.
	default:
		return false
	}
}
