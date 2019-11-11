package lnrpc

import (
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
)

// CalculateFeeLimit returns the fee limit in millisatoshis. If a percentage
// based fee limit has been requested, we'll factor in the ratio provided with
// the amount of the payment.
func CalculateFeeLimit(feeLimit *FeeLimit,
	amount lnwire.MilliSatoshi) lnwire.MilliSatoshi {

	switch feeLimit.GetLimit().(type) {

	case *FeeLimit_Fixed:
		return lnwire.NewMSatFromSatoshis(
			btcutil.Amount(feeLimit.GetFixed()),
		)

	case *FeeLimit_Percent:
		return amount * lnwire.MilliSatoshi(feeLimit.GetPercent()) / 100

	default:
		// If a fee limit was not specified, we'll use the payment's
		// amount as an upper bound in order to avoid payment attempts
		// from incurring fees higher than the payment amount itself.
		return amount
	}
}
