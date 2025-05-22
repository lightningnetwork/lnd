import (
	"bytes"
	"strings"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/stretchr/testify/require"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
)

// testBumpFeeUntilMaxReached tests that the sweeper stops bumping the fee when
// the maximum fee rate is reached for a pending sweep.
func testBumpFeeUntilMaxReached(ht *lntest.HarnessTest) {
    // Set maxFeeRate to 100 sats/vbyte for this test to reduce iterations.
    maxFeeRate := uint64(100)

    // Start Alice with a custom max fee rate.
    alice := ht.NewNode("Alice", []string{"--sweeper.maxfeerate=100"})

    // Fund Alice with 1 BTC to ensure sufficient funds.
    ht.FundCoins(btcutil.SatoshiPerBitcoin, alice)

    // Create a transaction sending 100,000 sats to an address, generating a change output.
    addr := alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
        Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
    })
    sendReq := &lnrpc.SendCoinsRequest{
        Addr:       addr.Address,
        Amount:     100000,
        TargetConf: 1,
    }
    txid := alice.RPC.SendCoins(sendReq).Txid

    // Mine a block to confirm the transaction.
    ht.MineBlocks(1)

    // Get the UTXOs and find the change output from the transaction.
    utxos := alice.RPC.ListUnspent(&lnrpc.ListUnspentRequest{})
    require.Greater(ht, len(utxos.Utxos), 0, "no UTXOs found")

    txHash, _ := chainhash.NewHashFromStr(txid)
    var out *lnrpc.Utxo
    for _, u := range utxos.Utxos {
        if bytes.Equal(u.TxidBytes, txHash[:]) {
            out = u
            break
        }
    }
    require.NotNil(ht, out, "change UTXO not found")

    op := &lnrpc.OutPoint{
        TxidBytes:   out.TxidBytes,
        OutputIndex: out.OutputIndex,
    }

    // Start sweeping with BumpFee, using a generous budget and short deadline.
    bumpFeeReq := &walletrpc.BumpFeeRequest{
        Outpoint:      op,
        Immediate:     true,
        DeadlineDelta: 5,                  // Short deadline to reach max faster
        Budget:        uint64(out.AmountSat / 2), // 50% of output value
    }
    alice.RPC.BumpFee(bumpFeeReq)

    // Wait for the sweeping transaction to appear in the mempool.
    ht.WaitForTxInMempool(1, defaultTimeout)

    // Get initial fee rate from PendingSweeps.
    sweeps := ht.AssertNumPendingSweeps(alice, 1)
    initialFeeRate := sweeps[0].SatPerVbyte

    // Loop: Bump fee until maxFeeRate is reached or fee rate stops increasing.
    for i := 0; i < 20; i++ { // Cap iterations to prevent infinite loop
        ht.Logf("Bump attempt #%d, current fee rate: %d sats/vbyte", i, initialFeeRate)

        // Attempt to bump the fee.
        _, err := alice.RPC.WalletKit.BumpFee(ht.Context(), bumpFeeReq)
        if err != nil {
            // Check for max fee rate error (specific error message may vary).
            if strings.Contains(err.Error(), "max fee rate exceeded") ||
                strings.Contains(err.Error(), "position already at max") {
                ht.Logf("Max fee rate reached at attempt #%d", i)
                break
            }
            require.NoError(ht, err, "unexpected bump fee error")
        }

        // Wait for the new transaction to appear in the mempool.
        ht.WaitForTxInMempool(1, defaultTimeout)

        // Get updated fee rate from PendingSweeps.
        sweeps = ht.AssertNumPendingSweeps(alice, 1)
        currentFeeRate := sweeps[0].SatPerVbyte

        // Check if fee rate has reached max or stopped increasing.
        if currentFeeRate >= maxFeeRate || currentFeeRate <= initialFeeRate {
            break
        }
        initialFeeRate = currentFeeRate
    }

    // Final validation: Ensure fee rate equals maxFeeRate.
    sweeps = ht.AssertNumPendingSweeps(alice, 1)
    finalFeeRate := sweeps[0].SatPerVbyte
    require.Equal(ht, maxFeeRate, finalFeeRate, "final fee rate did not reach maxFeeRate")

    // Clean up by mining the transaction.
    ht.MineBlocks(1)
}
