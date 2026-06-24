package hop

import (
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ForwardingInfo contains all the information that is necessary to forward and
// incoming HTLC to the next hop encoded within a valid HopIterator instance.
// Forwarding links are to use this information to authenticate the information
// received within the incoming HTLC, to ensure that the prior hop didn't
// tamper with the end-to-end routing information at all.
type ForwardingInfo struct {
	// NextHop is the channel ID of the next hop. The received HTLC should
	// be forwarded to this particular channel in order to continue the
	// end-to-end route.
	NextHop lnwire.ShortChannelID

	// AmountToForward is the amount of milli-satoshis that the receiving
	// node should forward to the next hop.
	AmountToForward lnwire.MilliSatoshi

	// OutgoingCTLV is the specified value of the CTLV timelock to be used
	// in the outgoing HTLC.
	OutgoingCTLV uint32

	// NextBlinding is an optional blinding point to be passed to the next
	// node in UpdateAddHtlc. This field is set if the htlc is part of a
	// blinded route.
	NextBlinding lnwire.BlindingPointRecord

	// PathID is a secret identifier that the creator of a blinded path
	// sets for itself to ensure that the blinded path has been used in the
	// correct context.
	PathID *chainhash.Hash
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
	case expiry < fwdInfo.OutgoingCTLV:
		return FinalHtlcInvalidCltv

	// The HTLC expiry is outside the supported final-hop CLTV range.
	case expiry > heightNow && expiry-heightNow > maxFinalCltvDelta:

		return FinalHtlcExpiryTooFar

	default:
		return FinalHtlcValid
	}
}
