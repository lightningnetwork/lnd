package lnrpc

import (
	"errors"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrSatMsatMutualExclusive is returned when both a sat and an msat
	// amount are set.
	ErrSatMsatMutualExclusive = errors.New(
		"sat and msat arguments are mutually exclusive",
	)
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

	case *FeeLimit_FixedMsat:
		return lnwire.MilliSatoshi(feeLimit.GetFixedMsat())

	case *FeeLimit_Percent:
		return amount * lnwire.MilliSatoshi(feeLimit.GetPercent()) / 100

	default:
		// If a fee limit was not specified, we'll use the payment's
		// amount as an upper bound in order to avoid payment attempts
		// from incurring fees higher than the payment amount itself.
		return amount
	}
}

// UnmarshallAmt returns a strong msat type for a sat/msat pair of rpc fields.
func UnmarshallAmt(amtSat, amtMsat int64) (lnwire.MilliSatoshi, error) {
	if amtSat != 0 && amtMsat != 0 {
		return 0, ErrSatMsatMutualExclusive
	}

	if amtSat != 0 {
		return lnwire.NewMSatFromSatoshis(btcutil.Amount(amtSat)), nil
	}

	return lnwire.MilliSatoshi(amtMsat), nil
}
