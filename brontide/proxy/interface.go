package proxy

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/peer"
)

// MessageConnWithPubkey matches both *brontide.Conn and *Conn to read and
// write messages as well as tell the identity of the connection's remote peer.
//
// TODO(aakselrod): Move this to peer package?
type MessageConnWithPubkey interface {
	// Inherit from net.Conn
	peer.MessageConn

	// LocalPub returns the connection's identity for the local peer.
	LocalPub() *btcec.PublicKey

	// RemotePub returns the connection's identity for the remote peer.
	RemotePub() *btcec.PublicKey
}
