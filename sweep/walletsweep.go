package sweep

import (
	"fmt"

	"github.com/lightningnetwork/lnd/lnwallet"
)

// FeePreference allows callers to express their time value for inclusion of a
// transaction into a block via either a confirmation target, or a fee rate.
type FeePreference struct {
	// ConfTarget if non-zero, signals a fee preference expressed in the
	// number of desired blocks between first broadcast, and confirmation.
	ConfTarget uint32

	// FeeRate if non-zero, signals a fee pre fence expressed in the fee
	// rate expressed in sat/kw for a particular transaction.
	FeeRate lnwallet.SatPerKWeight
}

// DetermineFeePerKw will determine the fee in sat/kw that should be paid given
// an estimator, a confirmation target, and a manual value for sat/byte. A
// value is chosen based on the two free parameters as one, or both of them can
// be zero.
func DetermineFeePerKw(feeEstimator lnwallet.FeeEstimator,
	feePref FeePreference) (lnwallet.SatPerKWeight, error) {

	switch {
	// If the target number of confirmations is set, then we'll use that to
	// consult our fee estimator for an adequate fee.
	case feePref.ConfTarget != 0:
		feePerKw, err := feeEstimator.EstimateFeePerKW(
			uint32(feePref.ConfTarget),
		)
		if err != nil {
			return 0, fmt.Errorf("unable to query fee "+
				"estimator: %v", err)
		}

		return feePerKw, nil

	// If a manual sat/byte fee rate is set, then we'll use that directly.
	// We'll need to convert it to sat/kw as this is what we use
	// internally.
	case feePref.FeeRate != 0:
		feePerKW := feePref.FeeRate
		if feePerKW < lnwallet.FeePerKwFloor {
			log.Infof("Manual fee rate input of %d sat/kw is "+
				"too low, using %d sat/kw instead", feePerKW,
				lnwallet.FeePerKwFloor)

			feePerKW = lnwallet.FeePerKwFloor
		}

		return feePerKW, nil

	// Otherwise, we'll attempt a relaxed confirmation target for the
	// transaction
	default:
		feePerKw, err := feeEstimator.EstimateFeePerKW(6)
		if err != nil {
			return 0, fmt.Errorf("unable to query fee estimator: "+
				"%v", err)
		}

		return feePerKw, nil
	}
}
