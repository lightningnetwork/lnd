package routing

import "errors"

var (
	// ErrNoPathFound is returned when a path to the target destination
	// does not exist in the graph.
	ErrNoPathFound = errors.New("unable to find a path to " +
		"destination")

	// ErrInsufficientCapacity is returned when a path if found, yet the
	// capacity of one of the channels in the path is insufficient to carry
	// the payment.
	ErrInsufficientCapacity = errors.New("channel graph has " +
		"insufficient capacity for the payment")

	// ErrMaxHopsExceeded is returned when a candidate path is found, but
	// the length of that path exceeds HopLimit.
	ErrMaxHopsExceeded = errors.New("potential path has too many hops")

	// ErrTargetNotInNetwork is returned when a
	ErrTargetNotInNetwork = errors.New("target not found")
)
