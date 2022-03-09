package lnwire

import (
	"fmt"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
)

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

	// Address is the IP address and port of the node. This is left
	// general so that multiple implementations can be used.
	Address net.Addr

	// ChainNet is the Bitcoin network this node is associated with.
	// TODO(roasbeef): make a slice in the future for multi-chain
	ChainNet wire.BitcoinNet
}

// A compile time assertion to ensure that NetAddress meets the net.Addr
// interface.
var _ net.Addr = (*NetAddress)(nil)

// String returns a human readable string describing the target NetAddress. The
// current string format is: <pubkey>@host.
//
// This part of the net.Addr interface.
func (n *NetAddress) String() string {
	// TODO(roasbeef): use base58?
	pubkey := n.IdentityKey.SerializeCompressed()

	return fmt.Sprintf("%x@%v", pubkey, n.Address)
}

// Network returns the name of the network this address is bound to.
//
// This part of the net.Addr interface.
func (n *NetAddress) Network() string {
	return n.Address.Network()
}
