package models

import (
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ChannelEdgePolicy represents a *directed* edge within the channel graph. For
// each channel in the database, there are two distinct edges: one for each
// possible direction of travel along the channel. The edges themselves hold
// information concerning fees, and minimum time-lock information which is
// utilized during path finding.
type ChannelEdgePolicy struct {
	// SigBytes is the raw bytes of the signature of the channel edge
	// policy. We'll only parse these if the caller needs to access the
	// signature for validation purposes. Do not set SigBytes directly, but
	// use SetSigBytes instead to make sure that the cache is invalidated.
	SigBytes []byte

	// sig is a cached fully parsed signature.
	sig *ecdsa.Signature

	// ChannelID is the unique channel ID for the channel. The first 3
	// bytes are the block height, the next 3 the index within the block,
	// and the last 2 bytes are the output index for the channel.
	ChannelID uint64

	// LastUpdate is the last time an authenticated edge for this channel
	// was received.
	LastUpdate time.Time

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
}

// Signature is a channel announcement signature, which is needed for proper
// edge policy announcement.
//
// NOTE: By having this method to access an attribute, we ensure we only need
// to fully deserialize the signature if absolutely necessary.
func (c *ChannelEdgePolicy) Signature() (*ecdsa.Signature, error) {
	if c.sig != nil {
		return c.sig, nil
	}

	sig, err := ecdsa.ParseSignature(c.SigBytes)
	if err != nil {
		return nil, err
	}

	c.sig = sig

	return sig, nil
}

// SetSigBytes updates the signature and invalidates the cached parsed
// signature.
func (c *ChannelEdgePolicy) SetSigBytes(sig []byte) {
	c.SigBytes = sig
	c.sig = nil
}

// IsDisabled determines whether the edge has the disabled bit set.
func (c *ChannelEdgePolicy) IsDisabled() bool {
	return c.ChannelFlags.IsDisabled()
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
	return fmt.Sprintf("ChannelID=%v, MessageFlags=%v, ChannelFlags=%v, "+
		"LastUpdate=%v", c.ChannelID, c.MessageFlags, c.ChannelFlags,
		c.LastUpdate)
}
