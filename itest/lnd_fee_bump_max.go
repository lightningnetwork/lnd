package itest

import (
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// testBumpFeeUntilMaxReached tests fee-bumping a wallet transaction until the
// maximum fee rate of 100 sat/vbyte is reached.
func testBumpFeeUntilMaxReached(ht *lntest.HarnessTest) {
	const (
		maxFeeRate     = 100 // sat/vbyte
		defaultTimeout = 30 * time.Second
	)

	// Set up Alice with a max fee rate of 100 sat/vbyte.
	args := []string{fmt.Sprintf("--sweeper.maxfeerate=%d", maxFeeRate)}
	alice := ht.NewNode("Alice", args)

	// Fund Alice's wallet with 1 BTC.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, alice)

	// Create a new address for a transaction.
	addrResp := alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	})

	// Send 0.001 BTC with a low fee rate (1 sat/vbyte) to keep it unconfirmed.
	sendReq := &lnrpc.SendCoinsRequest{
		Addr:       addrResp.Address,
		Amount:     100_000, // 0.001 BTC
		SatPerByte: 1,       // Low fee rate
	}
	txid := alice.RPC.SendCoins(sendReq).Txid

	// Wait for the transaction to appear in the mempool.
	waitForTxInMempool(ht, txid, defaultTimeout)

	// Get the raw transaction to find an input outpoint for fee-bumping.
	txHash, err := chainhash.NewHashFromStr(txid)
	require.NoError(ht, err, "invalid txid")
	txRawVerbose, err := ht.Miner().Client.GetRawTransactionVerbose(txHash)
	require.NoError(ht, err, "failed to get raw tx")

	// Select the first input outpoint (assumes at least one input).
	require.Greater(ht, len(txRawVerbose.Vin), 0, "no inputs in transaction")
	input := txRawVerbose.Vin[0]
	op := &lnrpc.OutPoint{
		TxidBytes:   txHash[:],
		OutputIndex: uint32(input.Vout),
	}

	// Calculate initial fee rate from the mempool transaction.
	initialFeeRate := getTxFeeRate(ht, txHash)
	ht.Logf("Initial fee rate: %d sat/vbyte", initialFeeRate)

	// Bump fee repeatedly until max fee rate is reached or budget is exhausted.
	bumpReq := &walletrpc.BumpFeeRequest{
		Outpoint:      op,
		Immediate:     true,
		DeadlineDelta: 5,
		Budget:        50_000, // Half of 0.001 BTC in satoshis
	}
	var currentFeeRate uint64
	for i := 0; i < 20; i++ {
		_, err := alice.RPC.WalletKit.BumpFee(ht.Context(), bumpReq)
		if err != nil {
			if strings.Contains(err.Error(), "max fee rate exceeded") ||
				strings.Contains(err.Error(), "position already at max") {
				ht.Logf("Stopped bumping at max fee rate")
				break
			}
			require.NoError(ht, err, "failed to bump fee")
		}

		// Wait for the new transaction in the mempool.
		waitForTxInMempool(ht, txid, defaultTimeout)

		// Get the latest transaction spending the input outpoint.
		currentFeeRate = getTxFeeRate(ht, txHash)
		ht.Logf("Attempt #%d: fee rate = %d sat/vbyte", i+1, currentFeeRate)

		// Stop if fee rate reaches or exceeds max or doesn't increase.
		if currentFeeRate >= maxFeeRate || currentFeeRate <= initialFeeRate {
			break
		}
		initialFeeRate = currentFeeRate
	}

	// Verify the final fee rate is at least the max fee rate.
	require.GreaterOrEqual(ht, currentFeeRate, uint64(maxFeeRate),
		"final fee rate %d sat/vbyte below max %d sat/vbyte",
		currentFeeRate, maxFeeRate)

	// Mine a block to confirm the transaction.
	ht.MineBlocks(1)
}

// waitForTxInMempool waits until the specified txid appears in the mempool.
func waitForTxInMempool(ht *lntest.HarnessTest, txid string, timeout time.Duration) {
	txHash, err := chainhash.NewHashFromStr(txid)
	require.NoError(ht, err, "invalid txid")

	err = wait.Predicate(func() bool {
		mempool, err := ht.Miner.Client.GetRawMempool()
		require.NoError(ht, err, "failed to get mempool")
		for _, memTx := range mempool {
			if memTx.IsEqual(txHash) {
				return true
			}
		}
		return false
	}, timeout)
	require.NoError(ht, err, "timeout waiting for tx %s in mempool", txid)
}

// getTxFeeRate retrieves the fee rate of the latest transaction spending the
// given outpoint from the mempool.
func getTxFeeRate(ht *lntest.HarnessTest, txHash *chainhash.Hash) uint64 {
	// Get all mempool transactions.
	mempool, err := ht.Miner.Client.GetRawMempoolVerbose()
	require.NoError(ht, err, "failed to get mempool")

	// Find the transaction spending the given outpoint.
	for txid, entry := range mempool {
		txRaw, err := ht.Miner.Client.GetRawTransactionVerbose(txid)
		require.NoError(ht, err, "failed to get raw tx %s", txid)

		for _, vin := range txRaw.Vin {
			if vin.Txid == txHash.String() {
				// Calculate fee rate: FeeSat / VSize.
				feeSat := uint64(entry.Fee * btcutil.SatoshiPerBitcoin)
				vsize := uint64(entry.Vsize)
				if vsize == 0 {
					ht.Fatalf("zero vsize for tx %s", txid)
				}
				return feeSat / vsize
			}
		}
	}

	ht.Fatalf("no transaction found spending outpoint %s", txHash)
	return 0
}
