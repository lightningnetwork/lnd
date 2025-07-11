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
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// testChainKit tests ChainKit RPC endpoints.
func testChainKit(ht *lntest.HarnessTest) {
	// Test functions registered as test cases spin up separate nodes
	// during execution. By calling sub-test functions as seen below we
	// avoid the need to start separate nodes.
	testChainKitGetBlock(ht)
	testChainKitGetBlockHeader(ht)
	testChainKitGetBlockHash(ht)
	testChainKitSendOutputsAnchorReserve(ht)
}

// testChainKitGetBlock ensures that given a block hash, the RPC endpoint
// returns the correct target block.
func testChainKitGetBlock(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", nil)

	// Get best block hash.
	bestBlockRes := alice.RPC.GetBestBlock(nil)

	var bestBlockHash chainhash.Hash
	err := bestBlockHash.SetBytes(bestBlockRes.BlockHash)
	require.NoError(ht, err)

	// Retrieve the best block by hash.
	getBlockReq := &chainrpc.GetBlockRequest{
		BlockHash: bestBlockHash[:],
	}
	getBlockRes := alice.RPC.GetBlock(getBlockReq)

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

// testChainKitGetBlockHeader ensures that given a block hash, the RPC endpoint
// returns the correct target block header.
func testChainKitGetBlockHeader(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", nil)

	// Get best block hash.
	bestBlockRes := alice.RPC.GetBestBlock(nil)

	var (
		bestBlockHash   chainhash.Hash
		bestBlockHeader wire.BlockHeader
		msgBlock        = &wire.MsgBlock{}
	)
	err := bestBlockHash.SetBytes(bestBlockRes.BlockHash)
	require.NoError(ht, err)

	// Retrieve the best block by hash.
	getBlockReq := &chainrpc.GetBlockRequest{
		BlockHash: bestBlockHash[:],
	}
	getBlockRes := alice.RPC.GetBlock(getBlockReq)

	// Deserialize the block which was retrieved by hash.
	blockReader := bytes.NewReader(getBlockRes.RawBlock)
	err = msgBlock.Deserialize(blockReader)
	require.NoError(ht, err)

	// Retrieve the block header for the best block.
	getBlockHeaderReq := &chainrpc.GetBlockHeaderRequest{
		BlockHash: bestBlockHash[:],
	}
	getBlockHeaderRes := alice.RPC.GetBlockHeader(getBlockHeaderReq)

	// Deserialize the block header which was retrieved by hash.
	blockHeaderReader := bytes.NewReader(getBlockHeaderRes.RawBlockHeader)
	err = bestBlockHeader.Deserialize(blockHeaderReader)
	require.NoError(ht, err)

	// Ensure the header of the best block is the same as retrieved block
	// header.
	expected := bestBlockHeader
	actual := msgBlock.Header
	require.Equal(ht, expected, actual)
}

// testChainKitGetBlockHash ensures that given a block height, the RPC endpoint
// returns the correct target block hash.
func testChainKitGetBlockHash(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", nil)

	// Get best block hash.
	bestBlockRes := alice.RPC.GetBestBlock(nil)

	// Retrieve the block hash at best block height.
	req := &chainrpc.GetBlockHashRequest{
		BlockHeight: int64(bestBlockRes.BlockHeight),
	}
	getBlockHashRes := alice.RPC.GetBlockHash(req)

	// Ensure best block hash is the same as retrieved block hash.
	expected := bestBlockRes.BlockHash
	actual := getBlockHashRes.BlockHash
	require.Equal(ht, expected, actual)
}

// testChainKitSendOutputsAnchorReserve checks if the SendOutputs rpc prevents
// our wallet balance to drop below the required anchor channel reserve amount.
func testChainKitSendOutputsAnchorReserve(ht *lntest.HarnessTest) {
	// Start two nodes supporting anchor channels.
	args := lntest.NodeArgsForCommitType(lnrpc.CommitmentType_ANCHORS)

	// NOTE: we cannot reuse the standby node here as the test requires the
	// node to start with no UTXOs.
	charlie := ht.NewNode("Charlie", args)
	bob := ht.NewNode("Bob", args)

	// We'll start the test by sending Charlie some coins.
	fundingAmount := btcutil.Amount(100_000)
	ht.FundCoins(fundingAmount, charlie)

	// Before opening the channel we ensure that the nodes are connected.
	ht.EnsureConnected(charlie, bob)

	// We'll get the anchor reserve that is required for a single channel.
	reserve := charlie.RPC.RequiredReserve(
		&walletrpc.RequiredReserveRequest{
			AdditionalPublicChannels: 1,
		},
	)

	// Charlie opens an anchor channel and keeps twice the amount of the
	// anchor reserve in her wallet.
	chanAmt := fundingAmount - 2*btcutil.Amount(reserve.RequiredReserve)
	ht.OpenChannel(charlie, bob, lntest.OpenChannelParams{
		Amt:            chanAmt,
		CommitmentType: lnrpc.CommitmentType_ANCHORS,
		SatPerVByte:    1,
	})

	// Now we obtain a taproot address from bob which Charlie will use to
	// send coins to him via the SendOutputs rpc.
	address := bob.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_TAPROOT_PUBKEY,
	})
	decodedAddr := ht.DecodeAddress(address.Address)
	addrScript := ht.PayToAddrScript(decodedAddr)

	// First she will try to send Bob an amount that would undershoot her
	// reserve requirement by one satoshi.
	balance := charlie.RPC.WalletBalance()
	utxo := &wire.TxOut{
		Value:    balance.TotalBalance - reserve.RequiredReserve + 1,
		PkScript: addrScript,
	}
	req := &walletrpc.SendOutputsRequest{
		Outputs: []*signrpc.TxOut{{
			Value:    utxo.Value,
			PkScript: utxo.PkScript,
		}},
		SatPerKw: 2400,
		MinConfs: 1,
	}

	// We try to send the reserve violating transaction and expect it to
	// fail.
	_, err := charlie.RPC.WalletKit.SendOutputs(ht.Context(), req)
	require.ErrorContains(ht, err, walletrpc.ErrInsufficientReserve.Error())

	ht.MineBlocksAndAssertNumTxes(1, 0)

	// Next she will try to send Bob an amount that just leaves enough
	// reserves in her wallet.
	utxo = &wire.TxOut{
		Value:    balance.TotalBalance - reserve.RequiredReserve,
		PkScript: addrScript,
	}
	req = &walletrpc.SendOutputsRequest{
		Outputs: []*signrpc.TxOut{{
			Value:    utxo.Value,
			PkScript: utxo.PkScript,
		}},
		SatPerKw: 2400,
		MinConfs: 1,
	}

	// This second transaction should be published correctly.
	charlie.RPC.SendOutputs(req)
	ht.MineBlocksAndAssertNumTxes(1, 1)
}

// testAnchorReservedValue tests that we won't allow sending transactions when
// that would take the value we reserve for anchor fee bumping out of our
// wallet.
func testAnchorReservedValue(ht *lntest.HarnessTest) {
	// Start two nodes supporting anchor channels.
	args := lntest.NodeArgsForCommitType(lnrpc.CommitmentType_ANCHORS)

	alice := ht.NewNode("Alice", args)
	bob := ht.NewNode("Bob", args)
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
		Addr:       resp.Address,
		SendAll:    true,
		TargetConf: 6,
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
	minerAddr := ht.NewMinerAddress()

	sweepReq = &lnrpc.SendCoinsRequest{
		Addr:       minerAddr.String(),
		SendAll:    true,
		TargetConf: 6,
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
		Addr:       minerAddr.String(),
		SendAll:    true,
		TargetConf: 6,
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
		testMemo        = "bob is a good peer"
	)
	ht.FundCoins(firstChanSize+anchorFeeBuffer, alice)

	// Open the channel between the two nodes and wait for it to confirm
	// fully.
	aliceChanPoint1 := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt:  firstChanSize,
			Memo: testMemo,
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

	// Verify that the channel Memo is returned even for channels that are
	// waiting close (close TX broadcasted but not confirmed)
	pendingChannelsResp := alice.RPC.PendingChannels()
	require.Equal(ht, testMemo,
		pendingChannelsResp.WaitingCloseChannels[0].Channel.Memo)

	// At this point, the channel is waiting close so we have the
	// commitment transaction in the mempool. Alice's anchor, however,
	// because there's no deadline pressure, it won't be swept.
	aliceCloseTx := waitingClose.Commitments.LocalTxid
	ht.MineBlocksAndAssertNumTxes(1, 1)
	forceCloseTxID, _ := chainhash.NewHashFromStr(aliceCloseTx)

	// Alice's should have the anchor sweep request. In addition, she should
	// see her immature to_local output sweep.
	ht.AssertNumPendingSweeps(alice, 2)

	// Mine 3 blocks so Alice will sweep her commit output.
	forceClose := ht.AssertChannelPendingForceClose(alice, aliceChanPoint1)
	ht.MineEmptyBlocks(int(forceClose.BlocksTilMaturity) - 1)

	// Alice's should have two sweep request - one for anchor output, the
	// other for commit output.
	sweeps := ht.AssertNumPendingSweeps(alice, 2)

	// Identify the sweep requests - the anchor sweep should have a smaller
	// deadline height since it's been offered to the sweeper earlier.
	anchor, commit := sweeps[0], sweeps[1]
	if anchor.DeadlineHeight > commit.DeadlineHeight {
		anchor, commit = commit, anchor
	}

	// We now update the anchor sweep's deadline to be different than the
	// commit sweep so they can won't grouped together.
	currentHeight := int32(ht.CurrentHeight())
	deadline := int32(commit.DeadlineHeight) - currentHeight
	require.Positive(ht, deadline)
	ht.Logf("Found commit deadline %d, anchor deadline %d",
		commit.DeadlineHeight, anchor.DeadlineHeight)

	// Update the anchor sweep's deadline and budget so it will always be
	// swept.
	bumpFeeReq := &walletrpc.BumpFeeRequest{
		Outpoint:      anchor.Outpoint,
		DeadlineDelta: uint32(deadline + 100),
		Budget:        uint64(anchor.AmountSat * 10),
		Immediate:     true,
	}
	alice.RPC.BumpFee(bumpFeeReq)

	// Wait until the anchor's deadline height is updated.
	err := wait.NoError(func() error {
		// Alice's should have two sweep request - one for anchor
		// output, the other for commit output.
		sweeps := ht.AssertNumPendingSweeps(alice, 2)

		if sweeps[0].DeadlineHeight != sweeps[1].DeadlineHeight {
			return nil
		}

		return fmt.Errorf("expected deadlines to be the different: %v",
			sweeps)
	}, wait.DefaultTimeout)
	require.NoError(ht, err, "deadline height not updated")

	// Mine one block to trigger Alice's sweeper to reconsider the anchor
	// sweeping - it will be swept with her commit output together in one
	// tx.
	txns := ht.GetNumTxsFromMempool(2)
	aliceSweep := txns[0]
	if aliceSweep.TxOut[0].Value > txns[1].TxOut[0].Value {
		aliceSweep = txns[1]
	}

	// Assert that the channel is now in PendingForceClose.
	//
	// NOTE: We must do this check to make sure `lnd` node has updated its
	// internal state regarding the closing transaction, otherwise the
	// `SendCoins` below might fail since it involves a reserved value
	// check, which requires a certain amount of coins to be reserved based
	// on the number of anchor channels.
	ht.AssertChannelPendingForceClose(alice, aliceChanPoint1)

	// Verify that the channel Memo is returned even for channels that are
	// pending force close (close TX confirmed but sweep hasn't happened)
	pendingChannelsResp = alice.RPC.PendingChannels()
	require.Equal(ht, testMemo,
		pendingChannelsResp.PendingForceClosingChannels[0].Channel.Memo)

	// With the anchor output located, and the main commitment mined we'll
	// instruct the wallet to send all coins in the wallet to a new address
	// (to the miner), including unconfirmed change.
	minerAddr := ht.NewMinerAddress()
	sweepReq := &lnrpc.SendCoinsRequest{
		Addr:             minerAddr.String(),
		SendAll:          true,
		TargetConf:       6,
		MinConfs:         0,
		SpendUnconfirmed: true,
	}
	sweepAllResp := alice.RPC.SendCoins(sweepReq)

	// Both the original anchor sweep transaction, as well as the
	// transaction we created to sweep all the coins from Alice's wallet
	// should be found in her transaction store.
	sweepAllTxID, _ := chainhash.NewHashFromStr(sweepAllResp.Txid)
	ht.AssertTransactionInWallet(alice, aliceSweep.TxHash())
	ht.AssertTransactionInWallet(alice, *sweepAllTxID)

	// Next, we mine enough blocks to pass so that the anchor output can be
	// swept by anyone. Rather than use the normal API call, we'll generate
	// a series of _empty_ blocks here.
	//
	// TODO(yy): also check the restart behavior of Alice.
	const anchorCsv = 16
	blocks := anchorCsv - defaultCSV

	// Mine empty blocks and check Alice still has the two pending sweeps.
	for i := 0; i < blocks; i++ {
		ht.MineEmptyBlocks(1)
		ht.AssertNumPendingSweeps(alice, 2)
	}

	// Now that the channel has been closed, and Alice has an unconfirmed
	// transaction spending the output produced by her anchor sweep, we'll
	// mine a transaction that double spends the output.
	thirdPartyAnchorSweep := genAnchorSweep(ht, aliceSweep, anchor.Outpoint)
	ht.Logf("Third party tx=%v", thirdPartyAnchorSweep.TxHash())
	ht.MineBlockWithTx(thirdPartyAnchorSweep)

	// At this point, we should no longer find Alice's transaction that
	// tried to sweep the anchor in her wallet.
	ht.AssertTransactionNotInWallet(alice, aliceSweep.TxHash())

	// In addition, the transaction she sent to sweep all her coins to the
	// miner also should no longer be found.
	ht.AssertTransactionNotInWallet(alice, *sweepAllTxID)

	// The anchor should now show as being "lost", while the force close
	// response is still present.
	assertAnchorOutputLost(ht, alice, aliceChanPoint1)

	// We now one block so Alice's commit output will be re-offered to her
	// sweeper again.
	ht.MineEmptyBlocks(1)
	ht.AssertNumPendingSweeps(alice, 1)

	// At this point Alice's CSV output should already be fully spent and
	// the channel marked as being resolved. We mine a block first, as so
	// far we've been generating custom blocks this whole time.
	commitSweepOp := wire.OutPoint{
		Hash:  *forceCloseTxID,
		Index: 1,
	}
	ht.AssertOutpointInMempool(commitSweepOp)
	ht.MineBlocksAndAssertNumTxes(1, 1)

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
func genAnchorSweep(ht *lntest.HarnessTest, aliceSweep *wire.MsgTx,
	aliceAnchor *lnrpc.OutPoint) *wire.MsgTx {

	var op wire.OutPoint
	copy(op.Hash[:], aliceAnchor.TxidBytes)
	op.Index = aliceAnchor.OutputIndex

	// At this point, we have the transaction that Alice used to try to
	// sweep her anchor. As this is actually just something anyone can
	// spend, just need to find the input spending the anchor output, then
	// we can swap the output address.
	aliceAnchorTxIn := func() wire.TxIn {
		sweepCopy := aliceSweep.Copy()
		for _, txIn := range sweepCopy.TxIn {
			if txIn.PreviousOutPoint == op {
				return *txIn
			}
		}

		require.FailNowf(ht, "cannot find anchor",
			"anchor op=%s not found in tx=%v", op,
			sweepCopy.TxHash())

		return wire.TxIn{}
	}()

	// We'll set the signature on the input to nil, and then set the
	// sequence to 16 (the anchor CSV period).
	aliceAnchorTxIn.Witness[0] = nil
	aliceAnchorTxIn.Sequence = 16

	minerAddr := ht.NewMinerAddress()
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

	return tx
}

// testRemoveTx tests that we are able to remove an unconfirmed transaction
// from the internal wallet as long as the tx is still unconfirmed. This test
// also verifies that after the tx is removed (while unconfirmed) it will show
// up as confirmed as soon as the original transaction is mined.
func testRemoveTx(ht *lntest.HarnessTest) {
	// Create a new node so that we start with no funds on the internal
	// wallet.
	alice := ht.NewNode("Alice", nil)

	const initialWalletAmt = btcutil.SatoshiPerBitcoin

	// Funding the node with an initial balance.
	ht.FundCoins(initialWalletAmt, alice)

	// Create an address for Alice to send the coins to.
	req := &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	}
	resp := alice.RPC.NewAddress(req)

	// We send half the amount to that address generating two unconfirmed
	// outpoints in our internal wallet.
	sendReq := &lnrpc.SendCoinsRequest{
		Addr:       resp.Address,
		Amount:     initialWalletAmt / 2,
		TargetConf: 6,
	}
	alice.RPC.SendCoins(sendReq)
	txID := ht.AssertNumTxsInMempool(1)[0]

	// Make sure the unspent number of utxos is 2 and the unconfirmed
	// balances add up.
	unconfirmed := ht.GetUTXOsUnconfirmed(
		alice, lnwallet.DefaultAccountName,
	)
	require.Lenf(ht, unconfirmed, 2, "number of unconfirmed tx")

	// Get the raw transaction to calculate the exact fee.
	tx := ht.GetNumTxsFromMempool(1)[0]

	// Calculate the tx fee so we can compare the end amounts. We are
	// sending from the internal wallet to the internal wallet so only
	// the tx fee applies when calucalting the final amount of the wallet.
	txFee := ht.CalculateTxFee(tx)

	// All of alice's balance is unconfirmed and equals the initial amount
	// minus the tx fee.
	aliceBalResp := alice.RPC.WalletBalance()
	expectedAmt := btcutil.Amount(initialWalletAmt) - txFee
	require.EqualValues(ht, expectedAmt, aliceBalResp.UnconfirmedBalance)

	// Now remove the transaction. We should see that the wallet state
	// equals the amount prior to sending the transaction. It is important
	// to understand that we do not remove any transaction from the mempool
	// (thats not possible in reality) we just remove it from our local
	// store.
	var buf bytes.Buffer
	require.NoError(ht, tx.Serialize(&buf))
	alice.RPC.RemoveTransaction(&walletrpc.GetTransactionRequest{
		Txid: txID.String(),
	})

	// Verify that the balance equals the initial state.
	confirmed := ht.GetUTXOsConfirmed(
		alice, lnwallet.DefaultAccountName,
	)
	require.Lenf(ht, confirmed, 1, "number confirmed tx")

	// Alice's balance should be the initial balance now because all the
	// unconfirmed tx got removed.
	aliceBalResp = alice.RPC.WalletBalance()
	expectedAmt = btcutil.Amount(initialWalletAmt)
	require.EqualValues(ht, expectedAmt, aliceBalResp.ConfirmedBalance)

	// Mine a block and make sure the transaction previously broadcasted
	// shows up in alice's wallet although we removed the transaction from
	// the wallet when it was unconfirmed.
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.AssertTxInBlock(block, txID)

	// Verify that alice has 2 confirmed unspent utxos in her default
	// wallet.
	err := wait.NoError(func() error {
		confirmed = ht.GetUTXOsConfirmed(
			alice, lnwallet.DefaultAccountName,
		)
		if len(confirmed) != 2 {
			return fmt.Errorf("expected 2 confirmed tx, "+
				" got %v", len(confirmed))
		}

		return nil
	}, lntest.DefaultTimeout)
	require.NoError(ht, err, "timeout checking for confirmed utxos")

	// The remaining balance should equal alice's starting balance minus the
	// tx fee.
	aliceBalResp = alice.RPC.WalletBalance()
	expectedAmt = btcutil.Amount(initialWalletAmt) - txFee
	require.EqualValues(ht, expectedAmt, aliceBalResp.ConfirmedBalance)
}

// testListSweeps tests that we are able to:
// - list only those sweeps that are currently in the mempool,
// - list sweeps given a starting block height,
// - list all sweeps.
func testListSweeps(ht *lntest.HarnessTest) {
	// Skip this test for neutrino, as it's not aware of mempool
	// transactions.
	if ht.IsNeutrinoBackend() {
		ht.Skipf("skipping ListSweeps test for neutrino backend")
	}

	// Create nodes so that we start with no funds on the internal wallet.
	alice := ht.NewNode("Alice", nil)
	bob := ht.NewNode("Bob", nil)

	const initialWalletAmt = btcutil.SatoshiPerBitcoin

	// Fund Alice with an initial balance.
	ht.FundCoins(initialWalletAmt, alice)

	// Connect Alice to Bob.
	ht.ConnectNodes(alice, bob)

	// Open a few channels between Alice and Bob.
	var chanPoints []*lnrpc.ChannelPoint
	for i := 0; i < 3; i++ {
		chanPoint := ht.OpenChannel(
			alice, bob, lntest.OpenChannelParams{
				Amt:     1e6,
				PushAmt: 5e5,
			},
		)

		chanPoints = append(chanPoints, chanPoint)
	}

	ht.Shutdown(bob)

	// Close the first channel and sweep the funds.
	ht.ForceCloseChannel(alice, chanPoints[0])

	// Jump a block.
	ht.MineEmptyBlocks(1)

	// Get the current block height.
	blockHeight := int32(ht.CurrentHeight())

	// Close the second channel and also sweep the funds.
	ht.ForceCloseChannel(alice, chanPoints[1])

	// Now close the third channel but don't sweep the funds just yet.
	closeStream, _ := ht.CloseChannelAssertPending(
		alice, chanPoints[2], true,
	)

	ht.AssertStreamChannelForceClosed(
		alice, chanPoints[2], false, closeStream,
	)

	// Mine enough blocks for the node to sweep its funds from the force
	// closed channel. The commit sweep resolver offers the outputs to the
	// sweeper up to one block before the CSV elapses, so wait until
	// defaulCSV-1.
	ht.MineEmptyBlocks(node.DefaultCSV - 1)
	ht.AssertNumPendingSweeps(alice, 1)

	// Mine a block to trigger the sweep.
	ht.MineEmptyBlocks(1)

	// Now we can expect that the sweep has been broadcast.
	ht.AssertNumTxsInMempool(1)

	// List all unconfirmed sweeps that alice's node had broadcast.
	req1 := &walletrpc.ListSweepsRequest{
		Verbose:     false,
		StartHeight: -1,
	}
	ht.AssertNumSweeps(alice, req1, 1)

	// Now list sweeps from the closing of the first channel. We should
	// only see the sweep from the second channel and the pending one.
	req2 := &walletrpc.ListSweepsRequest{
		Verbose:     false,
		StartHeight: blockHeight,
	}
	ht.AssertNumSweeps(alice, req2, 2)

	// Finally list all sweeps from the closing of the second channel. We
	// should see all sweeps, including the pending one.
	req3 := &walletrpc.ListSweepsRequest{
		Verbose:     false,
		StartHeight: 0,
	}
	ht.AssertNumSweeps(alice, req3, 3)

	// Mine the pending sweep and make sure it is no longer returned.
	ht.MineBlocksAndAssertNumTxes(1, 1)
	req4 := &walletrpc.ListSweepsRequest{
		Verbose:     false,
		StartHeight: -1,
	}
	ht.AssertNumSweeps(alice, req4, 0)
}
