package lnwire

import "github.com/btcsuite/btcd/chaincfg/chainhash"

// AnnounceSignatures is an interface that represents a message used to
// exchange signatures of a ChannelAnnouncment message during the funding flow.
type AnnounceSignatures interface {
	// SCID returns the ShortChannelID of the channel.
	SCID() ShortChannelID

	// ChanID returns the ChannelID identifying the channel.
	ChanID() ChannelID

	Message
}

// ChannelAnnouncement is an interface that must be satisfied by any message
// used to announce and prove the existence of a channel.
type ChannelAnnouncement interface {
	// SCID returns the short channel ID of the channel.
	SCID() ShortChannelID

	// GetChainHash returns the hash of the chain which this channel's
	// funding transaction is confirmed in.
	GetChainHash() chainhash.Hash

	// Node1KeyBytes returns the bytes representing the public key of node
	// 1 in the channel.
	Node1KeyBytes() [33]byte

	// Node2KeyBytes returns the bytes representing the public key of node
	// 2 in the channel.
	Node2KeyBytes() [33]byte

	Message
}
