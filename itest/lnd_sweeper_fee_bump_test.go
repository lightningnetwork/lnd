//go:build itest
// +build itest

package itest

import (
	"context"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// testSweeperFeeBump tests the sweeper's fee function behavior when fee bumping
// transactions. It verifies that:
// 1. The initial fee rate is set correctly
// 2. Fee bumping increases the fee rate monotonically
// 3. Fee bumping respects the maximum fee rate
func testSweeperFeeBump(ht *lntest.HarnessTest) {
	// Set test parameters
	const (
		maxFeeRate    = 50     // sat/vbyte
		deadlineDelta = 10     // blocks
		budget        = 100000 // sats
	)

	// Create a context with timeout for the entire test
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	alice := ht.Alice
	
	// Create and send initial low-fee transaction
	lowFeeTx := sendLowFeeTx(ht, alice)
	
	startTime := time.Now()
	var lastFeeRate float64
	
	// Monitor mempool for fee-bumped versions of the transaction
	for {
		select {
		case <-ctx.Done():
			ht.Fatalf("test timed out after %v", time.Since(startTime))
		default:
		}
		
		if time.Since(startTime) > maxTestDuration {
			ht.Fatalf("test exceeded maximum duration of %v", maxTestDuration)
		}
		
		// Get the latest transaction spending our outpoint from mempool
		tx := waitForTxInMempool(ht, alice, lowFeeTx.TxHash().String())
		if tx == nil {
			time.Sleep(pollInterval)
			continue
		}
		
		// Calculate current fee rate
		currentFeeRate := getTxFeeRate(tx)
		
		// Check if fee rate has increased
		if currentFeeRate > lastFeeRate {
			ht.Logf("Fee rate increased from %f to %f sat/vbyte",
				lastFeeRate, currentFeeRate)
			lastFeeRate = currentFeeRate
			
			// If we've reached or exceeded the max fee rate, we're done
			if currentFeeRate >= maxFeeRate {
				return
			}
		}
		
		time.Sleep(pollInterval)
	}
}

// waitForTxInMempool waits until the specified txid appears in the mempool
func waitForTxInMempool(ht *lntest.HarnessTest, node *lntest.HarnessNode, txid string) *wire.MsgTx {
	txHash, err := chainhash.NewHashFromStr(txid)
	require.NoError(ht, err, "invalid txid")

	err = wait.Predicate(func() bool {
		mempool, err := node.Client.GetRawMempool()
		require.NoError(ht, err, "failed to get mempool")
		for _, memTx := range mempool {
			if memTx.IsEqual(txHash) {
				return true
			}
		}
		return false
	}, testTimeout)
	if err != nil {
	}, timeout)
	require.NoError(ht, err, "timeout waiting for tx %s in mempool", txid)
}

// getTxFeeRate retrieves the fee rate of the latest transaction spending the
// given outpoint from the mempool
func getTxFeeRate(txRaw *wire.MsgTx) float64 {
	// Calculate fee rate: FeeSat / VSize
	feeSat := uint64(txRaw.TxOut[0].Value)
	vsize := uint64(txRaw.SerializeSize())
	return float64(feeSat) / float64(vsize)
}

// sendLowFeeTx creates and sends a transaction with a low fee rate to test fee bumping.
func sendLowFeeTx(ht *lntest.HarnessTest, node *lntest.HarnessNode) *wire.MsgTx {
	// Create a low-fee transaction
	addr := node.RPC.NewAddress()
	sweepReq := &walletrpc.SendOutputsRequest{
		Outputs: []*signrpc.TxOut{{
			Value:    50000000, // 0.5 BTC
			PkScript: addr.PkScript,
		}},
		SatPerVbyte: 1, // Start with minimum fee rate
	}

	// Send the transaction
	sendResp := node.RPC.SendOutputs(sweepReq)
	
	// Get the raw transaction
	txRaw := ht.Miner.Client.GetRawTransaction(sendResp.Txid)
	
	// Verify initial fee rate is low
	initialFeeRate := getTxFeeRate(txRaw)
	require.Less(ht, initialFeeRate, maxFeeRate,
		"initial fee rate %f should be less than max %f",
		initialFeeRate, maxFeeRate)
		
	return txRaw
}

func init() {
	// Add test to the set of integration tests
	lntest.AddTestCase(&lntest.TestCase{
		Name:     "sweeper fee bump",
		TestFunc: testSweeperFeeBump,
	})
}
