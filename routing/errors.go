package routing

import "errors"

var (
	// ErrNoPathFound is returned when a path to the target destination
	// does not exist in the graph.
	ErrNoPathFound = errors.New("unable to find a path to " +
		"destination")

	// ErrNoRouteFound is returned when the router is unable to find a
	// valid route to the target destination after fees and time-lock
	// limitations are factored in.
	ErrNoRouteFound = errors.New("unable to find a eligible route to " +
		"the destination")

	// ErrInsufficientCapacity is returned when a path if found, yet the
	// capacity of one of the channels in the path is insufficient to carry
	// the payment.
	ErrInsufficientCapacity = errors.New("channel graph has " +
		"insufficient capacity for the payment")

	// ErrMaxHopsExceeded is returned when a candidate path is found, but
	// the length of that path exceeds HopLimit.
	ErrMaxHopsExceeded = errors.New("potential path has too many hops")

	// ErrTargetNotInNetwork is returned when the target of a path-finding
	// or payment attempt isn't known to be within the current version of
	// the channel graph.
	ErrTargetNotInNetwork = errors.New("target not found")
)
