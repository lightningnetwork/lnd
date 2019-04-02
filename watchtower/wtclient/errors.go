package wtclient

import "errors"

var (
	// ErrClientExiting signals that the watchtower client is shutting down.
	ErrClientExiting = errors.New("watchtower client shutting down")

	// ErrTowerCandidatesExhausted signals that a TowerCandidateIterator has
	// cycled through all available candidates.
	ErrTowerCandidatesExhausted = errors.New("exhausted all tower " +
		"candidates")

	// ErrPermanentTowerFailure signals that the tower has reported that it
	// has permanently failed or the client believes this has happened based
	// on the tower's behavior.
	ErrPermanentTowerFailure = errors.New("permanent tower failure")

	// ErrNegotiatorExiting signals that the SessionNegotiator is shutting
	// down.
	ErrNegotiatorExiting = errors.New("negotiator exiting")

	// ErrNoTowerAddrs signals that the client could not be created because
	// we have no addresses with which we can reach a tower.
	ErrNoTowerAddrs = errors.New("no tower addresses")

	// ErrFailedNegotiation signals that the session negotiator could not
	// acquire a new session as requested.
	ErrFailedNegotiation = errors.New("session negotiation unsuccessful")

	// ErrUnregisteredChannel signals that the client was unable to backup a
	// revoked state becuase the channel had not been previously registered
	// with the client.
	ErrUnregisteredChannel = errors.New("channel is not registered")
)
