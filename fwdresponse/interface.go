package fwdresponse

import (
	"errors"

	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrNotFound identifies a clean forwarding-response store miss.
	// Callers must not treat other storage errors as proof that no response
	// exists.
	ErrNotFound = errors.New("forwarding response not found")

	// ErrUnknownType signals that a stored response deserialized into a
	// message that is neither a settle nor a fail.
	ErrUnknownType = errors.New("unknown forwarding response type")
)

// Response is an off-chain settle or fail retained under its outgoing circuit
// key. This gives forwarding responses the same circuit-oriented lookup model
// used by contractcourt resolution messages.
type Response struct {
	// OutKey identifies the outgoing HTLC that this response resolves.
	OutKey models.CircuitKey

	// Message is the wire response received from the outgoing peer. Keeping
	// the message intact allows the switch to use its regular response path
	// during replay.
	Message lnwire.Message
}

// Store persists off-chain forwarding responses until the switch can safely
// replay or reap them.
type Store interface {
	// FetchFwdResponses returns every persisted forwarding response.
	FetchFwdResponses() ([]*Response, error)

	// CheckFwdResponse returns nil if a response exists for the
	// outgoing circuit key, ErrNotFound if it is absent, and storage
	// errors unchanged.
	CheckFwdResponse(outKey *models.CircuitKey) error

	// DeleteFwdResponse removes the response for the outgoing circuit key.
	DeleteFwdResponse(outKey *models.CircuitKey) error
}

// KVStore extends the backend-independent forwarding-response store with the
// operations that must participate in a caller-owned KV transaction. Keeping
// these operations separate prevents runtime consumers from depending on KV
// transaction types while allowing channel cleanup and database wipes to
// remain atomic.
type KVStore interface {
	Store

	// KVFwdResponseExtractor participates in atomic channel cleanup.
	chanstate.KVFwdResponseExtractor

	// Wipe removes all forwarding responses in the transaction.
	Wipe(tx kvdb.RwTx) error
}
