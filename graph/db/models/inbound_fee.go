package models

import (
	"github.com/lightningnetwork/lnd/internal/fee"
	"github.com/lightningnetwork/lnd/lnwire"
)

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
// the given amount. This amount is the total of the outgoing amount plus the
// outbound fee, which is what the inbound fee is based on. It panics if the
// fee cannot be represented as an int64.
func (i *InboundFee) CalcFee(amt lnwire.MilliSatoshi) int64 {
	value, err := fee.Inbound(uint64(amt), i.Base, i.Rate).Unpack()
	if err != nil {
		panic(err)
	}

	return value
}
