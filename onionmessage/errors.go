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

	// ErrNoPathFound is returned when no path exists between the source
	// and destination nodes that supports onion messaging.
	ErrNoPathFound = errors.New("no path found to destination")

	// ErrCannotSendToSelf is returned when the caller tries to send an
	// onion message to this node itself.
	ErrCannotSendToSelf = errors.New("cannot send onion message to self")

	// ErrDestinationNoOnionSupport is returned when the destination node
	// does not advertise support for onion messages.
	ErrDestinationNoOnionSupport = errors.New("destination does not " +
		"support onion messages")

	// ErrNodeNotFound is returned when the node is not found in the graph.
	ErrNodeNotFound = errors.New("node not found in graph")

	// ErrNoHopsProvided is returned when sendViaPath is called with an
	// empty hop list.
	ErrNoHopsProvided = errors.New("no hops provided")
)
