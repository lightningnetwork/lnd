package lnwire

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
)

func TestMilliSatoshiConversion(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		mSatAmount MilliSatoshi

		satAmount btcutil.Amount
		btcAmount float64
	}{
		{
			mSatAmount: 0,
			satAmount:  0,
			btcAmount:  0,
		},
		{
			mSatAmount: 10,
			satAmount:  0,
			btcAmount:  0,
		},
		{
			mSatAmount: 999,
			satAmount:  0,
			btcAmount:  0,
		},
		{
			mSatAmount: 1000,
			satAmount:  1,
			btcAmount:  1e-8,
		},
		{
			mSatAmount: 10000,
			satAmount:  10,
			btcAmount:  0.00000010,
		},
		{
			mSatAmount: 100000000000,
			satAmount:  100000000,
			btcAmount:  1,
		},
		{
			mSatAmount: 2500000000000,
			satAmount:  2500000000,
			btcAmount:  25,
		},
		{
			mSatAmount: 5000000000000,
			satAmount:  5000000000,
			btcAmount:  50,
		},
		{
			mSatAmount: 21 * 1e6 * 1e8 * 1e3,
			satAmount:  21 * 1e6 * 1e8,
			btcAmount:  21 * 1e6,
		},
	}

	for i, test := range testCases {
		if test.mSatAmount.ToSatoshis() != test.satAmount {
			t.Fatalf("test #%v: wrong sat amount, expected %v "+
				"got %v", i, int64(test.satAmount),
				int64(test.mSatAmount.ToSatoshis()))
		}
		if test.mSatAmount.ToBTC() != test.btcAmount {
			t.Fatalf("test #%v: wrong btc amount, expected %v "+
				"got %v", i, test.btcAmount,
				test.mSatAmount.ToBTC())
		}
	}
}
