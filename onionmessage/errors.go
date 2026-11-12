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

	// ErrSCIDNotResolved is returned when a next-hop short channel ID is
	// not an announced channel or a local alias of an active channel.
	ErrSCIDNotResolved = errors.New("short channel ID not resolved")

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

	// ErrInvalidForwardPayload is returned when the payload of a
	// forwarding onion message carries a record other than the encrypted
	// recipient data.
	ErrInvalidForwardPayload = errors.New("invalid forwarding onion " +
		"message payload")

	// ErrMissingEncryptedData is returned when the payload of a forwarding
	// onion message has no encrypted recipient data.
	ErrMissingEncryptedData = errors.New("missing encrypted recipient " +
		"data")

	// ErrForwardPathID is returned when the decrypted recipient data of a
	// forwarding onion message contains a path ID, which only the final
	// hop may receive.
	ErrForwardPathID = errors.New("path ID in forwarding onion message")
)
