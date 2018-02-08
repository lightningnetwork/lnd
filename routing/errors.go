package routing

import "github.com/go-errors/errors"

// errorCode is used to represent the various errors that can occur within this
// package.
type errorCode uint8

const (
	// ErrNoPathFound is returned when a path to the target destination
	// does not exist in the graph.
	ErrNoPathFound errorCode = iota

	// ErrNoRouteFound is returned when the router is unable to find a
	// valid route to the target destination after fees and time-lock
	// limitations are factored in.
	ErrNoRouteFound

	// ErrInsufficientCapacity is returned when a path if found, yet the
	// capacity of one of the channels in the path is insufficient to carry
	// the payment.
	ErrInsufficientCapacity

	// ErrMaxHopsExceeded is returned when a candidate path is found, but
	// the length of that path exceeds HopLimit.
	ErrMaxHopsExceeded

	// ErrTargetNotInNetwork is returned when the target of a path-finding
	// or payment attempt isn't known to be within the current version of
	// the channel graph.
	ErrTargetNotInNetwork

	// ErrOutdated is returned when the routing update already have
	// been applied.
	ErrOutdated

	// ErrIgnored is returned when the update have been ignored because
	// this update can't bring us something new, or because a node
	// announcement was given for node not found in any channel.
	ErrIgnored

	// ErrPaymentAttemptTimeout is an error that indicates that a payment
	// attempt timed out before we were able to successfully route an HTLC.
	ErrPaymentAttemptTimeout
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

// newErr creates a routerError by the given error description and its
// corresponding error code.
func newErr(code errorCode, a interface{}) *routerError {
	return &routerError{
		code: code,
		err:  errors.New(a),
	}
}

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
