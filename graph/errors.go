package graph

import "github.com/go-errors/errors"

var (
	// ErrNoFundingTransaction is returned when we are unable to find the
	// funding transaction described by the short channel ID on chain.
	ErrNoFundingTransaction = errors.New(
		"unable to find the funding transaction",
	)

	// ErrInvalidFundingOutput is returned if the channel funding output
	// fails validation.
	ErrInvalidFundingOutput = errors.New(
		"channel funding output validation failed",
	)

	// ErrChannelSpent is returned when we go to validate a channel, but
	// the purported funding output has actually already been spent on
	// chain.
	ErrChannelSpent = errors.New("channel output has been spent")
)

// ErrorCode is used to represent the various errors that can occur within this
// package.
type ErrorCode uint8

const (
	// ErrOutdated is returned when the routing update already have
	// been applied, or a newer update is already known.
	ErrOutdated ErrorCode = iota

	// ErrIgnored is returned when the update have been ignored because
	// this update can't bring us something new, or because a node
	// announcement was given for node not found in any channel.
	ErrIgnored
)

// Error is a structure that represent the error inside the graph package,
// this structure carries additional information about error code in order to
// be able distinguish errors outside of the current package.
type Error struct {
	err  *errors.Error
	code ErrorCode
}

// Error represents errors as the string
// NOTE: Part of the error interface.
func (e *Error) Error() string {
	return e.err.Error()
}

// A compile time check to ensure Error implements the error interface.
var _ error = (*Error)(nil)

// NewErrf creates a Error by the given error formatted description and
// its corresponding error code.
func NewErrf(code ErrorCode, format string, a ...interface{}) *Error {
	return &Error{
		code: code,
		err:  errors.Errorf(format, a...),
	}
}

// IsError is a helper function which is needed to have ability to check that
// returned error has specific error code.
func IsError(e interface{}, codes ...ErrorCode) bool {
	err, ok := e.(*Error)
	if !ok {
		return false
	}

	for _, code := range codes {
		if err.code == code {
			return true
		}
	}

	return false
}
