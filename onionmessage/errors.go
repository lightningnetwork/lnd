package onionmessage

import "errors"

var (
	// ErrActortShuttingDown is returned by the actor logic when its context
	// is cancelled.
	ErrActorShuttingDown = errors.New("actor shutting down")
)
