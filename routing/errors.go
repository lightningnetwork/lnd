package routing

import "github.com/go-errors/errors"

// errorCode is used to represent the various errors that can occur within this
// package.
type errorCode uint8

const (
	// ErrOutdated is returned when the routing update already have
	// been applied, or a newer update is already known.
	ErrOutdated errorCode = iota

	// ErrIgnored is returned when the update have been ignored because
	// this update can't bring us something new, or because a node
	// announcement was given for node not found in any channel.
	ErrIgnored

	// ErrChannelSpent is returned when we go to validate a channel, but
	// the purported funding output has actually already been spent on
	// chain.
	ErrChannelSpent

	// ErrNoFundingTransaction is returned when we are unable to find the
	// funding transaction described by the short channel ID on chain.
	ErrNoFundingTransaction

	// ErrInvalidFundingOutput is returned if the channel funding output
	// fails validation.
	ErrInvalidFundingOutput

	// ErrVBarrierShuttingDown signals that the barrier has been requested
	// to shutdown, and that the caller should not treat the wait condition
	// as fulfilled.
	ErrVBarrierShuttingDown

	// ErrParentValidationFailed signals that the validation of a
	// dependent's parent failed, so the dependent must not be processed.
	ErrParentValidationFailed
)

// routerError is a structure that represent the error inside the routing package,
// this structure carries additional information about error code in order to
// be able distinguish errors outside of the current package.
type routerError struct {
	err  *errors.Error
	code errorCode
}

// Error represents errors as the string
// NOTE: Part of the error interface.
func (e *routerError) Error() string {
	return e.err.Error()
}

// A compile time check to ensure routerError implements the error interface.
var _ error = (*routerError)(nil)

// newErrf creates a routerError by the given error formatted description and
// its corresponding error code.
func newErrf(code errorCode, format string, a ...interface{}) *routerError {
	return &routerError{
		code: code,
		err:  errors.Errorf(format, a...),
	}
}

// IsError is a helper function which is needed to have ability to check that
// returned error has specific error code.
func IsError(e interface{}, codes ...errorCode) bool {
	err, ok := e.(*routerError)
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
