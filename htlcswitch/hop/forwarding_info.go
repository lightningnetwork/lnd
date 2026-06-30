package hop

import (
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ForwardingInfo contains all the information that is necessary to forward and
// incoming HTLC to the next hop encoded within a valid HopIterator instance.
// Forwarding links are to use this information to authenticate the information
// received within the incoming HTLC, to ensure that the prior hop didn't
// tamper with the end-to-end routing information at all.
type ForwardingInfo struct {
	// NextHop identifies the next hop the HTLC should be forwarded to. It
	// is either the short channel ID of the outgoing channel (the common
	// case, a Left) or, for a blinded route whose recipient identifies the
	// next hop by node ID (next_node_id), the next node's compressed public
	// key (a Right). A node-ID next hop is resolved to one of our channels
	// with that peer by the switch's non-strict forwarding logic. The zero
	// value is a Left equal to hop.Exit, which denotes the exit hop.
	NextHop fn.Either[lnwire.ShortChannelID, [33]byte]

	// AmountToForward is the amount of milli-satoshis that the receiving
	// node should forward to the next hop.
	AmountToForward lnwire.MilliSatoshi

	// OutgoingCLTV is the specified value of the CLTV timelock to be used
	// in the outgoing HTLC.
	OutgoingCLTV uint32

	// NextBlinding is an optional blinding point to be passed to the next
	// node in UpdateAddHtlc. This field is set if the htlc is part of a
	// blinded route.
	NextBlinding lnwire.BlindingPointRecord

	// PathID is a secret identifier that the creator of a blinded path
	// sets for itself to ensure that the blinded path has been used in the
	// correct context.
	PathID *chainhash.Hash
}

// NewChannelNextHop returns a next-hop value that identifies the outgoing
// channel by its short channel ID, which is the common case.
func NewChannelNextHop(
	scid lnwire.ShortChannelID) fn.Either[lnwire.ShortChannelID, [33]byte] {

	return fn.NewLeft[lnwire.ShortChannelID, [33]byte](scid)
}

// IsExit returns true if this forwarding info denotes the exit hop, i.e. we are
// the final recipient of the HTLC. This is the case when the next hop is a
// short channel ID equal to hop.Exit. A node-ID next hop (used by some blinded
// routes) is always a forward, never the exit hop.
func (f ForwardingInfo) IsExit() bool {
	var isExit bool
	f.NextHop.WhenLeft(func(scid lnwire.ShortChannelID) {
		isExit = scid == Exit
	})

	return isExit
}

// NextHopChannel returns the short channel ID of the outgoing channel when the
// next hop is identified by channel ID (the common case). It returns None when
// the next hop is identified by node ID instead, in which case the outgoing
// channel is selected by the switch's non-strict forwarding.
func (f ForwardingInfo) NextHopChannel() fn.Option[lnwire.ShortChannelID] {
	return f.NextHop.LeftToSome()
}

// FinalHtlcValidationResult describes the result of checking a final-hop
// HTLC against the onion payload and supported final-hop CLTV range.
type FinalHtlcValidationResult uint8

const (
	// FinalHtlcValid indicates that the HTLC matches the final-hop payload
	// and supported final-hop CLTV range.
	FinalHtlcValid FinalHtlcValidationResult = iota

	// FinalHtlcInvalidAmount indicates that the HTLC amount is below the
	// final amount requested by the onion payload.
	FinalHtlcInvalidAmount

	// FinalHtlcInvalidCltv indicates that the HTLC expiry is below the
	// final CLTV requested by the onion payload.
	FinalHtlcInvalidCltv

	// FinalHtlcExpiryTooFar indicates that the HTLC expiry is outside the
	// supported final-hop CLTV range.
	FinalHtlcExpiryTooFar
)

// ValidateFinalHtlc checks final-hop HTLC amount and CLTV details before
// invoice resolution.
func ValidateFinalHtlc(amt lnwire.MilliSatoshi, expiry, heightNow,
	maxFinalCltvDelta uint32, fwdInfo ForwardingInfo,
	validateAmount bool) FinalHtlcValidationResult {

	switch {
	// The HTLC amount is below the final amount requested by the
	// onion payload.
	case validateAmount && amt < fwdInfo.AmountToForward:
		return FinalHtlcInvalidAmount

	// The HTLC expiry is below the final CLTV requested by the onion
	// payload.
	case expiry < fwdInfo.OutgoingCLTV:
		return FinalHtlcInvalidCltv

	// The HTLC expiry is outside the supported final-hop CLTV range.
	case expiry > heightNow && expiry-heightNow > maxFinalCltvDelta:

		return FinalHtlcExpiryTooFar

	default:
		return FinalHtlcValid
	}
}
