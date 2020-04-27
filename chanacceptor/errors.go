package chanacceptor

import (
	"errors"
	"fmt"
)

const (
	// Length of the prefix message in `ErrRejectedWithMsg` + the max. length of
	// custom message allowed to be sent to the remote peer.
	trimmedAcceptanceErrorLength = 31 + 128
)

// AcceptanceError wraps errors returned by ChannelAcceptor's Accept method.
// Node operators can set up custom ChannelAcceptor's and reject
// ChannelAcceptRequest's with specific rejection message. AcceptanceError is
// used to wrap such message in an error and send its trimmed version
// (see `TrimmedError` method) to the remote peer.
type AcceptanceError struct {
	error
}

// TrimmedError returns only first 159 bytes of the error message. Trimmed error
// is sent to the remote peer instead of the full message to prevent sending of
// very long messages.
func (e AcceptanceError) TrimmedError() string {
	if len(e.Error()) > trimmedAcceptanceErrorLength {
		return e.Error()[:trimmedAcceptanceErrorLength] + "..."
	}

	return e.Error()
}

// A compile time check to ensure AcceptanceError implements the error
// interface.
var _ error = (*AcceptanceError)(nil)

// ErrRejected returns a general open channel error. It is sent to a remote node
// when a custom ChannelAcceptor rejected an open channel request and didn't
// specify any rejection message.
func ErrRejected() AcceptanceError {
	return AcceptanceError{
		errors.New("open channel request rejected"),
	}
}

// ErrRejectedWithMsg returns a custom open channel error. It is used by the
// funding manager to wrap and send rejection error messages to the remote peer.
// Only first 128 bytes of `rejectionMsg` will be sent to the remote peer.
func ErrRejectedWithMsg(rejectionMsg string) AcceptanceError {
	return AcceptanceError{
		fmt.Errorf("open channel request rejected: %s", rejectionMsg),
	}
}
