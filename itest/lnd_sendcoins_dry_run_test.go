package itest

import (
	"bytes"
	"encoding/hex"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/stretchr/testify/require"
)

// testSendCoinsDryRun tests that a SendCoins dry run returns an unsigned
// transaction and its fee without broadcasting it or leaving any inputs
// locked, and that the reported fee matches the one of the real send.
func testSendCoinsDryRun(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", nil)

	ht.FundCoins(btcutil.Amount(500_000), alice)
	ht.FundCoins(btcutil.Amount(300_000), alice)

	destAddr := alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_TAPROOT_PUBKEY,
	}).Address

	// First we do a dry run of a regular send with a fixed fee rate.
	sendReq := &lnrpc.SendCoinsRequest{
		Addr:        destAddr,
		Amount:      100_000,
		SatPerVbyte: 5,
		DryRun:      true,
	}
	dryRunResp := alice.RPC.SendCoins(sendReq)
	dryRunTx := assertDryRunResp(ht, alice, dryRunResp)

	// Spending the same inputs for real should result in the same fee.
	sendReq.DryRun = false
	sendReq.Outpoints = txInputs(dryRunTx)
	sendResp := alice.RPC.SendCoins(sendReq)
	require.Empty(ht, sendResp.RawTx)
	require.Zero(ht, sendResp.FeeSat)
	require.Equal(
		ht, dryRunResp.FeeSat, txFee(ht, alice, sendResp.Txid),
	)
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Now we do the same for a send_all, which should sweep every UTXO
	// to the destination address in a single output.
	sweepReq := &lnrpc.SendCoinsRequest{
		Addr:        destAddr,
		SendAll:     true,
		SatPerVbyte: 5,
		DryRun:      true,
	}
	dryRunResp = alice.RPC.SendCoins(sweepReq)
	dryRunTx = assertDryRunResp(ht, alice, dryRunResp)
	require.Len(ht, dryRunTx.TxOut, 1)

	sweepReq.DryRun = false
	sweepResp := alice.RPC.SendCoins(sweepReq)
	require.Equal(
		ht, dryRunResp.FeeSat, txFee(ht, alice, sweepResp.Txid),
	)
	ht.MineBlocksAndAssertNumTxes(1, 1)
}

// assertDryRunResp asserts that the given dry run response contains an
// unsigned transaction and a fee, and that the transaction wasn't broadcast
// and didn't leave any of the wallet's outputs leased.
func assertDryRunResp(ht *lntest.HarnessTest, hn *node.HarnessNode,
	resp *lnrpc.SendCoinsResponse) *wire.MsgTx {

	rawTx, err := hex.DecodeString(resp.RawTx)
	require.NoError(ht, err)

	tx := &wire.MsgTx{}
	require.NoError(ht, tx.Deserialize(bytes.NewReader(rawTx)))
	require.Equal(ht, tx.TxHash().String(), resp.Txid)
	require.Positive(ht, resp.FeeSat)

	for _, txIn := range tx.TxIn {
		require.Empty(ht, txIn.SignatureScript)
		require.Empty(ht, txIn.Witness)
	}

	ht.AssertNumTxsInMempool(0)
	require.Empty(ht, hn.RPC.ListLeases().LockedUtxos)

	return tx
}

// txInputs returns the outpoints spent by the given transaction.
func txInputs(tx *wire.MsgTx) []*lnrpc.OutPoint {
	outpoints := make([]*lnrpc.OutPoint, 0, len(tx.TxIn))
	for _, txIn := range tx.TxIn {
		outpoints = append(outpoints, &lnrpc.OutPoint{
			TxidStr:     txIn.PreviousOutPoint.Hash.String(),
			OutputIndex: txIn.PreviousOutPoint.Index,
		})
	}

	return outpoints
}

// txFee returns the total fee of the given wallet transaction.
func txFee(ht *lntest.HarnessTest, hn *node.HarnessNode, txid string) int64 {
	txns := hn.RPC.GetTransactions(&lnrpc.GetTransactionsRequest{
		EndHeight: -1,
	})
	for _, tx := range txns.Transactions {
		if tx.TxHash == txid {
			return tx.TotalFees
		}
	}

	require.Failf(ht, "tx not found", "txid %v", txid)

	return 0
}
