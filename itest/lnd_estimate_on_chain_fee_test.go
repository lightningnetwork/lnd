package itest

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testEstimateOnChainFeeWithSelectedInputs tests that the EstimateFee RPC
// with specific input selection produces an accurate fee estimate that matches
// the actual fee when sending coins with the same inputs.
func testEstimateOnChainFeeWithSelectedInputs(ht *lntest.HarnessTest) {
	// Create a new node for this test.
	alice := ht.NewNode("Alice", nil)

	// Fund Alice with multiple UTXOs of different amounts to give us
	// several inputs to choose from.
	const (
		utxo1Amount = btcutil.Amount(500_000)
		utxo2Amount = btcutil.Amount(300_000)
		utxo3Amount = btcutil.Amount(200_000)
		targetConf  = 2
	)

	ht.FundCoins(utxo1Amount, alice)
	ht.FundCoins(utxo2Amount, alice)
	ht.FundCoins(utxo3Amount, alice)

	// List Alice's UTXOs to get their outpoints.
	utxosResp := alice.RPC.ListUnspent(&walletrpc.ListUnspentRequest{
		MinConfs: 1,
		MaxConfs: 1000,
	})
	require.GreaterOrEqual(ht, len(utxosResp.Utxos), 3,
		"expected at least 3 UTXOs")

	// Create a lookup map to find outpoints by amount.
	utxoLookup := make(map[int64]*lnrpc.OutPoint)
	for _, utxo := range utxosResp.Utxos {
		utxoLookup[utxo.AmountSat] = utxo.Outpoint
	}

	// Select the first two UTXOs for our test.
	selectedInputs := []*lnrpc.OutPoint{
		utxoLookup[int64(utxo1Amount)],
		utxoLookup[int64(utxo2Amount)],
	}
	require.NotNil(ht, selectedInputs[0], "first UTXO not found")
	require.NotNil(ht, selectedInputs[1], "second UTXO not found")

	// Generate a destination address for the transaction.
	destAddrResp := alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	})

	// Amount to send (less than the sum of selected UTXOs to allow for
	// fees and change).
	const sendAmount = int64(400_000)

	// Create an address-to-amount map for the transaction.
	addrToAmount := map[string]int64{
		destAddrResp.Address: sendAmount,
	}

	// Call EstimateFee with the selected inputs.
	estimateReq := &lnrpc.EstimateFeeRequest{
		AddrToAmount: addrToAmount,
		TargetConf:   targetConf,
		Inputs:       selectedInputs,
	}
	estimateResp, err := alice.RPC.LN.EstimateFee(ht.Context(), estimateReq)
	require.NoError(ht, err, "EstimateFee failed")

	ht.Logf("Fee estimate: %d sats, fee rate: %d sat/vbyte",
		estimateResp.FeeSat, estimateResp.SatPerVbyte)

	// Verify that the estimate response includes the inputs we specified.
	require.Len(ht, estimateResp.Inputs, 2,
		"expected 2 inputs in estimate response")

	// Create a map of input outpoints from the estimate for an easy lookup.
	estimateInputs := make(map[string]bool)
	for _, input := range estimateResp.Inputs {
		key := fmt.Sprintf("%s:%d", input.TxidStr, input.OutputIndex)
		estimateInputs[key] = true
	}

	// Verify each selected outpoint is in the estimate.
	for _, outpoint := range selectedInputs {
		key := fmt.Sprintf("%s:%d", outpoint.TxidStr,
			outpoint.OutputIndex)

		require.True(ht, estimateInputs[key],
			"outpoint %s not found in estimate inputs", key)
	}

	// Now actually send coins using the same parameters.
	sendReq := &lnrpc.SendCoinsRequest{
		Addr:       destAddrResp.Address,
		Amount:     sendAmount,
		TargetConf: targetConf,
		Outpoints:  selectedInputs,
	}
	sendResp := alice.RPC.SendCoins(sendReq)
	txid := sendResp.Txid

	ht.Logf("Transaction sent with txid: %s", txid)

	// Get the transaction details to extract the actual fee paid.
	txDetails := alice.RPC.GetTransactions(
		&lnrpc.GetTransactionsRequest{
			StartHeight: 0,
			EndHeight:   -1,
		},
	)

	// Find our transaction in the list.
	var actualFee int64
	var found bool
	for _, tx := range txDetails.Transactions {
		if tx.TxHash == txid {
			actualFee = tx.TotalFees
			found = true
			ht.Logf("Actual fee paid: %d sats", actualFee)

			break
		}
	}
	require.True(ht, found, "sent transaction not found")

	require.EqualValues(
		ht, estimateResp.FeeSat, actualFee, "fee estimate does not "+
			"match actual fee",
	)

	// Mine the SendCoinsRequest.
	ht.MineBlocksAndAssertNumTxes(1, 1)
}

// testEstimateOnChainFeeAutoSelectedInputs tests that the EstimateFee RPC
// without input selection allows the wallet to auto-select inputs, and that
// using those selected inputs in SendCoins produces a matching fee.
func testEstimateOnChainFeeAutoSelectedInputs(ht *lntest.HarnessTest) {
	// Create a new node for this test.
	alice := ht.NewNode("Alice", nil)

	// Fund Alice with multiple UTXOs.
	const (
		utxo1Amount = btcutil.Amount(500_000)
		utxo2Amount = btcutil.Amount(300_000)
		utxo3Amount = btcutil.Amount(200_000)
		targetConf  = 2
	)

	ht.FundCoins(utxo1Amount, alice)
	ht.FundCoins(utxo2Amount, alice)
	ht.FundCoins(utxo3Amount, alice)

	// Generate a destination address.
	destAddrResp := alice.RPC.NewAddress(
		&lnrpc.NewAddressRequest{
			Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
		},
	)

	const sendAmount = int64(100_000)
	addrToAmount := map[string]int64{
		destAddrResp.Address: sendAmount,
	}

	// Estimate fee without specifying inputs (wallet auto-selects).
	estimateReq := &lnrpc.EstimateFeeRequest{
		AddrToAmount: addrToAmount,
		TargetConf:   targetConf,
	}
	estimateResp, err := alice.RPC.LN.EstimateFee(ht.Context(), estimateReq)
	require.NoError(ht, err, "EstimateFee failed")

	ht.Logf("Fee estimate (auto-select): %d sats, "+
		"fee rate: %d sat/vbyte, inputs selected: %d",
		estimateResp.FeeSat, estimateResp.SatPerVbyte,
		len(estimateResp.Inputs))

	// The estimate should have selected some inputs.
	require.NotEmpty(ht, estimateResp.Inputs,
		"estimate should have selected inputs")

	// Send using the inputs that were selected by the estimate.
	sendReq := &lnrpc.SendCoinsRequest{
		Addr:       destAddrResp.Address,
		Amount:     sendAmount,
		TargetConf: targetConf,
		Outpoints:  estimateResp.Inputs,
	}
	sendResp := alice.RPC.SendCoins(sendReq)
	txid := sendResp.Txid

	ht.Logf("Transaction sent with txid: %s", txid)

	// Get the actual fee.
	txDetails := alice.RPC.GetTransactions(
		&lnrpc.GetTransactionsRequest{
			StartHeight: 0,
			EndHeight:   -1,
		},
	)

	var actualFee int64
	var found bool
	for _, tx := range txDetails.Transactions {
		if tx.TxHash == txid {
			actualFee = tx.TotalFees
			found = true
			ht.Logf("Actual fee paid: %d sats", actualFee)

			break
		}
	}
	require.True(ht, found, "sent transaction not found")

	require.EqualValues(
		ht, estimateResp.FeeSat, actualFee, "fee estimate does not "+
			"match actual fee",
	)

	// Mine the SendCoinsRequest.
	ht.MineBlocksAndAssertNumTxes(1, 1)
}
