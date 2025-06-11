package models

import (
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

const (
	// feeRateParts is the total number of parts used to express fee rates.
	feeRateParts = 1e6
)

// CachedEdgePolicy is a struct that only caches the information of a
// ChannelEdgePolicy that we actually use for pathfinding and therefore need to
// store in the cache.
type CachedEdgePolicy struct {
	// ChannelID is the unique channel ID for the channel. The first 3
	// bytes are the block height, the next 3 the index within the block,
	// and the last 2 bytes are the output index for the channel.
	ChannelID uint64

	// MessageFlags is a bitfield which indicates the presence of optional
	// fields (like max_htlc) in the policy.
	MessageFlags lnwire.ChanUpdateMsgFlags

	// ChannelFlags is a bitfield which signals the capabilities of the
	// channel as well as the directed edge this update applies to.
	ChannelFlags lnwire.ChanUpdateChanFlags

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

	// InboundFee is the fee that the node will charge for incoming HTLCs.
	InboundFee fn.Option[lnwire.Fee]

	// ToNodePubKey is a function that returns the to node of a policy.
	// Since we only ever store the inbound policy, this is always the node
	// that we query the channels for in ForEachChannel(). Therefore, we can
	// save a lot of space by not storing this information in the memory and
	// instead just set this function when we copy the policy from cache in
	// ForEachChannel().
	ToNodePubKey func() route.Vertex

	// ToNodeFeatures are the to node's features. They are never set while
	// the edge is in the cache, only on the copy that is returned in
	// ForEachChannel().
	ToNodeFeatures *lnwire.FeatureVector
}

// ComputeFee computes the fee to forward an HTLC of `amt` milli-satoshis over
// the passed active payment channel. This value is currently computed as
// specified in BOLT07, but will likely change in the near future.
func (c *CachedEdgePolicy) ComputeFee(
	amt lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	return c.FeeBaseMSat + (amt*c.FeeProportionalMillionths)/feeRateParts
}

// IsDisabled returns true if the channel is disabled in the direction from  the
// advertising node.
func (c *CachedEdgePolicy) IsDisabled() bool {
	return c.ChannelFlags&lnwire.ChanUpdateDisabled != 0
}

// IsNode1 returns true if this policy was announced by the channel's node_1
// node.
func (c *CachedEdgePolicy) IsNode1() bool {
	return c.ChannelFlags&lnwire.ChanUpdateDirection == 0
}

// NewCachedPolicy turns a full policy into a minimal one that can be cached.
func NewCachedPolicy(policy *ChannelEdgePolicy) *CachedEdgePolicy {
	return &CachedEdgePolicy{
		ChannelID:                 policy.ChannelID,
		MessageFlags:              policy.MessageFlags,
		ChannelFlags:              policy.ChannelFlags,
		TimeLockDelta:             policy.TimeLockDelta,
		MinHTLC:                   policy.MinHTLC,
		MaxHTLC:                   policy.MaxHTLC,
		FeeBaseMSat:               policy.FeeBaseMSat,
		FeeProportionalMillionths: policy.FeeProportionalMillionths,
		InboundFee:                policy.InboundFee,
	}
}
