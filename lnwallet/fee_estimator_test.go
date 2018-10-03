package lnwallet_test

import (
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// TestFeeRateTypes checks that converting fee rates between the
// different types that represent fee rates and calculating fees
// work as expected.
func TestFeeRateTypes(t *testing.T) {
	t.Parallel()

	// We'll be calculating the transaction fees for the given measurements
	// using different fee rates and expecting them to match.
	const vsize = 300
	const weight = vsize * 4

	// Test the conversion from sat/kw to sat/kb.
	for feePerKw := lnwallet.SatPerKWeight(250); feePerKw < 10000; feePerKw += 50 {
		feePerKB := feePerKw.FeePerKVByte()
		if feePerKB != lnwallet.SatPerKVByte(feePerKw*4) {
			t.Fatalf("expected %d sat/kb, got %d sat/kb when "+
				"converting from %d sat/kw", feePerKw*4,
				feePerKB, feePerKw)
		}

		// The resulting transaction fee should be the same when using
		// both rates.
		expectedFee := btcutil.Amount(feePerKw * weight / 1000)
		fee1 := feePerKw.FeeForWeight(weight)
		if fee1 != expectedFee {
			t.Fatalf("expected fee of %d sats, got %d sats",
				expectedFee, fee1)
		}
		fee2 := feePerKB.FeeForVSize(vsize)
		if fee2 != expectedFee {
			t.Fatalf("expected fee of %d sats, got %d sats",
				expectedFee, fee2)
		}
	}

	// Test the conversion from sat/kb to sat/kw.
	for feePerKB := lnwallet.SatPerKVByte(1000); feePerKB < 40000; feePerKB += 1000 {
		feePerKw := feePerKB.FeePerKWeight()
		if feePerKw != lnwallet.SatPerKWeight(feePerKB/4) {
			t.Fatalf("expected %d sat/kw, got %d sat/kw when "+
				"converting from %d sat/kb", feePerKB/4,
				feePerKw, feePerKB)
		}

		// The resulting transaction fee should be the same when using
		// both rates.
		expectedFee := btcutil.Amount(feePerKB * vsize / 1000)
		fee1 := feePerKB.FeeForVSize(vsize)
		if fee1 != expectedFee {
			t.Fatalf("expected fee of %d sats, got %d sats",
				expectedFee, fee1)
		}
		fee2 := feePerKw.FeeForWeight(weight)
		if fee2 != expectedFee {
			t.Fatalf("expected fee of %d sats, got %d sats",
				expectedFee, fee2)
		}
	}
}

// TestStaticFeeEstimator checks that the StaticFeeEstimator
// returns the expected fee rate.
func TestStaticFeeEstimator(t *testing.T) {
	t.Parallel()

	const feePerKw = lnwallet.FeePerKwFloor

	feeEstimator := &lnwallet.StaticFeeEstimator{
		FeePerKW: feePerKw,
	}
	if err := feeEstimator.Start(); err != nil {
		t.Fatalf("unable to start fee estimator: %v", err)
	}
	defer feeEstimator.Stop()

	feeRate, err := feeEstimator.EstimateFeePerKW(6)
	if err != nil {
		t.Fatalf("unable to get fee rate: %v", err)
	}

	if feeRate != feePerKw {
		t.Fatalf("expected fee rate %v, got %v", feePerKw, feeRate)
	}
}
