package models

import (
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ChannelEdgePolicy1 represents a *directed* edge within the channel graph. For
// each channel in the database, there are two distinct edges: one for each
// possible direction of travel along the channel. The edges themselves hold
// information concerning fees, and minimum time-lock information which is
// utilized during path finding.
type ChannelEdgePolicy1 struct {
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
func (c *ChannelEdgePolicy1) Signature() (*ecdsa.Signature, error) {
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
func (c *ChannelEdgePolicy1) SetSigBytes(sig []byte) {
	c.SigBytes = sig
	c.sig = nil
}

// IsDisabled determines whether the edge has the disabled bit set.
//
// NOTE: This is part of the ChannelEdgePolicy interface.
func (c *ChannelEdgePolicy1) IsDisabled() bool {
	return c.ChannelFlags.IsDisabled()
}

// ComputeFee computes the fee to forward an HTLC of `amt` milli-satoshis over
// the passed active payment channel. This value is currently computed as
// specified in BOLT07, but will likely change in the near future.
func (c *ChannelEdgePolicy1) ComputeFee(
	amt lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	return c.FeeBaseMSat + (amt*c.FeeProportionalMillionths)/feeRateParts
}

// SCID returns the short channel ID of the channel being referred to.
//
// NOTE: This is part of the ChannelEdgePolicy interface.
func (c *ChannelEdgePolicy1) SCID() lnwire.ShortChannelID {
	return lnwire.NewShortChanIDFromInt(c.ChannelID)
}

// IsNode1 returns true if the update was constructed by node 1 of the
// channel.
//
// NOTE: This is part of the ChannelEdgePolicy interface.
func (c *ChannelEdgePolicy1) IsNode1() bool {
	return c.ChannelFlags&lnwire.ChanUpdateDirection == 0
}

// GetToNode returns the pub key of the node that did not produce the update.
//
// NOTE: This is part of the ChannelEdgePolicy interface.
func (c *ChannelEdgePolicy1) GetToNode() [33]byte {
	return c.ToNode
}

// ForwardingPolicy return the various forwarding policy rules set by the
// update.
//
// NOTE: This is part of the ChannelEdgePolicy interface.
func (c *ChannelEdgePolicy1) ForwardingPolicy() *lnwire.ForwardingPolicy {
	return &lnwire.ForwardingPolicy{
		TimeLockDelta: c.TimeLockDelta,
		BaseFee:       c.FeeBaseMSat,
		FeeRate:       c.FeeProportionalMillionths,
		MinHTLC:       c.MinHTLC,
		HasMaxHTLC:    c.MessageFlags.HasMaxHtlc(),
		MaxHTLC:       c.MaxHTLC,
	}
}

// Before compares this update against the passed update and returns true if
// this update has a lower timestamp than the passed one.
//
// NOTE: This is part of the ChannelEdgePolicy interface.
func (c *ChannelEdgePolicy1) Before(policy ChannelEdgePolicy) (bool, error) {
	other, ok := policy.(*ChannelEdgePolicy1)
	if !ok {
		return false, fmt.Errorf("can't compare type %T to type "+
			"ChannelEdgePolicy1", policy)
	}

	return c.LastUpdate.Before(other.LastUpdate), nil
}

// AfterUpdateMsg compares this update against the passed
// lnwire.ChannelUpdate message and returns true if this update is newer than
// the passed one.
//
// NOTE: This is part of the ChannelEdgePolicy interface.
func (c *ChannelEdgePolicy1) AfterUpdateMsg(msg lnwire.ChannelUpdate) (bool,
	error) {

	upd, ok := msg.(*lnwire.ChannelUpdate1)
	if !ok {
		return false, fmt.Errorf("expected *lnwire.ChannelUpdate1 to "+
			"be coupled with ChannelEdgePolicy1, got: %T", msg)
	}

	timestamp := time.Unix(int64(upd.Timestamp), 0)

	return c.LastUpdate.After(timestamp), nil
}

func (c *ChannelEdgePolicy1) ExtraData() lnwire.ExtraOpaqueData {
	return c.ExtraOpaqueData
}

// Sig returns the signature of the update message.
//
// NOTE: This is part of the ChannelEdgePolicy interface.
func (c *ChannelEdgePolicy1) Sig() (input.Signature, error) {
	return c.Signature()
}

// A compile-time check to ensure that ChannelEdgePolicy1 implements the
// ChannelEdgePolicy interface.
var _ ChannelEdgePolicy = (*ChannelEdgePolicy1)(nil)

type ChannelEdgePolicy2 struct {
	lnwire.ChannelUpdate2

	ToNode [33]byte
}

// Sig returns the signature of the update message.
//
// NOTE: This is part of the ChannelEdgePolicy interface.
func (c *ChannelEdgePolicy2) Sig() (input.Signature, error) {
	return c.Signature.ToSignature()
}

// AfterUpdateMsg compares this update against the passed lnwire.ChannelUpdate
// message and returns true if this update is newer than the passed one.
//
// NOTE: This is part of the ChannelEdgePolicy interface.
func (c *ChannelEdgePolicy2) AfterUpdateMsg(msg lnwire.ChannelUpdate) (bool,
	error) {

	upd, ok := msg.(*lnwire.ChannelUpdate2)
	if !ok {
		return false, fmt.Errorf("expected *lnwire.ChannelUpdate2 to "+
			"be coupled with ChannelEdgePolicy2, got: %T", msg)
	}

	return c.BlockHeight.Val > upd.BlockHeight.Val, nil
}

// Before compares this update against the passed update and returns true if
// this update has a lower timestamp than the passed one.
//
// NOTE: This is part of the ChannelEdgePolicy interface.
func (c *ChannelEdgePolicy2) Before(policy ChannelEdgePolicy) (bool, error) {
	other, ok := policy.(*ChannelEdgePolicy2)
	if !ok {
		return false, fmt.Errorf("can't compare type %T to type "+
			"ChannelEdgePolicy2", policy)
	}

	return c.BlockHeight.Val < other.BlockHeight.Val, nil
}

// GetToNode returns the pub key of the node that did not produce the update.
//
// NOTE: This is part of the ChannelEdgePolicy interface.
func (c *ChannelEdgePolicy2) GetToNode() [33]byte {
	return c.ToNode
}

// A compile-time check to ensure that ChannelEdgePolicy2 implements the
// ChannelEdgePolicy interface.
var _ ChannelEdgePolicy = (*ChannelEdgePolicy2)(nil)

// EdgePolicyFromUpdate converts the given lnwire.ChannelUpdate into the
// corresponding ChannelEdgePolicy type.
func EdgePolicyFromUpdate(update lnwire.ChannelUpdate) (
	ChannelEdgePolicy, error) {

	switch upd := update.(type) {
	case *lnwire.ChannelUpdate1:
		//nolint:lll
		return &ChannelEdgePolicy1{
			SigBytes:                  upd.Signature.ToSignatureBytes(),
			ChannelID:                 upd.ShortChannelID.ToUint64(),
			LastUpdate:                time.Unix(int64(upd.Timestamp), 0),
			MessageFlags:              upd.MessageFlags,
			ChannelFlags:              upd.ChannelFlags,
			TimeLockDelta:             upd.TimeLockDelta,
			MinHTLC:                   upd.HtlcMinimumMsat,
			MaxHTLC:                   upd.HtlcMaximumMsat,
			FeeBaseMSat:               lnwire.MilliSatoshi(upd.BaseFee),
			FeeProportionalMillionths: lnwire.MilliSatoshi(upd.FeeRate),
			ExtraOpaqueData:           upd.ExtraOpaqueData,
		}, nil

	case *lnwire.ChannelUpdate2:
		return &ChannelEdgePolicy2{
			ChannelUpdate2: *upd,
		}, nil

	default:
		return nil, fmt.Errorf("unhandled implementation of "+
			"lnwire.ChannelUpdate: %T", update)
	}
}
