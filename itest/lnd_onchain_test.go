package itest

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/sweep"
	"github.com/stretchr/testify/require"
)

// testChainKit tests ChainKit RPC endpoints.
func testChainKit(ht *lntest.HarnessTest) {
	// Test functions registered as test cases spin up separate nodes
	// during execution. By calling sub-test functions as seen below we
	// avoid the need to start separate nodes.
	testChainKitGetBlock(ht)
	testChainKitGetBlockHash(ht)
}

// testChainKitGetBlock ensures that given a block hash, the RPC endpoint
// returns the correct target block.
func testChainKitGetBlock(ht *lntest.HarnessTest) {
	// Get best block hash.
	bestBlockRes := ht.Alice.RPC.GetBestBlock(nil)

	var bestBlockHash chainhash.Hash
	err := bestBlockHash.SetBytes(bestBlockRes.BlockHash)
	require.NoError(ht, err)

	// Retrieve the best block by hash.
	getBlockReq := &chainrpc.GetBlockRequest{
		BlockHash: bestBlockHash[:],
	}
	getBlockRes := ht.Alice.RPC.GetBlock(getBlockReq)

	// Deserialize the block which was retrieved by hash.
	msgBlock := &wire.MsgBlock{}
	blockReader := bytes.NewReader(getBlockRes.RawBlock)
	err = msgBlock.Deserialize(blockReader)
	require.NoError(ht, err)

	// Ensure best block hash is the same as retrieved block hash.
	expected := bestBlockHash
	actual := msgBlock.BlockHash()
	require.Equal(ht, expected, actual)
}

// testChainKitGetBlockHash ensures that given a block height, the RPC endpoint
// returns the correct target block hash.
func testChainKitGetBlockHash(ht *lntest.HarnessTest) {
	// Get best block hash.
	bestBlockRes := ht.Alice.RPC.GetBestBlock(nil)

	// Retrieve the block hash at best block height.
	req := &chainrpc.GetBlockHashRequest{
		BlockHeight: int64(bestBlockRes.BlockHeight),
	}
	getBlockHashRes := ht.Alice.RPC.GetBlockHash(req)

	// Ensure best block hash is the same as retrieved block hash.
	expected := bestBlockRes.BlockHash
	actual := getBlockHashRes.BlockHash
	require.Equal(ht, expected, actual)
}

// testCPFP ensures that the daemon can bump an unconfirmed  transaction's fee
// rate by broadcasting a Child-Pays-For-Parent (CPFP) transaction.
//
// TODO(wilmer): Add RBF case once btcd supports it.
func testCPFP(ht *lntest.HarnessTest) {
	runCPFP(ht, ht.Alice, ht.Bob)
}

// runCPFP ensures that the daemon can bump an unconfirmed  transaction's fee
// rate by broadcasting a Child-Pays-For-Parent (CPFP) transaction.
func runCPFP(ht *lntest.HarnessTest, alice, bob *node.HarnessNode) {
	// Skip this test for neutrino, as it's not aware of mempool
	// transactions.
	if ht.IsNeutrinoBackend() {
		ht.Skipf("skipping CPFP test for neutrino backend")
	}

	// We'll start the test by sending Alice some coins, which she'll use
	// to send to Bob.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, alice)

	// Create an address for Bob to send the coins to.
	req := &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	}
	resp := bob.RPC.NewAddress(req)

	// Send the coins from Alice to Bob. We should expect a transaction to
	// be broadcast and seen in the mempool.
	sendReq := &lnrpc.SendCoinsRequest{
		Addr:   resp.Address,
		Amount: btcutil.SatoshiPerBitcoin,
	}
	alice.RPC.SendCoins(sendReq)
	txid := ht.Miner.AssertNumTxsInMempool(1)[0]

	// We'll then extract the raw transaction from the mempool in order to
	// determine the index of Bob's output.
	tx := ht.Miner.GetRawTransaction(txid)
	bobOutputIdx := -1
	for i, txOut := range tx.MsgTx().TxOut {
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(
			txOut.PkScript, ht.Miner.ActiveNet,
		)
		require.NoErrorf(ht, err, "unable to extract address "+
			"from pkScript=%x: %v", txOut.PkScript, err)

		if addrs[0].String() == resp.Address {
			bobOutputIdx = i
		}
	}
	require.NotEqual(ht, -1, bobOutputIdx, "bob's output was not found "+
		"within the transaction")

	// Wait until bob has seen the tx and considers it as owned.
	op := &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(bobOutputIdx),
	}
	ht.AssertUTXOInWallet(bob, op, "")

	// We'll attempt to bump the fee of this transaction by performing a
	// CPFP from Alice's point of view.
	bumpFeeReq := &walletrpc.BumpFeeRequest{
		Outpoint: op,
		SatPerVbyte: uint64(
			sweep.DefaultMaxFeeRate.FeePerKVByte() / 2000,
		),
	}
	bob.RPC.BumpFee(bumpFeeReq)

	// We should now expect to see two transactions within the mempool, a
	// parent and its child.
	ht.Miner.AssertNumTxsInMempool(2)

	// We should also expect to see the output being swept by the
	// UtxoSweeper. We'll ensure it's using the fee rate specified.
	pendingSweepsResp := bob.RPC.PendingSweeps()
	require.Len(ht, pendingSweepsResp.PendingSweeps, 1,
		"expected to find 1 pending sweep")
	pendingSweep := pendingSweepsResp.PendingSweeps[0]
	require.Equal(ht, pendingSweep.Outpoint.TxidBytes, op.TxidBytes,
		"output txid not matched")
	require.Equal(ht, pendingSweep.Outpoint.OutputIndex, op.OutputIndex,
		"output index not matched")
	require.Equal(ht, pendingSweep.SatPerVbyte, bumpFeeReq.SatPerVbyte,
		"sweep sat per vbyte not matched")

	// Mine a block to clean up the unconfirmed transactions.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// The input used to CPFP should no longer be pending.
	err := wait.NoError(func() error {
		resp := bob.RPC.PendingSweeps()
		if len(resp.PendingSweeps) != 0 {
			return fmt.Errorf("expected 0 pending sweeps, found %d",
				len(resp.PendingSweeps))
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err, "timeout checking bob's pending sweeps")
}

// testAnchorReservedValue tests that we won't allow sending transactions when
// that would take the value we reserve for anchor fee bumping out of our
// wallet.
func testAnchorReservedValue(ht *lntest.HarnessTest) {
	// Start two nodes supporting anchor channels.
	args := lntest.NodeArgsForCommitType(lnrpc.CommitmentType_ANCHORS)

	// NOTE: we cannot reuse the standby node here as the test requires the
	// node to start with no UTXOs.
	alice := ht.NewNode("Alice", args)
	bob := ht.Bob
	ht.RestartNodeWithExtraArgs(bob, args)

	ht.ConnectNodes(alice, bob)

	// Send just enough coins for Alice to open a channel without a change
	// output.
	const (
		chanAmt = 1000000
		feeEst  = 8000
	)

	ht.FundCoins(chanAmt+feeEst, alice)

	// wallet, without a change output. This should not be allowed.
	ht.OpenChannelAssertErr(
		alice, bob, lntest.OpenChannelParams{
			Amt: chanAmt,
		}, lnwallet.ErrReservedValueInvalidated,
	)

	// Alice opens a smaller channel. This works since it will have a
	// change output.
	chanPoint1 := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: chanAmt / 4},
	)

	// If Alice tries to open another anchor channel to Bob, Bob should not
	// reject it as he is not contributing any funds.
	chanPoint2 := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: chanAmt / 4},
	)

	// Similarly, if Alice tries to open a legacy channel to Bob, Bob
	// should not reject it as he is not contributing any funds. We'll
	// restart Bob to remove his support for anchors.
	ht.RestartNode(bob)

	// Before opening the channel, make sure the nodes are connected.
	ht.EnsureConnected(alice, bob)

	chanPoint3 := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: chanAmt / 4},
	)
	chanPoints := []*lnrpc.ChannelPoint{chanPoint1, chanPoint2, chanPoint3}

	// Alice tries to send all coins to an internal address. This is
	// allowed, since the final wallet balance will still be above the
	// reserved value.
	req := &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	}
	resp := alice.RPC.NewAddress(req)

	sweepReq := &lnrpc.SendCoinsRequest{
		Addr:    resp.Address,
		SendAll: true,
	}
	alice.RPC.SendCoins(sweepReq)

	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]

	assertNumTxInAndTxOut := func(tx *wire.MsgTx, in, out int) {
		require.Len(ht, tx.TxIn, in, "num inputs not matched")
		require.Len(ht, tx.TxOut, out, "num outputs not matched")
	}

	// The sweep transaction should have exactly one input, the change from
	// the previous SendCoins call.
	sweepTx := block.Transactions[1]

	// It should have a single output.
	assertNumTxInAndTxOut(sweepTx, 1, 1)

	// Wait for Alice to see her balance as confirmed.
	waitForConfirmedBalance := func() int64 {
		var balance int64
		err := wait.NoError(func() error {
			resp := alice.RPC.WalletBalance()

			if resp.TotalBalance == 0 {
				return fmt.Errorf("no balance")
			}

			if resp.UnconfirmedBalance > 0 {
				return fmt.Errorf("unconfirmed balance")
			}

			balance = resp.TotalBalance

			return nil
		}, defaultTimeout)
		require.NoError(ht, err, "timeout checking alice's balance")

		return balance
	}

	waitForConfirmedBalance()

	// Alice tries to send all funds to an external address, the reserved
	// value must stay in her wallet.
	minerAddr := ht.Miner.NewMinerAddress()

	sweepReq = &lnrpc.SendCoinsRequest{
		Addr:    minerAddr.String(),
		SendAll: true,
	}
	alice.RPC.SendCoins(sweepReq)

	// We'll mine a block which should include the sweep transaction we
	// generated above.
	block = ht.MineBlocksAndAssertNumTxes(1, 1)[0]

	// The sweep transaction should have exactly one inputs as we only had
	// the single output from above in the wallet.
	sweepTx = block.Transactions[1]

	// It should have two outputs, one being the miner address, the other
	// one being the reserve going back to our wallet.
	assertNumTxInAndTxOut(sweepTx, 1, 2)

	// The reserved value is now back in Alice's wallet.
	aliceBalance := waitForConfirmedBalance()

	// Alice closes channel, should now be allowed to send everything to an
	// external address.
	for _, chanPoint := range chanPoints {
		ht.CloseChannel(alice, chanPoint)
	}

	newBalance := waitForConfirmedBalance()
	require.Greater(ht, newBalance, aliceBalance,
		"Alice's balance did not increase after channel close")

	// Assert there are no open or pending channels anymore.
	ht.AssertNumWaitingClose(alice, 0)
	ht.AssertNodeNumChannels(alice, 0)

	// We'll wait for the balance to reflect that the channel has been
	// closed and the funds are in the wallet.
	sweepReq = &lnrpc.SendCoinsRequest{
		Addr:    minerAddr.String(),
		SendAll: true,
	}
	alice.RPC.SendCoins(sweepReq)

	// We'll mine a block which should include the sweep transaction we
	// generated above.
	block = ht.MineBlocksAndAssertNumTxes(1, 1)[0]

	// The sweep transaction should have four inputs, the change output from
	// the previous sweep, and the outputs from the coop closed channels.
	sweepTx = block.Transactions[1]

	// It should have a single output.
	assertNumTxInAndTxOut(sweepTx, 4, 1)
}

// testAnchorThirdPartySpend tests that if we force close a channel, but then
// don't sweep the anchor in time and a 3rd party spends it, that we remove any
// transactions that are a descendent of that sweep.
func testAnchorThirdPartySpend(ht *lntest.HarnessTest) {
	// First, we'll create two new nodes that both default to anchor
	// channels.
	//
	// NOTE: The itests differ here as anchors is default off vs the normal
	// lnd binary.
	args := lntest.NodeArgsForCommitType(lnrpc.CommitmentType_ANCHORS)
	alice := ht.NewNode("Alice", args)
	bob := ht.NewNode("Bob", args)

	ht.EnsureConnected(alice, bob)

	// We'll fund our Alice with coins, as she'll be opening the channel.
	// We'll fund her with *just* enough coins to open the channel and
	// sweep the anchor.
	const (
		firstChanSize   = 1_000_000
		anchorFeeBuffer = 500_000
	)
	ht.FundCoins(firstChanSize+anchorFeeBuffer, alice)

	// Open the channel between the two nodes and wait for it to confirm
	// fully.
	aliceChanPoint1 := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt: firstChanSize,
		},
	)

	// Send another UTXO if this is a neutrino backend. When sweeping
	// anchors, there are two transactions created, `local_sweep_tx` for
	// sweeping Alice's anchor on the local commitment, `remote_sweep_tx`
	// for sweeping her anchor on the remote commitment. Whenever the force
	// close transaction is published, Alice will always create these two
	// transactions to sweep her anchor.
	// On the other hand, when creating the sweep txes, the anchor itself
	// is not able to cover the fee, so another wallet UTXO is needed. In
	// our test case, there's a change output that can be used from the
	// above funding process. And it's used by both sweep txes - when `lnd`
	// happens to create the `remote_sweep_tx` first, it will receive an
	// error since its parent tx, the remote commitment, is not known,
	// hence freeing the change output to be used by `local_sweep_tx`.
	// For neutrino client, however, it will consider the transaction which
	// sweeps the remote anchor as an orphan tx, and it will neither send
	// it to the mempool nor return an error to free the change output.
	// Thus, if the change output is already used in `remote_sweep_tx`, we
	// won't have UTXO to create `local_sweep_tx`.
	//
	// NOTE: the order of the sweep requests for the two anchors cannot be
	// guaranteed. If the sweeper happens to sweep the remote anchor first,
	// then the test won't pass without the extra UTXO, which is the source
	// of the flakeness.
	//
	// TODO(yy): make a RPC server for sweeper so we can explicitly check
	// and control its state.
	if ht.IsNeutrinoBackend() {
		ht.FundCoins(anchorFeeBuffer, alice)
	}

	// With the channel open, we'll actually immediately force close it. We
	// don't care about network announcements here since there's no routing
	// in this test.
	ht.CloseChannelAssertPending(alice, aliceChanPoint1, true)

	// Now that the channel has been force closed, it should show up in the
	// PendingChannels RPC under the waiting close section.
	waitingClose := ht.AssertChannelWaitingClose(alice, aliceChanPoint1)

	// At this point, the channel is waiting close, and we have both the
	// commitment transaction and anchor sweep in the mempool.
	const expectedTxns = 2
	sweepTxns := ht.Miner.GetNumTxsFromMempool(expectedTxns)
	aliceCloseTx := waitingClose.Commitments.LocalTxid
	_, aliceAnchor := ht.FindCommitAndAnchor(sweepTxns, aliceCloseTx)

	// We'll now mine _only_ the commitment force close transaction, as we
	// want the anchor sweep to stay unconfirmed.
	forceCloseTxID, _ := chainhash.NewHashFromStr(aliceCloseTx)
	commitTxn := ht.Miner.GetRawTransaction(forceCloseTxID)
	ht.Miner.MineBlockWithTxes([]*btcutil.Tx{commitTxn})

	// Assert that the channel is now in PendingForceClose.
	//
	// NOTE: We must do this check to make sure `lnd` node has updated its
	// internal state regarding the closing transaction, otherwise the
	// `SendCoins` below might fail since it involves a reserved value
	// check, which requires a certain amount of coins to be reserved based
	// on the number of anchor channels.
	ht.AssertChannelPendingForceClose(alice, aliceChanPoint1)

	// With the anchor output located, and the main commitment mined we'll
	// instruct the wallet to send all coins in the wallet to a new address
	// (to the miner), including unconfirmed change.
	minerAddr := ht.Miner.NewMinerAddress()
	sweepReq := &lnrpc.SendCoinsRequest{
		Addr:             minerAddr.String(),
		SendAll:          true,
		MinConfs:         0,
		SpendUnconfirmed: true,
	}
	sweepAllResp := alice.RPC.SendCoins(sweepReq)

	// Both the original anchor sweep transaction, as well as the
	// transaction we created to sweep all the coins from Alice's wallet
	// should be found in her transaction store.
	sweepAllTxID, _ := chainhash.NewHashFromStr(sweepAllResp.Txid)
	ht.AssertTransactionInWallet(alice, aliceAnchor.SweepTx.TxHash())
	ht.AssertTransactionInWallet(alice, *sweepAllTxID)

	// Next, we'll shutdown Alice, and allow 16 blocks to pass so that the
	// anchor output can be swept by anyone. Rather than use the normal API
	// call, we'll generate a series of _empty_ blocks here.
	aliceRestart := ht.SuspendNode(alice)
	const anchorCsv = 16
	ht.MineEmptyBlocks(anchorCsv)

	// Before we sweep the anchor, we'll restart Alice.
	require.NoErrorf(ht, aliceRestart(), "unable to restart alice")

	// Now that the channel has been closed, and Alice has an unconfirmed
	// transaction spending the output produced by her anchor sweep, we'll
	// mine a transaction that double spends the output.
	thirdPartyAnchorSweep := genAnchorSweep(ht, aliceAnchor, anchorCsv)
	ht.Miner.MineBlockWithTxes([]*btcutil.Tx{thirdPartyAnchorSweep})

	// At this point, we should no longer find Alice's transaction that
	// tried to sweep the anchor in her wallet.
	ht.AssertTransactionNotInWallet(alice, aliceAnchor.SweepTx.TxHash())

	// In addition, the transaction she sent to sweep all her coins to the
	// miner also should no longer be found.
	ht.AssertTransactionNotInWallet(alice, *sweepAllTxID)

	// The anchor should now show as being "lost", while the force close
	// response is still present.
	assertAnchorOutputLost(ht, alice, aliceChanPoint1)

	// At this point Alice's CSV output should already be fully spent and
	// the channel marked as being resolved. We mine a block first, as so
	// far we've been generating custom blocks this whole time.
	commitSweepOp := wire.OutPoint{
		Hash:  *forceCloseTxID,
		Index: 1,
	}
	ht.Miner.AssertOutpointInMempool(commitSweepOp)
	ht.MineBlocks(1)

	ht.AssertNumWaitingClose(alice, 0)
}

// assertAnchorOutputLost asserts that the anchor output for the given channel
// has the state of being lost.
func assertAnchorOutputLost(ht *lntest.HarnessTest, hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint) {

	cp := ht.OutPointFromChannelPoint(chanPoint)

	expected := lnrpc.PendingChannelsResponse_ForceClosedChannel_LOST

	err := wait.NoError(func() error {
		resp := hn.RPC.PendingChannels()
		channels := resp.PendingForceClosingChannels

		for _, c := range channels {
			// Not the wanted channel, skipped.
			if c.Channel.ChannelPoint != cp.String() {
				continue
			}

			// Found the channel, check the anchor state.
			if c.Anchor == expected {
				return nil
			}

			return fmt.Errorf("unexpected anchor state, want %v, "+
				"got %v", expected, c.Anchor)
		}

		return fmt.Errorf("channel not found using cp=%v", cp)
	}, defaultTimeout)
	require.NoError(ht, err, "anchor doesn't show as being lost")
}

// genAnchorSweep generates a "3rd party" anchor sweeping from an existing one.
// In practice, we just re-use the existing witness, and track on our own
// output producing a 1-in-1-out transaction.
func genAnchorSweep(ht *lntest.HarnessTest,
	aliceAnchor *lntest.SweptOutput, anchorCsv uint32) *btcutil.Tx {

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

		require.FailNow(ht, "anchor op not found")

		return wire.TxIn{}
	}()

	// We'll set the signature on the input to nil, and then set the
	// sequence to 16 (the anchor CSV period).
	aliceAnchorTxIn.Witness[0] = nil
	aliceAnchorTxIn.Sequence = anchorCsv

	minerAddr := ht.Miner.NewMinerAddress()
	addrScript, err := txscript.PayToAddrScript(minerAddr)
	require.NoError(ht, err, "unable to gen addr script")

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
