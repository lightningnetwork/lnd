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

// CompareResult represents the result after comparing two things.
type CompareResult uint8

const (
	// LessThan indicates that base object is less than the object it was
	// compared to.
	LessThan CompareResult = iota

	// EqualTo indicates that the base object is equal to the object it was
	// compared to.
	EqualTo

	// GreaterThan indicates that base object is greater than the object it
	// was compared to.
	GreaterThan
)

// ChannelUpdate is an interface that describes a message used to update the
// forwarding rules of a channel.
type ChannelUpdate interface {
	// SCID returns the ShortChannelID of the channel that the update
	// applies to.
	SCID() ShortChannelID

	// IsNode1 is true if the update was produced by node 1 of the channel
	// peers. Node 1 is the node with the lexicographically smaller public
	// key.
	IsNode1() bool

	// IsDisabled is true if the update is announcing that the channel
	// should be considered disabled.
	IsDisabled() bool

	// GetChainHash returns the hash of the chain that the message is
	// referring to.
	GetChainHash() chainhash.Hash

	// ForwardingPolicy returns the set of forwarding constraints of the
	// update.
	ForwardingPolicy() *ForwardingPolicy

	// CmpAge can be used to determine if the update is older or newer than
	// the passed update. It returns LessThan if this update is older than
	// the passed update, GreaterThan if it is newer and EqualTo if they are
	// the same age.
	CmpAge(update ChannelUpdate) (CompareResult, error)

	// SetDisabledFlag can be used to adjust the disabled flag of an update.
	SetDisabledFlag(bool)

	// SetSCID can be used to overwrite the SCID of the update.
	SetSCID(scid ShortChannelID)

	Message
}

// NodeAnnouncement is an interface that must be satisfied by any message used
// to announce the existence of a node.
type NodeAnnouncement interface {
	// NodePub returns the identity public key of the node.
	NodePub() [33]byte

	// NodeFeatures returns the set of features supported by the node.
	NodeFeatures() *FeatureVector

	// TimestampDesc returns a human-readable description of the
	// timestamp of the announcement.
	TimestampDesc() string

	Message
}

// ForwardingPolicy defines the set of forwarding constraints advertised in a
// ChannelUpdate message.
type ForwardingPolicy struct {
	// TimeLockDelta is the minimum number of blocks that the node requires
	// to be added to the expiry of HTLCs. This is a security parameter
	// determined by the node operator. This value represents the required
	// gap between the time locks of the incoming and outgoing HTLC's set
	// to this node.
	TimeLockDelta uint16

	// BaseFee is the base fee that must be used for incoming HTLC's to
	// this particular channel. This value will be tacked onto the required
	// for a payment independent of the size of the payment.
	BaseFee MilliSatoshi

	// FeeRate is the fee rate that will be charged per millionth of a
	// satoshi.
	FeeRate MilliSatoshi

	// HtlcMinimumMsat is the minimum HTLC value which will be accepted.
	MinHTLC MilliSatoshi

	// HasMaxHTLC is true if the MaxHTLC field is provided in the update.
	HasMaxHTLC bool

	// HtlcMaximumMsat is the maximum HTLC value which will be accepted.
	MaxHTLC MilliSatoshi
}
