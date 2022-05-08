package watchtowerrpc

import (
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
)

// WatchtowerBackend abstracts access to the watchtower information that is
// served via RPC connections.
type WatchtowerBackend interface {
	// PubKey returns the public key for the watchtower used to
	// authentication and encrypt traffic with clients.
	PubKey() *btcec.PublicKey

	// ListeningAddrs returns the listening addresses where the watchtower
	// server can accept client connections.
	ListeningAddrs() []net.Addr

	// ExternalIPs returns the addresses where the watchtower can be reached
	// by clients externally.
	ExternalIPs() []net.Addr
}
