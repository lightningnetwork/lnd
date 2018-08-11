package strayoutputpool

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwallet"
)

func TestCheckTransactionSanity(t *testing.T) {
	var wEstimate lnwallet.TxWeightEstimator

	wEstimate.AddP2WKHOutput()

	//wEstimate.AddWitnessInput()
	//wEstimate.AddNestedP2WSHInput()
	wEstimate.AddP2PKHInput()
	wEstimate.AddNestedP2WKHInput()
	wEstimate.AddP2WKHInput()


	estimator := &lnwallet.StaticFeeEstimator{FeeRate: 50}


	feePerVSize, _ := estimator.EstimateFeePerVSize(6)
	feePerKw := feePerVSize.FeePerKWeight()

	// Calculate transaction weight
	//blockchain.GetTransactionWeight(tx)


	t.Logf("VSize: %v, Weight: %v, \nfeePerKw: %v, \nFeeForVSize: %v, FeeForWeight: %v",
		wEstimate.VSize(),
		wEstimate.Weight(),
		feePerKw,
		feePerVSize.FeeForVSize(int64(wEstimate.VSize())),
		feePerKw.FeeForWeight(int64(wEstimate.Weight())),
	)
}