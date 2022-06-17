package itest

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/sweep"
	"github.com/stretchr/testify/require"
)

// testCPFP ensures that the daemon can bump an unconfirmed  transaction's fee
// rate by broadcasting a Child-Pays-For-Parent (CPFP) transaction.
//
// TODO(wilmer): Add RBF case once btcd supports it.
func testCPFP(net *lntest.NetworkHarness, t *harnessTest) {
	runCPFP(net, t, net.Alice, net.Bob)
}

// runCPFP ensures that the daemon can bump an unconfirmed  transaction's fee
// rate by broadcasting a Child-Pays-For-Parent (CPFP) transaction.
func runCPFP(net *lntest.NetworkHarness, t *harnessTest,
	alice, bob *lntest.HarnessNode) {

	// Skip this test for neutrino, as it's not aware of mempool
	// transactions.
	if net.BackendCfg.Name() == lntest.NeutrinoBackendName {
		t.Skipf("skipping CPFP test for neutrino backend")
	}

	// We'll start the test by sending Alice some coins, which she'll use to
	// send to Bob.
	ctxb := context.Background()
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, alice)

	// Create an address for Bob to send the coins to.
	addrReq := &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	}
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	resp, err := bob.NewAddress(ctxt, addrReq)
	if err != nil {
		t.Fatalf("unable to get new address for bob: %v", err)
	}

	// Send the coins from Alice to Bob. We should expect a transaction to
	// be broadcast and seen in the mempool.
	sendReq := &lnrpc.SendCoinsRequest{
		Addr:   resp.Address,
		Amount: btcutil.SatoshiPerBitcoin,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if _, err = alice.SendCoins(ctxt, sendReq); err != nil {
		t.Fatalf("unable to send coins to bob: %v", err)
	}

	txid, err := waitForTxInMempool(net.Miner.Client, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("expected one mempool transaction: %v", err)
	}

	// We'll then extract the raw transaction from the mempool in order to
	// determine the index of Bob's output.
	tx, err := net.Miner.Client.GetRawTransaction(txid)
	if err != nil {
		t.Fatalf("unable to extract raw transaction from mempool: %v",
			err)
	}
	bobOutputIdx := -1
	for i, txOut := range tx.MsgTx().TxOut {
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(
			txOut.PkScript, net.Miner.ActiveNet,
		)
		if err != nil {
			t.Fatalf("unable to extract address from pkScript=%x: "+
				"%v", txOut.PkScript, err)
		}
		if addrs[0].String() == resp.Address {
			bobOutputIdx = i
		}
	}
	if bobOutputIdx == -1 {
		t.Fatalf("bob's output was not found within the transaction")
	}

	// Wait until bob has seen the tx and considers it as owned.
	op := &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(bobOutputIdx),
	}
	assertWalletUnspent(t, bob, op)

	// We'll attempt to bump the fee of this transaction by performing a
	// CPFP from Alice's point of view.
	bumpFeeReq := &walletrpc.BumpFeeRequest{
		Outpoint: op,
		SatPerVbyte: uint64(
			sweep.DefaultMaxFeeRate.FeePerKVByte() / 2000,
		),
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = bob.WalletKitClient.BumpFee(ctxt, bumpFeeReq)
	if err != nil {
		t.Fatalf("unable to bump fee: %v", err)
	}

	// We should now expect to see two transactions within the mempool, a
	// parent and its child.
	_, err = waitForNTxsInMempool(net.Miner.Client, 2, minerMempoolTimeout)
	if err != nil {
		t.Fatalf("expected two mempool transactions: %v", err)
	}

	// We should also expect to see the output being swept by the
	// UtxoSweeper. We'll ensure it's using the fee rate specified.
	pendingSweepsReq := &walletrpc.PendingSweepsRequest{}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	pendingSweepsResp, err := bob.WalletKitClient.PendingSweeps(
		ctxt, pendingSweepsReq,
	)
	if err != nil {
		t.Fatalf("unable to retrieve pending sweeps: %v", err)
	}
	if len(pendingSweepsResp.PendingSweeps) != 1 {
		t.Fatalf("expected to find %v pending sweep(s), found %v", 1,
			len(pendingSweepsResp.PendingSweeps))
	}
	pendingSweep := pendingSweepsResp.PendingSweeps[0]
	if !bytes.Equal(pendingSweep.Outpoint.TxidBytes, op.TxidBytes) {
		t.Fatalf("expected output txid %x, got %x", op.TxidBytes,
			pendingSweep.Outpoint.TxidBytes)
	}
	if pendingSweep.Outpoint.OutputIndex != op.OutputIndex {
		t.Fatalf("expected output index %v, got %v", op.OutputIndex,
			pendingSweep.Outpoint.OutputIndex)
	}
	if pendingSweep.SatPerVbyte != bumpFeeReq.SatPerVbyte {
		t.Fatalf("expected sweep sat per vbyte %v, got %v",
			bumpFeeReq.SatPerVbyte, pendingSweep.SatPerVbyte)
	}

	// Mine a block to clean up the unconfirmed transactions.
	mineBlocks(t, net, 1, 2)

	// The input used to CPFP should no longer be pending.
	err = wait.NoError(func() error {
		req := &walletrpc.PendingSweepsRequest{}
		ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
		resp, err := bob.WalletKitClient.PendingSweeps(ctxt, req)
		if err != nil {
			return fmt.Errorf("unable to retrieve bob's pending "+
				"sweeps: %v", err)
		}
		if len(resp.PendingSweeps) != 0 {
			return fmt.Errorf("expected 0 pending sweeps, found %d",
				len(resp.PendingSweeps))
		}
		return nil
	}, defaultTimeout)
	if err != nil {
		t.Fatalf(err.Error())
	}
}

// testAnchorReservedValue tests that we won't allow sending transactions when
// that would take the value we reserve for anchor fee bumping out of our
// wallet.
func testAnchorReservedValue(net *lntest.NetworkHarness, t *harnessTest) {
	// Start two nodes supporting anchor channels.
	args := nodeArgsForCommitType(lnrpc.CommitmentType_ANCHORS)
	alice := net.NewNode(t.t, "Alice", args)
	defer shutdownAndAssert(net, t, alice)

	bob := net.NewNode(t.t, "Bob", args)
	defer shutdownAndAssert(net, t, bob)

	ctxb := context.Background()
	net.ConnectNodes(t.t, alice, bob)

	// Send just enough coins for Alice to open a channel without a change
	// output.
	const (
		chanAmt = 1000000
		feeEst  = 8000
	)

	net.SendCoins(t.t, chanAmt+feeEst, alice)

	// wallet, without a change output. This should not be allowed.
	resErr := lnwallet.ErrReservedValueInvalidated.Error()

	_, err := net.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	if err == nil || !strings.Contains(err.Error(), resErr) {
		t.Fatalf("expected failure, got: %v", err)
	}

	// Alice opens a smaller channel. This works since it will have a
	// change output.
	aliceChanPoint1 := openChannelAndAssert(
		t, net, alice, bob, lntest.OpenChannelParams{
			Amt: chanAmt / 4,
		},
	)

	// If Alice tries to open another anchor channel to Bob, Bob should not
	// reject it as he is not contributing any funds.
	aliceChanPoint2 := openChannelAndAssert(
		t, net, alice, bob, lntest.OpenChannelParams{
			Amt: chanAmt / 4,
		},
	)

	// Similarly, if Alice tries to open a legacy channel to Bob, Bob should
	// not reject it as he is not contributing any funds. We'll restart Bob
	// to remove his support for anchors.
	err = net.RestartNode(bob, nil)
	require.NoError(t.t, err)
	aliceChanPoint3 := openChannelAndAssert(
		t, net, alice, bob, lntest.OpenChannelParams{
			Amt: chanAmt / 4,
		},
	)

	chanPoints := []*lnrpc.ChannelPoint{
		aliceChanPoint1, aliceChanPoint2, aliceChanPoint3,
	}
	for _, chanPoint := range chanPoints {
		err = alice.WaitForNetworkChannelOpen(chanPoint)
		require.NoError(t.t, err)

		err = bob.WaitForNetworkChannelOpen(chanPoint)
		require.NoError(t.t, err)
	}

	// Alice tries to send all coins to an internal address. This is
	// allowed, since the final wallet balance will still be above the
	// reserved value.
	addrReq := &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	}
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	resp, err := alice.NewAddress(ctxt, addrReq)
	require.NoError(t.t, err)

	sweepReq := &lnrpc.SendCoinsRequest{
		Addr:    resp.Address,
		SendAll: true,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = alice.SendCoins(ctxt, sweepReq)
	require.NoError(t.t, err)

	block := mineBlocks(t, net, 1, 1)[0]

	// The sweep transaction should have exactly one input, the change from
	// the previous SendCoins call.
	sweepTx := block.Transactions[1]
	if len(sweepTx.TxIn) != 1 {
		t.Fatalf("expected 1 inputs instead have %v", len(sweepTx.TxIn))
	}

	// It should have a single output.
	if len(sweepTx.TxOut) != 1 {
		t.Fatalf("expected 1 output instead have %v", len(sweepTx.TxOut))
	}

	// Wait for Alice to see her balance as confirmed.
	waitForConfirmedBalance := func() int64 {
		var balance int64
		err := wait.NoError(func() error {
			req := &lnrpc.WalletBalanceRequest{}
			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			resp, err := alice.WalletBalance(ctxt, req)
			if err != nil {
				return err
			}

			if resp.TotalBalance == 0 {
				return fmt.Errorf("no balance")
			}

			if resp.UnconfirmedBalance > 0 {
				return fmt.Errorf("unconfirmed balance")
			}

			balance = resp.TotalBalance
			return nil
		}, defaultTimeout)
		require.NoError(t.t, err)

		return balance
	}

	_ = waitForConfirmedBalance()

	// Alice tries to send all funds to an external address, the reserved
	// value must stay in her wallet.
	minerAddr, err := net.Miner.NewAddress()
	require.NoError(t.t, err)

	sweepReq = &lnrpc.SendCoinsRequest{
		Addr:    minerAddr.String(),
		SendAll: true,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = alice.SendCoins(ctxt, sweepReq)
	require.NoError(t.t, err)

	// We'll mine a block which should include the sweep transaction we
	// generated above.
	block = mineBlocks(t, net, 1, 1)[0]

	// The sweep transaction should have exactly one inputs as we only had
	// the single output from above in the wallet.
	sweepTx = block.Transactions[1]
	if len(sweepTx.TxIn) != 1 {
		t.Fatalf("expected 1 inputs instead have %v", len(sweepTx.TxIn))
	}

	// It should have two outputs, one being the miner address, the other
	// one being the reserve going back to our wallet.
	if len(sweepTx.TxOut) != 2 {
		t.Fatalf("expected 2 outputs instead have %v", len(sweepTx.TxOut))
	}

	// The reserved value is now back in Alice's wallet.
	aliceBalance := waitForConfirmedBalance()

	// The reserved value should be equal to the required reserve for anchor
	// channels.
	walletBalanceResp, err := alice.WalletBalance(
		ctxb, &lnrpc.WalletBalanceRequest{},
	)
	require.NoError(t.t, err)
	require.Equal(
		t.t, aliceBalance, walletBalanceResp.ReservedBalanceAnchorChan,
	)

	additionalChannels := int64(1)

	// Required reserve when additional channels are provided.
	requiredReserveResp, err := alice.WalletKitClient.RequiredReserve(
		ctxb, &walletrpc.RequiredReserveRequest{
			AdditionalPublicChannels: uint32(additionalChannels),
		},
	)
	require.NoError(t.t, err)

	additionalReservedValue := btcutil.Amount(additionalChannels *
		int64(lnwallet.AnchorChanReservedValue))
	totalReserved := btcutil.Amount(aliceBalance) + additionalReservedValue

	// The total reserved value should not exceed the maximum value reserved
	// for anchor channels.
	if totalReserved > lnwallet.MaxAnchorChanReservedValue {
		totalReserved = lnwallet.MaxAnchorChanReservedValue
	}
	require.Equal(
		t.t, int64(totalReserved), requiredReserveResp.RequiredReserve,
	)

	// Alice closes channel, should now be allowed to send everything to an
	// external address.
	for _, chanPoint := range chanPoints {
		closeChannelAndAssert(t, net, alice, chanPoint, false)
	}

	newBalance := waitForConfirmedBalance()
	if newBalance <= aliceBalance {
		t.Fatalf("Alice's balance did not increase after channel close")
	}

	// Assert there are no open or pending channels anymore.
	assertNumPendingChannels(t, alice, 0, 0)
	assertNodeNumChannels(t, alice, 0)

	// We'll wait for the balance to reflect that the channel has been
	// closed and the funds are in the wallet.
	sweepReq = &lnrpc.SendCoinsRequest{
		Addr:    minerAddr.String(),
		SendAll: true,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = alice.SendCoins(ctxt, sweepReq)
	require.NoError(t.t, err)

	// We'll mine a block which should include the sweep transaction we
	// generated above.
	block = mineBlocks(t, net, 1, 1)[0]

	// The sweep transaction should have four inputs, the change output from
	// the previous sweep, and the outputs from the coop closed channels.
	sweepTx = block.Transactions[1]
	if len(sweepTx.TxIn) != 4 {
		t.Fatalf("expected 4 inputs instead have %v", len(sweepTx.TxIn))
	}

	// It should have a single output.
	if len(sweepTx.TxOut) != 1 {
		t.Fatalf("expected 1 output instead have %v", len(sweepTx.TxOut))
	}
}

// genAnchorSweep generates a "3rd party" anchor sweeping from an existing one.
// In practice, we just re-use the existing witness, and track on our own
// output producing a 1-in-1-out transaction.
func genAnchorSweep(t *harnessTest, net *lntest.NetworkHarness,
	aliceAnchor *sweptOutput, anchorCsv uint32) *btcutil.Tx {

	// At this point, we have the transaction that Alice used to try to
	// sweep her anchor. As this is actually just something anyone can
	// spend, just need to find the input spending the anchor output, then
	// we can swap the output address.
	aliceAnchorTxIn := func() wire.TxIn {
		sweepCopy := aliceAnchor.SweepTx.Copy()
		for _, txIn := range sweepCopy.TxIn {
			if txIn.PreviousOutPoint == aliceAnchor.OutPoint {
				return *txIn
			}
		}

		t.Fatalf("anchor op not found")
		return wire.TxIn{}
	}()

	// We'll set the signature on the input to nil, and then set the
	// sequence to 16 (the anchor CSV period).
	aliceAnchorTxIn.Witness[0] = nil
	aliceAnchorTxIn.Sequence = anchorCsv

	minerAddr, err := net.Miner.NewAddress()
	if err != nil {
		t.Fatalf("unable to get miner addr: %v", err)
	}
	addrScript, err := txscript.PayToAddrScript(minerAddr)
	if err != nil {
		t.Fatalf("unable to gen addr script: %v", err)
	}

	// Now that we have the txIn, we can just make a new transaction that
	// uses a different script for the output.
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&aliceAnchorTxIn)
	tx.AddTxOut(&wire.TxOut{
		PkScript: addrScript,
		Value:    anchorSize - 1,
	})

	return btcutil.NewTx(tx)
}

// testAnchorThirdPartySpend tests that if we force close a channel, but then
// don't sweep the anchor in time and a 3rd party spends it, that we remove any
// transactions that are a descendent of that sweep.
func testAnchorThirdPartySpend(net *lntest.NetworkHarness, t *harnessTest) {
	// First, we'll create two new nodes that both default to anchor
	// channels.
	//
	// NOTE: The itests differ here as anchors is default off vs the normal
	// lnd binary.
	args := nodeArgsForCommitType(lnrpc.CommitmentType_ANCHORS)
	alice := net.NewNode(t.t, "Alice", args)
	defer shutdownAndAssert(net, t, alice)

	bob := net.NewNode(t.t, "Bob", args)
	defer shutdownAndAssert(net, t, bob)

	ctxb := context.Background()
	net.ConnectNodes(t.t, alice, bob)

	// We'll fund our Alice with coins, as she'll be opening the channel.
	// We'll fund her with *just* enough coins to open the channel.
	const (
		firstChanSize   = 1_000_000
		anchorFeeBuffer = 500_000
	)
	net.SendCoins(t.t, firstChanSize, alice)

	// We'll give Alice another spare UTXO as well so she can use it to
	// help sweep all coins.
	net.SendCoins(t.t, anchorFeeBuffer, alice)

	// Open the channel between the two nodes and wait for it to confirm
	// fully.
	aliceChanPoint1 := openChannelAndAssert(
		t, net, alice, bob, lntest.OpenChannelParams{
			Amt: firstChanSize,
		},
	)

	// With the channel open, we'll actually immediately force close it. We
	// don't care about network announcements here since there's no routing
	// in this test.
	_, _, err := net.CloseChannel(alice, aliceChanPoint1, true)
	if err != nil {
		t.Fatalf("unable to execute force channel closure: %v", err)
	}

	// Now that the channel has been force closed, it should show up in the
	// PendingChannels RPC under the waiting close section.
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	pendingChanResp, err := alice.PendingChannels(ctxt, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	err = checkNumWaitingCloseChannels(pendingChanResp, 1)
	if err != nil {
		t.Fatalf(err.Error())
	}

	// Get the normal channel outpoint so we can track it in the set of
	// channels that are waiting to be closed.
	fundingTxID, err := lnrpc.GetChanPointFundingTxid(aliceChanPoint1)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	chanPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: aliceChanPoint1.OutputIndex,
	}
	waitingClose, err := findWaitingCloseChannel(pendingChanResp, &chanPoint)
	if err != nil {
		t.Fatalf(err.Error())
	}

	// At this point, the channel is waiting close, and we have both the
	// commitment transaction and anchor sweep in the mempool.
	const expectedTxns = 2
	sweepTxns, err := getNTxsFromMempool(
		net.Miner.Client, expectedTxns, minerMempoolTimeout,
	)
	require.NoError(t.t, err, "no sweep txns in miner mempool")
	aliceCloseTx := waitingClose.Commitments.LocalTxid
	_, aliceAnchor := findCommitAndAnchor(t, net, sweepTxns, aliceCloseTx)

	// We'll now mine _only_ the commitment force close transaction, as we
	// want the anchor sweep to stay unconfirmed.
	var emptyTime time.Time
	forceCloseTxID, _ := chainhash.NewHashFromStr(aliceCloseTx)
	commitTxn, err := net.Miner.Client.GetRawTransaction(
		forceCloseTxID,
	)
	if err != nil {
		t.Fatalf("unable to get transaction: %v", err)
	}
	_, err = net.Miner.GenerateAndSubmitBlock(
		[]*btcutil.Tx{commitTxn}, -1, emptyTime,
	)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// With the anchor output located, and the main commitment mined we'll
	// instruct the wallet to send all coins in the wallet to a new address
	// (to the miner), including unconfirmed change.
	minerAddr, err := net.Miner.NewAddress()
	if err != nil {
		t.Fatalf("unable to create new miner addr: %v", err)
	}
	sweepReq := &lnrpc.SendCoinsRequest{
		Addr:             minerAddr.String(),
		SendAll:          true,
		MinConfs:         0,
		SpendUnconfirmed: true,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	sweepAllResp, err := alice.SendCoins(ctxt, sweepReq)
	if err != nil {
		t.Fatalf("unable to sweep coins: %v", err)
	}

	// Both the original anchor sweep transaction, as well as the
	// transaction we created to sweep all the coins from Alice's wallet
	// should be found in her transaction store.
	sweepAllTxID, _ := chainhash.NewHashFromStr(sweepAllResp.Txid)
	assertTransactionInWallet(t.t, alice, aliceAnchor.SweepTx.TxHash())
	assertTransactionInWallet(t.t, alice, *sweepAllTxID)

	// Next, we'll shutdown Alice, and allow 16 blocks to pass so that the
	// anchor output can be swept by anyone. Rather than use the normal API
	// call, we'll generate a series of _empty_ blocks here.
	aliceRestart, err := net.SuspendNode(alice)
	if err != nil {
		t.Fatalf("unable to shutdown alice: %v", err)
	}
	const anchorCsv = 16
	for i := 0; i < anchorCsv; i++ {
		_, err := net.Miner.GenerateAndSubmitBlock(nil, -1, emptyTime)
		if err != nil {
			t.Fatalf("unable to generate block: %v", err)
		}
	}

	// Before we sweep the anchor, we'll restart Alice.
	if err := aliceRestart(); err != nil {
		t.Fatalf("unable to restart alice: %v", err)
	}

	// Now that the channel has been closed, and Alice has an unconfirmed
	// transaction spending the output produced by her anchor sweep, we'll
	// mine a transaction that double spends the output.
	thirdPartyAnchorSweep := genAnchorSweep(t, net, aliceAnchor, anchorCsv)
	_, err = net.Miner.GenerateAndSubmitBlock(
		[]*btcutil.Tx{thirdPartyAnchorSweep}, -1, emptyTime,
	)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// At this point, we should no longer find Alice's transaction that
	// tried to sweep the anchor in her wallet.
	assertTransactionNotInWallet(t.t, alice, aliceAnchor.SweepTx.TxHash())

	// In addition, the transaction she sent to sweep all her coins to the
	// miner also should no longer be found.
	assertTransactionNotInWallet(t.t, alice, *sweepAllTxID)

	// The anchor should now show as being "lost", while the force close
	// response is still present.
	assertAnchorOutputLost(t, alice, chanPoint)

	// At this point Alice's CSV output should already be fully spent and
	// the channel marked as being resolved. We mine a block first, as so
	// far we've been generating custom blocks this whole time..
	commitSweepOp := wire.OutPoint{
		Hash:  *forceCloseTxID,
		Index: 1,
	}
	assertSpendingTxInMempool(
		t, net.Miner.Client, minerMempoolTimeout, commitSweepOp,
	)
	_, err = net.Miner.Client.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	assertNumPendingChannels(t, alice, 0, 0)
}
