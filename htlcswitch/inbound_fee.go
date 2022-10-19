package htlcswitch

import "github.com/lightningnetwork/lnd/lnwire"

type InboundFee struct {
	Base int32
	Rate int32
}

// NewInboundFeeFromWire constructs an inbound fee structure from a wire fee.
func NewInboundFeeFromWire(fee lnwire.Fee) InboundFee {
	return InboundFee{
		Base: fee.BaseFee,
		Rate: fee.FeeRate,
	}
}

// ToWire converts the inbound fee to a wire fee structure.
func (i *InboundFee) ToWire() lnwire.Fee {
	return lnwire.Fee{
		BaseFee: i.Base,
		FeeRate: i.Rate,
	}
}

// CalcFee calculates what the inbound fee should minimally be for forwarding
// the given amount. This amount is the _outgoing_ amount, which is what the
// inbound fee is based on.
func (i *InboundFee) CalcFee(amt lnwire.MilliSatoshi) int64 {
	fee := int64(i.Base)

	// Calculate proportional component. Always round down in favor of the
	// payer of the fee. That way rounding differences can not cause a
	// payment to fail.
	switch {
	case i.Rate > 0:
		fee += int64(i.Rate) * int64(amt) / 1e6

	case i.Rate < 0:
		fee += (int64(i.Rate)*int64(amt) - (1e6 - 1)) / 1e6
	}

	return fee
}
