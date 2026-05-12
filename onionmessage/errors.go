package onionmessage

import "errors"

var (
	// ErrActorShuttingDown is returned by the actor logic when its context
	// is cancelled.
	ErrActorShuttingDown = errors.New("actor shutting down")

	// ErrNextNodeIdEmpty is returned when the next node ID is missing from
	// the route data.
	ErrNextNodeIdEmpty = errors.New("next node ID empty")

	// ErrSCIDEmpty is returned when the short channel ID is missing from
	// the route data.
	ErrSCIDEmpty = errors.New("short channel ID empty")

	// ErrSamePeerCycle is returned when a forwarding onion message
	// would be sent back to the same peer it was received from.
	ErrSamePeerCycle = errors.New("onion message cycle: next " +
		"hop is the sending peer")
	// ErrNoPathFound is returned when no path exists between the source
	// and destination nodes that supports onion messaging.
	ErrNoPathFound = errors.New("no path found to destination")

	// ErrDestinationNoOnionSupport is returned when the destination node
	// does not advertise support for onion messages.
	ErrDestinationNoOnionSupport = errors.New("destination does not " +
		"support onion messages")

	// ErrNodeNotFound is returned when the node is not found in the graph.
	ErrNodeNotFound = errors.New("node not found in graph")
)
