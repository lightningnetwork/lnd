package wtclient

import (
	"errors"
)

var (
	// ErrClientExiting signals that the watchtower client is shutting down.
	ErrClientExiting = errors.New("watchtower client shutting down")

	// ErrTowerCandidatesExhausted signals that a TowerCandidateIterator has
	// cycled through all available candidates.
	ErrTowerCandidatesExhausted = errors.New("exhausted all tower " +
		"candidates")

	// ErrTowerNotInIterator is returned when a requested tower was not
	// found in the iterator.
	ErrTowerNotInIterator = errors.New("tower not in iterator")

	// ErrPermanentTowerFailure signals that the tower has reported that it
	// has permanently failed or the client believes this has happened based
	// on the tower's behavior.
	ErrPermanentTowerFailure = errors.New("permanent tower failure")

	// ErrNegotiatorExiting signals that the SessionNegotiator is shutting
	// down.
	ErrNegotiatorExiting = errors.New("negotiator exiting")

	// ErrFailedNegotiation signals that the session negotiator could not
	// acquire a new session as requested.
	ErrFailedNegotiation = errors.New("session negotiation unsuccessful")

	// ErrUnregisteredChannel signals that the client was unable to backup a
	// revoked state because the channel had not been previously registered
	// with the client.
	ErrUnregisteredChannel = errors.New("channel is not registered")

	// ErrSessionKeyAlreadyUsed indicates that the client attempted to
	// create a new session with a tower with a session key that has already
	// been used in the past.
	ErrSessionKeyAlreadyUsed = errors.New("session key already used")
)
