package lnwallet_test

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/roasbeef/btcutil"
)

// TestFeeRateTypes checks that converting fee rates between the
// different types that represent fee rates and calculating fees
// work as expected.
func TestFeeRateTypes(t *testing.T) {
	t.Parallel()

	// Let our fee rate be 100 sat/vbyte.
	feePerVSize := lnwallet.SatPerVByte(100)

	// It is also equivalent to 25000 sat/kw.
	feePerKw := feePerVSize.FeePerKWeight()
	if feePerKw != 25000 {
		t.Fatalf("expected %d sat/kw, got %d sat/kw", 25000,
			feePerKw)
	}

	const txVSize = 300

	// We'll now run through a set of values for the fee per vsize type,
	// making sure the conversion to sat/kw and fee calculation is done
	// correctly.
	for f := lnwallet.SatPerVByte(0); f <= 40; f++ {
		fPerKw := f.FeePerKWeight()

		// The kw is always 250*vsize.
		if fPerKw != lnwallet.SatPerKWeight(f*250) {
			t.Fatalf("expected %d sat/kw, got %d sat/kw, when "+
				"converting %d sat/vbyte", f*250, fPerKw, f)
		}

		// The tx fee should simply be f*txvsize.
		fee := f.FeeForVSize(txVSize)
		if fee != btcutil.Amount(f*txVSize) {
			t.Fatalf("expected tx fee to be %d sat, was %d sat",
				f*txVSize, fee)
		}

		// The weight is 4*vsize. Fee calculation from the fee/kw
		// should result in the same fee.
		fee2 := fPerKw.FeeForWeight(txVSize * 4)
		if fee != fee2 {
			t.Fatalf("fee calculated from vsize (%d) not equal "+
				"fee calculated from weight (%d)", fee, fee2)
		}
	}

	// Do the same for fee per kw.
	for f := lnwallet.SatPerKWeight(0); f < 1500; f++ {
		weight := int64(txVSize * 4)

		// The expected fee is weight*f / 1000, since the fee is
		// denominated per 1000 wu.
		expFee := btcutil.Amount(weight) * btcutil.Amount(f) / 1000
		fee := f.FeeForWeight(weight)
		if fee != expFee {
			t.Fatalf("expected fee to be %d sat, was %d",
				fee, expFee)
		}
	}
}

// TestStaticFeeEstimator checks that the StaticFeeEstimator
// returns the expected fee rate.
func TestStaticFeeEstimator(t *testing.T) {
	t.Parallel()

	const feePerVSize = 100

	feeEstimator := &lnwallet.StaticFeeEstimator{
		FeeRate: feePerVSize,
	}
	if err := feeEstimator.Start(); err != nil {
		t.Fatalf("unable to start fee estimator: %v", err)
	}
	defer feeEstimator.Stop()

	feeRate, err := feeEstimator.EstimateFeePerVSize(6)
	if err != nil {
		t.Fatalf("unable to get fee rate: %v", err)
	}

	if feeRate != feePerVSize {
		t.Fatalf("expected fee rate %v, got %v", feePerVSize, feeRate)
	}
}
