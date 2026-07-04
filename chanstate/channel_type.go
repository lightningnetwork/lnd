package chanstate

// ChannelType is an enum-like type that describes one of several possible
// channel types. Each open channel is associated with a particular type as the
// channel type may determine how higher level operations are conducted such as
// fee negotiation, channel closing, the format of HTLCs, etc. Structure-wise,
// a ChannelType is a bit field, with each bit denoting a modification from the
// base channel type of single funder.
type ChannelType uint64

const (
	// NOTE: iota isn't used here for this enum needs to be stable
	// long-term as it will be persisted to the database.

	// SingleFunderBit represents a channel wherein one party solely funds
	// the entire capacity of the channel.
	SingleFunderBit ChannelType = 0

	// DualFunderBit represents a channel wherein both parties contribute
	// funds towards the total capacity of the channel. The channel may be
	// funded symmetrically or asymmetrically.
	DualFunderBit ChannelType = 1 << 0

	// SingleFunderTweaklessBit is similar to the basic SingleFunder channel
	// type, but it omits the tweak for one's key in the commitment
	// transaction of the remote party.
	SingleFunderTweaklessBit ChannelType = 1 << 1

	// NoFundingTxBit denotes if we have the funding transaction locally on
	// disk. This bit may be on if the funding transaction was crafted by a
	// wallet external to the primary daemon.
	NoFundingTxBit ChannelType = 1 << 2

	// AnchorOutputsBit indicates that the channel makes use of anchor
	// outputs to bump the commitment transaction's effective feerate. This
	// channel type also uses a delayed to_remote output script.
	AnchorOutputsBit ChannelType = 1 << 3

	// FrozenBit indicates that the channel is a frozen channel, meaning
	// that only the responder can decide to cooperatively close the
	// channel.
	FrozenBit ChannelType = 1 << 4

	// ZeroHtlcTxFeeBit indicates that the channel should use zero-fee
	// second-level HTLC transactions.
	ZeroHtlcTxFeeBit ChannelType = 1 << 5

	// LeaseExpirationBit indicates that the channel has been leased for a
	// period of time, constraining every output that pays to the channel
	// initiator with an additional CLTV of the lease maturity.
	LeaseExpirationBit ChannelType = 1 << 6

	// ZeroConfBit indicates that the channel is a zero-conf channel.
	ZeroConfBit ChannelType = 1 << 7

	// ScidAliasChanBit indicates that the channel has negotiated the
	// scid-alias channel type.
	ScidAliasChanBit ChannelType = 1 << 8

	// ScidAliasFeatureBit indicates that the scid-alias feature bit was
	// negotiated during the lifetime of this channel.
	ScidAliasFeatureBit ChannelType = 1 << 9

	// SimpleTaprootFeatureBit indicates that the simple-taproot-chans
	// feature bit was negotiated during the lifetime of the channel.
	SimpleTaprootFeatureBit ChannelType = 1 << 10

	// TapscriptRootBit indicates that this is a MuSig2 channel with a top
	// level tapscript commitment. This MUST be set along with the
	// SimpleTaprootFeatureBit.
	TapscriptRootBit ChannelType = 1 << 11

	// TaprootFinalBit indicates that this is a MuSig2 channel using the
	// final/production taproot scripts and feature bits 80/81. This MUST
	// be set along with the SimpleTaprootFeatureBit.
	TaprootFinalBit ChannelType = 1 << 12
)

// IsSingleFunder returns true if the channel type if one of the known single
// funder variants.
func (c ChannelType) IsSingleFunder() bool {
	return c&DualFunderBit == 0
}

// IsDualFunder returns true if the ChannelType has the DualFunderBit set.
func (c ChannelType) IsDualFunder() bool {
	return c&DualFunderBit == DualFunderBit
}

// IsTweakless returns true if the target channel uses a commitment that
// doesn't tweak the key for the remote party.
func (c ChannelType) IsTweakless() bool {
	return c&SingleFunderTweaklessBit == SingleFunderTweaklessBit
}

// HasFundingTx returns true if this channel type is one that has a funding
// transaction stored locally.
func (c ChannelType) HasFundingTx() bool {
	return c&NoFundingTxBit == 0
}

// HasAnchors returns true if this channel type has anchor outputs on its
// commitment.
func (c ChannelType) HasAnchors() bool {
	return c&AnchorOutputsBit == AnchorOutputsBit
}

// ZeroHtlcTxFee returns true if this channel type uses second-level HTLC
// transactions signed with zero-fee.
func (c ChannelType) ZeroHtlcTxFee() bool {
	return c&ZeroHtlcTxFeeBit == ZeroHtlcTxFeeBit
}

// IsFrozen returns true if the channel is considered to be "frozen". A frozen
// channel means that only the responder can initiate a cooperative channel
// closure.
func (c ChannelType) IsFrozen() bool {
	return c&FrozenBit == FrozenBit
}

// HasLeaseExpiration returns true if the channel originated from a lease.
func (c ChannelType) HasLeaseExpiration() bool {
	return c&LeaseExpirationBit == LeaseExpirationBit
}

// HasZeroConf returns true if the channel is a zero-conf channel.
func (c ChannelType) HasZeroConf() bool {
	return c&ZeroConfBit == ZeroConfBit
}

// HasScidAliasChan returns true if the scid-alias channel type was negotiated.
func (c ChannelType) HasScidAliasChan() bool {
	return c&ScidAliasChanBit == ScidAliasChanBit
}

// HasScidAliasFeature returns true if the scid-alias feature bit was
// negotiated during the lifetime of this channel.
func (c ChannelType) HasScidAliasFeature() bool {
	return c&ScidAliasFeatureBit == ScidAliasFeatureBit
}

// IsTaproot returns true if the channel is using taproot features.
func (c ChannelType) IsTaproot() bool {
	return c&SimpleTaprootFeatureBit == SimpleTaprootFeatureBit
}

// HasTapscriptRoot returns true if the channel is using a top level tapscript
// root commitment.
func (c ChannelType) HasTapscriptRoot() bool {
	return c&TapscriptRootBit == TapscriptRootBit
}

// IsTaprootFinal returns true if the channel is using final/production taproot
// scripts and feature bits.
func (c ChannelType) IsTaprootFinal() bool {
	return c&TaprootFinalBit == TaprootFinalBit
}
