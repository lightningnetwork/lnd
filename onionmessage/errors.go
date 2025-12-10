package onionmessage

import "errors"

var (
	// ErrActortShuttingDown is returned by the actor logic when its context
	// is cancelled.
	ErrActorShuttingDown = errors.New("actor shutting down")

	// ErrNextNodeIdEmpty is returned when the next node ID is missing from
	// the route data.
	ErrNextNodeIdEmpty = errors.New("next node ID empty")

	// ErrNilReceptionist is returned when a nil receptionist is provided.
	ErrNilReceptionist = errors.New("receptionist cannot be nil")

	// ErrNilRouter is returned when a nil router is provided.
	ErrNilRouter = errors.New("router cannot be nil")

	// ErrNilResolver is returned when a nil resolver is provided.
	ErrNilResolver = errors.New("resolver cannot be nil")
)
