package itest

import (
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// waitForTxInMempool waits until the specified txid appears in the mempool.
func waitForTxInMempool(ht *lntest.HarnessTest, txid string, timeout time.Duration) {
	txHash, err := chainhash.NewHashFromStr(txid)
	require.NoError(ht, err, "invalid txid")

	err = wait.Predicate(func() bool {
		mempool, err := ht.Miner().Client.GetRawMempool()
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
	mempool, err := ht.Miner().Client.GetRawMempoolVerbose()
	require.NoError(ht, err, "failed to get mempool")

	// Find the transaction spending the given outpoint.
	for txid, entry := range mempool {
		txHash2, err := chainhash.NewHashFromStr(txid)
		require.NoError(ht, err, "invalid txid")
		txRaw, err := ht.Miner().Client.GetRawTransactionVerbose(txHash2)
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
