package models

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// ChannelEdgeInfo is an interface that describes a channel announcement.
type ChannelEdgeInfo interface { //nolint:interfacebloat
	// GetChainHash returns the hash of the genesis block of the chain that
	// the edge is on.
	GetChainHash() chainhash.Hash

	// GetChanID returns the channel ID.
	GetChanID() uint64

	// GetAuthProof returns the ChannelAuthProof for the edge.
	GetAuthProof() ChannelAuthProof

	// GetCapacity returns the capacity of the channel.
	GetCapacity() btcutil.Amount

	// SetAuthProof sets the proof of the channel.
	SetAuthProof(ChannelAuthProof) error

	// NodeKey1 returns the public key of node 1.
	NodeKey1() (*btcec.PublicKey, error)

	// NodeKey2 returns the public key of node 2.
	NodeKey2() (*btcec.PublicKey, error)

	// Node1Bytes returns bytes of the public key of node 1.
	Node1Bytes() [33]byte

	// Node2Bytes returns bytes the public key of node 2.
	Node2Bytes() [33]byte

	// GetChanPoint returns the outpoint of the funding transaction of the
	// channel.
	GetChanPoint() wire.OutPoint

	// FundingScript returns the pk script for the funding output of the
	// channel.
	FundingScript() ([]byte, error)

	// Copy returns a copy of the ChannelEdgeInfo.
	Copy() ChannelEdgeInfo
}

// ChannelAuthProof is an interface that describes the proof of ownership of
// a channel.
type ChannelAuthProof interface {
	// isChanAuthProof is a no-op method used to ensure that a struct must
	// explicitly inherit this interface to be considered a
	// ChannelAuthProof type.
	isChanAuthProof()
}
