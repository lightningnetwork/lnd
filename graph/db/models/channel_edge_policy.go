package models

import (
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ChannelEdgePolicy represents a *directed* edge within the channel graph. For
// each channel in the database, there are two distinct edges: one for each
// possible direction of travel along the channel. The edges themselves hold
// information concerning fees, and minimum time-lock information which is
// utilized during path finding.
type ChannelEdgePolicy struct {
	// Version is the gossip version of the channel update that produced
	// this policy.
	Version lnwire.GossipVersion

	// SigBytes is the raw bytes of the signature of the channel edge
	// policy. We'll only parse these if the caller needs to access the
	// signature for validation purposes.
	SigBytes []byte

	// ChannelID is the unique channel ID for the channel. The first 3
	// bytes are the block height, the next 3 the index within the block,
	// and the last 2 bytes are the output index for the channel.
	ChannelID uint64

	// LastUpdate is the last time an authenticated edge for this channel
	// was received.
	LastUpdate time.Time

	// LastBlockHeight is the block height that timestamps the last update
	// for v2 channel updates.
	LastBlockHeight uint32

	// SecondPeer indicates whether this policy was announced by the second
	// peer in the channel for v2 channel updates.
	SecondPeer bool

	// MessageFlags is a bitfield which indicates the presence of optional
	// fields (like max_htlc) in the policy.
	MessageFlags lnwire.ChanUpdateMsgFlags

	// ChannelFlags is a bitfield which signals the capabilities of the
	// channel as well as the directed edge this update applies to.
	ChannelFlags lnwire.ChanUpdateChanFlags

	// DisableFlags is a v2-specific bitfield which signals whether the
	// channel is disabled for incoming or outgoing traffic.
	DisableFlags lnwire.ChanUpdateDisableFlags

	// TimeLockDelta is the number of blocks this node will subtract from
	// the expiry of an incoming HTLC. This value expresses the time buffer
	// the node would like to HTLC exchanges.
	TimeLockDelta uint16

	// MinHTLC is the smallest value HTLC this node will forward, expressed
	// in millisatoshi.
	MinHTLC lnwire.MilliSatoshi

	// MaxHTLC is the largest value HTLC this node will forward, expressed
	// in millisatoshi.
	MaxHTLC lnwire.MilliSatoshi

	// FeeBaseMSat is the base HTLC fee that will be charged for forwarding
	// ANY HTLC, expressed in mSAT's.
	FeeBaseMSat lnwire.MilliSatoshi

	// FeeProportionalMillionths is the rate that the node will charge for
	// HTLCs for each millionth of a satoshi forwarded.
	FeeProportionalMillionths lnwire.MilliSatoshi

	// ToNode is the public key of the node that this directed edge leads
	// to. Using this pub key, the channel graph can further be traversed.
	ToNode [33]byte

	// InboundFee is the fee that must be paid for incoming HTLCs.
	//
	// NOTE: for our kvdb implementation of the graph store, inbound fees
	// are still only persisted as part of extra opaque data and so this
	// field is not explicitly stored but is rather populated from the
	// ExtraOpaqueData field on deserialization. For our SQL implementation,
	// this field will be explicitly persisted in the database.
	InboundFee fn.Option[lnwire.Fee]

	// ExtraOpaqueData is the set of data that was appended to this
	// message, some of which we may not actually know how to iterate or
	// parse. By holding onto this data, we ensure that we're able to
	// properly validate the set of signatures that cover these new fields,
	// and ensure we're able to make upgrades to the network in a forwards
	// compatible manner.
	ExtraOpaqueData lnwire.ExtraOpaqueData

	// ExtraSignedFields are the extra signed fields found in v2 channel
	// updates.
	ExtraSignedFields map[uint64][]byte
}

// IsNode1 returns true if this policy was announced by the channel's node_1.
func (c *ChannelEdgePolicy) IsNode1() bool {
	if c.Version == lnwire.GossipVersion1 {
		return c.ChannelFlags&lnwire.ChanUpdateDirection == 0
	}

	return !c.SecondPeer
}

// IsDisabled determines whether the edge has the disabled bit set.
//
// NOTE: for v2 channel updates, we return true here only if both the incoming
// and outgoing disabled bits are set.
func (c *ChannelEdgePolicy) IsDisabled() bool {
	if c.Version == lnwire.GossipVersion1 {
		return c.ChannelFlags.IsDisabled()
	}

	return !c.DisableFlags.IsEnabled()
}

// ComputeFee computes the fee to forward an HTLC of `amt` milli-satoshis over
// the passed active payment channel. This value is currently computed as
// specified in BOLT07, but will likely change in the near future.
func (c *ChannelEdgePolicy) ComputeFee(
	amt lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	return c.FeeBaseMSat + (amt*c.FeeProportionalMillionths)/feeRateParts
}

// String returns a human-readable version of the channel edge policy.
func (c *ChannelEdgePolicy) String() string {
	if c.Version == lnwire.GossipVersion1 {
		return fmt.Sprintf("ChannelID=%v, MessageFlags=%v, "+
			"ChannelFlags=%v, LastUpdate=%v", c.ChannelID,
			c.MessageFlags, c.ChannelFlags, c.LastUpdate)
	}

	return fmt.Sprintf("ChannelID=%v, Node1=%v, DisableFlags=%v, "+
		"BlockHeight=%v", c.ChannelID, !c.SecondPeer,
		c.DisableFlags, c.LastBlockHeight)
}
