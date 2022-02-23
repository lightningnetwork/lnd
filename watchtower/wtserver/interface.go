package wtserver

import (
	"io"
	"net"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
)

// Interface represents a simple, listen-only service that accepts watchtower
// clients, and provides responses to their requests.
type Interface interface {
	// InboundPeerConnected accepts a new watchtower client, and handles any
	// requests sent by the peer.
	InboundPeerConnected(Peer)

	// Start sets up the watchtower server.
	Start() error

	// Stop cleans up the watchtower's current connections and resources.
	Stop() error
}

// Peer is the primary interface used to abstract watchtower clients.
type Peer interface {
	io.WriteCloser

	// ReadNextMessage pulls the next framed message from the client.
	ReadNextMessage() ([]byte, error)

	// SetWriteDeadline specifies the time by which the client must have
	// read a message sent by the server. In practice, the connection is
	// buffered, so the client must read enough from the connection to
	// support the server adding another reply.
	SetWriteDeadline(time.Time) error

	// SetReadDeadline specifies the time by which the client must send
	// another message.
	SetReadDeadline(time.Time) error

	// RemotePub returns the client's public key.
	RemotePub() *btcec.PublicKey

	// RemoteAddr returns the client's network address.
	RemoteAddr() net.Addr
}

// DB provides the server access to session creation and retrieval, as well as
// persisting state updates sent by clients.
type DB interface {
	// InsertSessionInfo saves a newly agreed-upon session from a client.
	// This method should fail if a session with the same session id already
	// exists.
	InsertSessionInfo(*wtdb.SessionInfo) error

	// GetSessionInfo retrieves the SessionInfo associated with the session
	// id, if it exists.
	GetSessionInfo(*wtdb.SessionID) (*wtdb.SessionInfo, error)

	// InsertStateUpdate persists a state update sent by a client, and
	// validates the update against the current SessionInfo stored under the
	// update's session id..
	InsertStateUpdate(*wtdb.SessionStateUpdate) (uint16, error)

	// DeleteSession removes all data associated with a particular session
	// id from the tower's database.
	DeleteSession(wtdb.SessionID) error
}
