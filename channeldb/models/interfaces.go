package models

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
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

// ChannelEdgePolicy is an interface that describes an update to the forwarding
// rules of a channel.
type ChannelEdgePolicy interface {
	// SCID returns the short channel ID of the channel being referred to.
	SCID() lnwire.ShortChannelID

	// IsDisabled returns true if the update is indicating that the channel
	// should be considered disabled.
	IsDisabled() bool

	// IsNode1 returns true if the update was constructed by node 1 of the
	// channel.
	IsNode1() bool

	// GetToNode returns the pub key of the node that did not produce the
	// update.
	GetToNode() [33]byte

	// ForwardingPolicy return the various forwarding policy rules set by
	// the update.
	ForwardingPolicy() *lnwire.ForwardingPolicy

	// Before compares this update against the passed update and returns
	// true if this update has a lower timestamp than the passed one.
	Before(policy ChannelEdgePolicy) (bool, error)

	// AfterUpdateMsg compares this update against the passed
	// lnwire.ChannelUpdate message and returns true if this update is
	// newer than the passed one.
	// TODO(elle): combine with Before?
	AfterUpdateMsg(msg lnwire.ChannelUpdate) (bool, error)

	// Sig returns the signature of the update message.
	Sig() (input.Signature, error)
}
