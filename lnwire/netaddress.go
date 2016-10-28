package lnwire

import (
	"fmt"
	"net"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
)

// ServiceFlag identifies the services supported by a particular Lightning
// Network peer.
type ServiceFlag uint64

// NetAddress represents information pertaining to the identity and network
// reachability of a peer. Information stored includes the node's identity
// public key for establishing a confidential+authenticated connection, the
// service bits it supports, and a TCP address the node is reachable at.
//
// TODO(roasbeef): merge with LinkNode in some fashion
type NetAddress struct {
	// IdentityKey is the long-term static public key for a node. This node is
	// used throughout the network as a node's identity key. It is used to
	// authenticate any data sent to the network on behalf of the node, and
	// additionally to establish a confidential+authenticated connection with
	// the node.
	IdentityKey *btcec.PublicKey

	// Services defines the set of services supported by the node reachable at
	// the address and identity key defined in the struct.
	Services ServiceFlag

	// Address is is the IP address and port of the node.
	Address *net.TCPAddr

	// ChainNet is the Bitcoin network this node is associated with.
	// TODO(roasbeef): make a slice in the future for multi-chain
	ChainNet wire.BitcoinNet
}

// String returns a human readable string describing the target NetAddress. The
// current string format is: <pubkey>@host.
func (n *NetAddress) String() string {
	// TODO(roasbeef): use base58?
	pubkey := n.IdentityKey.SerializeCompressed()

	return fmt.Sprintf("%x@%v", pubkey, n.Address)
}
