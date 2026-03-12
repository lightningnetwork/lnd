package onionmessage

import (
	"context"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
)

// OnionRouter wraps the sphinx router operations needed for onion message
// processing.
type OnionRouter interface {
	// ProcessOnionPacket processes an onion packet and returns the
	// processed result.
	ProcessOnionPacket(pkt *sphinx.OnionPacket, assocData []byte,
		incomingCltv uint32,
		opts ...sphinx.ProcessOnionOpt) (*sphinx.ProcessedPacket, error)

	// DecryptBlindedHopData decrypts the encrypted hop data using the
	// given path key.
	DecryptBlindedHopData(pathKey *btcec.PublicKey,
		encData []byte) ([]byte, error)

	// NextEphemeral derives the next ephemeral key from the current path
	// key.
	NextEphemeral(
		currentPathKey *btcec.PublicKey) (*btcec.PublicKey, error)
}

// OnionMessageUpdateDispatcher dispatches onion message updates to
// subscribers.
type OnionMessageUpdateDispatcher interface {
	// SendUpdate sends an onion message update to all subscribers.
	SendUpdate(update any) error
}

// PeerMessageSender sends onion messages to peers identified by public key.
type PeerMessageSender interface {
	// SendToPeer sends an onion message to the peer identified by the
	// given compressed public key.
	SendToPeer(pubKey [33]byte, msg *lnwire.OnionMessage) error
}

// GraphSessionProvider provides a read-only graph session for pathfinding.
// The callback receives a NodeTraverser scoped to the session's lifetime.
type GraphSessionProvider interface {
	// GraphSession calls cb with a NodeTraverser backed by a read-only
	// graph session. reset is called between retries if the session must
	// be restarted.
	GraphSession(ctx context.Context,
		cb func(graphdb.NodeTraverser) error, reset func()) error
}
