package htlcswitch

import "github.com/go-errors/errors"

var (
	// ErrLinkShuttingDown signals that the link is shutting down.
	ErrLinkShuttingDown = errors.New("link shutting down")
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
	// If the failure is a result of the peer sending us an error, we don't
	// have to respond with one.
	case ErrRemoteError:
		return false

	// In all other cases we will attempt to send our peer an error message.
	default:
		return true
	}
}
