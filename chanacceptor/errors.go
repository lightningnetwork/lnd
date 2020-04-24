package chanacceptor

import (
	"errors"
	"fmt"
)

// AcceptanceError wraps errors returned by ChannelAcceptor's Accept method.
// Node operators can set up custom ChannelAcceptor's and reject
// ChannelAcceptRequest's with specific rejection message. AcceptanceError is
// used to wrap such message as an error and send it to the node willing to open
// a channel.
type AcceptanceError struct {
	error
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
// funding manager to wrap and send rejection error messages to remote nodes.
func ErrRejectedWithMsg(rejectionMsg string) AcceptanceError {
	return AcceptanceError{
		fmt.Errorf("open channel request rejected: %s", rejectionMsg),
	}
}
