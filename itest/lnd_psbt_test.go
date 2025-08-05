package itest

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
	"github.com/stretchr/testify/require"
)

// psbtFundingTestCases contains the test cases for funding via PSBT.
var psbtFundingTestCases = []*lntest.TestCase{
	{
		Name: "psbt funding anchor",
		TestFunc: func(ht *lntest.HarnessTest) {
			runPsbtChanFunding(
				ht, false, lnrpc.CommitmentType_ANCHORS,
			)
		},
	},
	{
		Name: "psbt external funding anchor",
		TestFunc: func(ht *lntest.HarnessTest) {
			runPsbtChanFundingExternal(
				ht, false, lnrpc.CommitmentType_ANCHORS,
			)
		},
	},
	{
		Name: "psbt single step funding anchor",
		TestFunc: func(ht *lntest.HarnessTest) {
			runPsbtChanFundingSingleStep(
				ht, false, lnrpc.CommitmentType_ANCHORS,
			)
		},
	},
	{
		Name: "psbt funding simple taproot",
		TestFunc: func(ht *lntest.HarnessTest) {
			runPsbtChanFunding(
				ht, true, lnrpc.CommitmentType_SIMPLE_TAPROOT,
			)
		},
	},
	{
		Name: "psbt external funding simple taproot",
		TestFunc: func(ht *lntest.HarnessTest) {
			runPsbtChanFundingExternal(
				ht, true, lnrpc.CommitmentType_SIMPLE_TAPROOT,
			)
		},
	},
	{
		Name: "psbt single step funding simple taproot",
		TestFunc: func(ht *lntest.HarnessTest) {
			runPsbtChanFundingSingleStep(
				ht, true, lnrpc.CommitmentType_SIMPLE_TAPROOT,
			)
		},
	},
}

// runPsbtChanFunding makes sure a channel can be opened between carol and dave
// by using a Partially Signed Bitcoin Transaction that funds the channel
// multisig funding output.
func runPsbtChanFunding(ht *lntest.HarnessTest, private bool,
	commitType lnrpc.CommitmentType) {

	args := lntest.NodeArgsForCommitType(commitType)

	// First, we'll create two new nodes that we'll use to open channels
	// between for this test. Dave gets some coins that will be used to
	// fund the PSBT, just to make sure that Carol has an empty wallet.
	carol := ht.NewNode("carol", args)
	dave := ht.NewNode("dave", args)

	// We just send enough funds to satisfy the anchor channel reserve for
	// 5 channels (50k sats).
	ht.FundCoins(50_000, carol)
	ht.FundCoins(50_000, dave)

	runPsbtChanFundingWithNodes(ht, carol, dave, private, commitType)
}

// runPsbtChanFundingWithNodes run a test case to make sure a channel can be
// opened between carol and dave by using a PSBT that funds the channel
// multisig funding output.
func runPsbtChanFundingWithNodes(ht *lntest.HarnessTest, carol,
	dave *node.HarnessNode, private bool, commitType lnrpc.CommitmentType) {

	const chanSize = funding.MaxBtcFundingAmount
	ht.FundCoins(btcutil.SatoshiPerBitcoin, dave)

	// Before we start the test, we'll ensure both sides are connected so
	// the funding flow can be properly executed.
	alice := ht.NewNodeWithCoins("Alice", nil)
	ht.EnsureConnected(carol, dave)
	ht.EnsureConnected(carol, alice)

	// At this point, we can begin our PSBT channel funding workflow. We'll
	// start by generating a pending channel ID externally that will be used
	// to track this new funding type.
	pendingChanID := ht.Random32Bytes()

	// We'll also test batch funding of two channels so we need another ID.
	pendingChanID2 := ht.Random32Bytes()

	// Now that we have the pending channel ID, Carol will open the channel
	// by specifying a PSBT shim. We use the NoPublish flag here to avoid
	// publishing the whole batch TX too early.
	chanUpdates, tempPsbt := ht.OpenChannelPsbt(
		carol, dave, lntest.OpenChannelParams{
			Amt: chanSize,
			FundingShim: &lnrpc.FundingShim{
				Shim: &lnrpc.FundingShim_PsbtShim{
					PsbtShim: &lnrpc.PsbtShim{
						PendingChanId: pendingChanID,
						NoPublish:     true,
					},
				},
			},
			Private:        private,
			CommitmentType: commitType,
		},
	)

	// If this is a taproot channel, then we'll decode the PSBT to assert
	// that an internal key is included.
	if commitType == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		decodedPSBT, err := psbt.NewFromRawBytes(
			bytes.NewReader(tempPsbt), false,
		)
		require.NoError(ht, err)

		require.Len(ht, decodedPSBT.Outputs[0].TaprootInternalKey, 32)
	}

	// Let's add a second channel to the batch. This time between Carol and
	// Alice. We will publish the batch TX once this channel funding is
	// complete.
	chanUpdates2, psbtBytes2 := ht.OpenChannelPsbt(
		carol, alice, lntest.OpenChannelParams{
			Amt: chanSize,
			FundingShim: &lnrpc.FundingShim{
				Shim: &lnrpc.FundingShim_PsbtShim{
					PsbtShim: &lnrpc.PsbtShim{
						PendingChanId: pendingChanID2,
						NoPublish:     false,
						BasePsbt:      tempPsbt,
					},
				},
			},
			// We haven't started Alice with the explicit params to
			// support the current commit type, so we'll just use
			// the default for this channel. That also allows us to
			// test batches of different channel types.
		},
	)

	// We'll now ask Dave's wallet to fund the PSBT for us. This will return
	// a packet with inputs and outputs set but without any witness data.
	// This is exactly what we need for the next step.
	fundReq := &walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_Psbt{
			Psbt: psbtBytes2,
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: 2,
		},
	}
	fundResp := dave.RPC.FundPsbt(fundReq)

	// We have a PSBT that has no witness data yet, which is exactly what we
	// need for the next step: Verify the PSBT with the funding intents.
	carol.RPC.FundingStateStep(&lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: pendingChanID,
				FundedPsbt:    fundResp.FundedPsbt,
			},
		},
	})
	carol.RPC.FundingStateStep(&lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: pendingChanID2,
				FundedPsbt:    fundResp.FundedPsbt,
			},
		},
	})

	// Now we'll ask Dave's wallet to sign the PSBT so we can finish the
	// funding flow.
	finalizeReq := &walletrpc.FinalizePsbtRequest{
		FundedPsbt: fundResp.FundedPsbt,
	}
	finalizeRes := dave.RPC.FinalizePsbt(finalizeReq)

	// We've signed our PSBT now, let's pass it to the intent again.
	carol.RPC.FundingStateStep(&lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtFinalize{
			PsbtFinalize: &lnrpc.FundingPsbtFinalize{
				PendingChanId: pendingChanID,
				SignedPsbt:    finalizeRes.SignedPsbt,
			},
		},
	})

	// Consume the "channel pending" update. This waits until the funding
	// transaction was fully compiled.
	updateResp := ht.ReceiveOpenChannelUpdate(chanUpdates)
	upd, ok := updateResp.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.True(ht, ok)
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: upd.ChanPending.Txid,
		},
		OutputIndex: upd.ChanPending.OutputIndex,
	}

	// No transaction should have been published yet.
	ht.AssertNumTxsInMempool(0)

	// Let's progress the second channel now. This time we'll use the raw
	// wire format transaction directly.
	carol.RPC.FundingStateStep(&lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtFinalize{
			PsbtFinalize: &lnrpc.FundingPsbtFinalize{
				PendingChanId: pendingChanID2,
				FinalRawTx:    finalizeRes.RawFinalTx,
			},
		},
	})

	// Consume the "channel pending" update for the second channel. This
	// waits until the funding transaction was fully compiled and in this
	// case published.
	updateResp2 := ht.ReceiveOpenChannelUpdate(chanUpdates2)
	upd2, ok := updateResp2.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.True(ht, ok)
	chanPoint2 := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: upd2.ChanPending.Txid,
		},
		OutputIndex: upd2.ChanPending.OutputIndex,
	}

	// Great, now we can mine a block to get the transaction confirmed, then
	// wait for the new channel to be propagated through the network.
	var finalTx wire.MsgTx
	err := finalTx.Deserialize(bytes.NewReader(finalizeRes.RawFinalTx))
	require.NoError(ht, err)

	txHash := finalTx.TxHash()
	block := ht.MineBlocksAndAssertNumTxes(6, 1)[0]
	ht.AssertTxInBlock(block, txHash)

	ht.AssertChannelActive(carol, chanPoint)
	ht.AssertChannelActive(carol, chanPoint2)
	ht.AssertChannelInGraph(carol, chanPoint)
	ht.AssertChannelInGraph(carol, chanPoint2)

	// With the channel open, ensure that it is counted towards Carol's
	// total channel balance.
	balRes := carol.RPC.ChannelBalance()
	require.NotZero(ht, balRes.LocalBalance.Sat)

	// Next, to make sure the channel functions as normal, we'll make some
	// payments within the channel.
	payAmt := btcutil.Amount(100000)
	invoice := &lnrpc.Invoice{
		Memo:  "new chans",
		Value: int64(payAmt),
	}
	resp := dave.RPC.AddInvoice(invoice)
	ht.CompletePaymentRequests(carol, []string{resp.PaymentRequest})
}

// runPsbtChanFundingExternal makes sure a channel can be opened between carol
// and dave by using a Partially Signed Bitcoin Transaction that funds the
// channel multisig funding output and is fully funded by an external third
// party.
func runPsbtChanFundingExternal(ht *lntest.HarnessTest, private bool,
	commitType lnrpc.CommitmentType) {

	args := lntest.NodeArgsForCommitType(commitType)

	// First, we'll create two new nodes that we'll use to open channels
	// between for this test. Dave gets some coins that will be used to
	// fund the PSBT, just to make sure that Carol has an empty wallet.
	carol := ht.NewNode("carol", args)
	dave := ht.NewNode("dave", args)

	// We just send enough funds to satisfy the anchor channel reserve for
	// 5 channels (50k sats).
	ht.FundCoins(50_000, carol)
	ht.FundCoins(50_000, dave)

	const chanSize = funding.MaxBtcFundingAmount

	// Before we start the test, we'll ensure both sides are connected so
	// the funding flow can be properly executed.
	alice := ht.NewNodeWithCoins("Alice", nil)
	ht.EnsureConnected(carol, dave)
	ht.EnsureConnected(carol, alice)

	// At this point, we can begin our PSBT channel funding workflow. We'll
	// start by generating a pending channel ID externally that will be used
	// to track this new funding type.
	pendingChanID := ht.Random32Bytes()

	// We'll also test batch funding of two channels so we need another ID.
	pendingChanID2 := ht.Random32Bytes()

	// Now that we have the pending channel ID, Carol will open the channel
	// by specifying a PSBT shim. We use the NoPublish flag here to avoid
	// publishing the whole batch TX too early.
	chanUpdates, tempPsbt := ht.OpenChannelPsbt(
		carol, dave, lntest.OpenChannelParams{
			Amt: chanSize,
			FundingShim: &lnrpc.FundingShim{
				Shim: &lnrpc.FundingShim_PsbtShim{
					PsbtShim: &lnrpc.PsbtShim{
						PendingChanId: pendingChanID,
						NoPublish:     true,
					},
				},
			},
			Private:        private,
			CommitmentType: commitType,
		},
	)

	// Let's add a second channel to the batch. This time between Carol and
	// Alice. We will publish the batch TX once this channel funding is
	// complete.
	chanUpdates2, psbtBytes2 := ht.OpenChannelPsbt(
		carol, alice, lntest.OpenChannelParams{
			Amt: chanSize,
			FundingShim: &lnrpc.FundingShim{
				Shim: &lnrpc.FundingShim_PsbtShim{
					PsbtShim: &lnrpc.PsbtShim{
						PendingChanId: pendingChanID2,
						NoPublish:     true,
						BasePsbt:      tempPsbt,
					},
				},
			},
			// We haven't started Alice with the explicit params to
			// support the current commit type, so we'll just use
			// the default for this channel. That also allows us to
			// test batches of different channel types.
		},
	)

	// We'll now ask Alice's wallet to fund the PSBT for us. This will
	// return a packet with inputs and outputs set but without any witness
	// data. This is exactly what we need for the next step.
	fundReq := &walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_Psbt{
			Psbt: psbtBytes2,
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: 2,
		},
	}
	fundResp := alice.RPC.FundPsbt(fundReq)

	// We have a PSBT that has no witness data yet, which is exactly what we
	// need for the next step: Verify the PSBT with the funding intents.
	// We tell the PSBT intent to skip the finalize step because we know the
	// final transaction will not be broadcast by Carol herself but by
	// Alice. And we assume that Alice is a third party that is not in
	// direct communication with Carol and won't send the signed TX to her
	// before broadcasting it. So we cannot call the finalize step but
	// instead just tell lnd to wait for a TX to be published/confirmed.
	carol.RPC.FundingStateStep(&lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: pendingChanID,
				FundedPsbt:    fundResp.FundedPsbt,
				SkipFinalize:  true,
			},
		},
	})
	carol.RPC.FundingStateStep(&lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: pendingChanID2,
				FundedPsbt:    fundResp.FundedPsbt,
				SkipFinalize:  true,
			},
		},
	})

	// Consume the "channel pending" update. This waits until the funding
	// transaction was fully compiled for both channels.
	updateResp := ht.ReceiveOpenChannelUpdate(chanUpdates)
	upd, ok := updateResp.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.True(ht, ok)
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: upd.ChanPending.Txid,
		},
		OutputIndex: upd.ChanPending.OutputIndex,
	}
	updateResp2 := ht.ReceiveOpenChannelUpdate(chanUpdates2)
	upd2, ok := updateResp2.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.True(ht, ok)
	chanPoint2 := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: upd2.ChanPending.Txid,
		},
		OutputIndex: upd2.ChanPending.OutputIndex,
	}
	ht.AssertNumPendingOpenChannels(carol, 2)

	// Now we'll ask Alice's wallet to sign the PSBT so we can finish the
	// funding flow.
	finalizeReq := &walletrpc.FinalizePsbtRequest{
		FundedPsbt: fundResp.FundedPsbt,
	}
	finalizeRes := alice.RPC.FinalizePsbt(finalizeReq)

	// No transaction should have been published yet.
	ht.AssertNumTxsInMempool(0)

	// Great, now let's publish the final raw transaction.
	var finalTx wire.MsgTx
	err := finalTx.Deserialize(bytes.NewReader(finalizeRes.RawFinalTx))
	require.NoError(ht, err)

	txHash := finalTx.TxHash()
	_, err = ht.SendRawTransaction(&finalTx, false)
	require.NoError(ht, err)

	// Now we can mine a block to get the transaction confirmed, then wait
	// for the new channel to be propagated through the network.
	block := ht.MineBlocksAndAssertNumTxes(6, 1)[0]
	ht.AssertTxInBlock(block, txHash)
	ht.AssertChannelInGraph(carol, chanPoint)
	ht.AssertChannelInGraph(carol, chanPoint2)

	// With the channel open, ensure that it is counted towards Carol's
	// total channel balance.
	balRes := carol.RPC.ChannelBalance()
	require.NotZero(ht, balRes.LocalBalance.Sat)

	// Next, to make sure the channel functions as normal, we'll make some
	// payments within the channel.
	payAmt := btcutil.Amount(100000)
	invoice := &lnrpc.Invoice{
		Memo:  "new chans",
		Value: int64(payAmt),
	}
	resp := dave.RPC.AddInvoice(invoice)
	ht.CompletePaymentRequests(carol, []string{resp.PaymentRequest})
}

// runPsbtChanFundingSingleStep checks whether PSBT funding works also when
// the wallet of both nodes are empty and one of them uses PSBT and an external
// wallet to fund the channel while creating reserve output in the same
// transaction.
func runPsbtChanFundingSingleStep(ht *lntest.HarnessTest, private bool,
	commitType lnrpc.CommitmentType) {

	args := lntest.NodeArgsForCommitType(commitType)

	// First, we'll create two new nodes that we'll use to open channels
	// between for this test.
	carol := ht.NewNode("carol", args)
	dave := ht.NewNode("dave", args)

	const chanSize = funding.MaxBtcFundingAmount

	alice := ht.NewNodeWithCoins("Alice", nil)

	// Get new address for anchor reserve.
	req := &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	}
	addrResp := carol.RPC.NewAddress(req)
	reserveAddr, err := btcutil.DecodeAddress(
		addrResp.Address, harnessNetParams,
	)
	require.NoError(ht, err)
	reserveAddrScript, err := txscript.PayToAddrScript(reserveAddr)
	require.NoError(ht, err)

	// Before we start the test, we'll ensure both sides are connected so
	// the funding flow can be properly executed.
	ht.EnsureConnected(carol, dave)

	// At this point, we can begin our PSBT channel funding workflow. We'll
	// start by generating a pending channel ID externally that will be used
	// to track this new funding type.
	pendingChanID := ht.Random32Bytes()

	// Now that we have the pending channel ID, Carol will open the channel
	// by specifying a PSBT shim.
	chanUpdates, tempPsbt := ht.OpenChannelPsbt(
		carol, dave, lntest.OpenChannelParams{
			Amt: chanSize,
			FundingShim: &lnrpc.FundingShim{
				Shim: &lnrpc.FundingShim_PsbtShim{
					PsbtShim: &lnrpc.PsbtShim{
						PendingChanId: pendingChanID,
						NoPublish:     false,
					},
				},
			},
			Private:        private,
			CommitmentType: commitType,
		},
	)

	decodedPsbt, err := psbt.NewFromRawBytes(
		bytes.NewReader(tempPsbt), false,
	)
	require.NoError(ht, err)

	reserveTxOut := wire.TxOut{
		Value:    10000,
		PkScript: reserveAddrScript,
	}

	decodedPsbt.UnsignedTx.TxOut = append(
		decodedPsbt.UnsignedTx.TxOut, &reserveTxOut,
	)
	decodedPsbt.Outputs = append(decodedPsbt.Outputs, psbt.POutput{})

	var psbtBytes bytes.Buffer
	err = decodedPsbt.Serialize(&psbtBytes)
	require.NoError(ht, err)

	fundReq := &walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_Psbt{
			Psbt: psbtBytes.Bytes(),
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: 2,
		},
	}
	fundResp := alice.RPC.FundPsbt(fundReq)

	// Make sure the wallets are actually empty
	ht.AssertNumUTXOsUnconfirmed(alice, 0)
	ht.AssertNumUTXOsUnconfirmed(dave, 0)

	// We have a PSBT that has no witness data yet, which is exactly what we
	// need for the next step: Verify the PSBT with the funding intents.
	carol.RPC.FundingStateStep(&lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: pendingChanID,
				FundedPsbt:    fundResp.FundedPsbt,
			},
		},
	})

	// Now we'll ask Alice's wallet to sign the PSBT so we can finish the
	// funding flow.
	finalizeReq := &walletrpc.FinalizePsbtRequest{
		FundedPsbt: fundResp.FundedPsbt,
	}
	finalizeRes := alice.RPC.FinalizePsbt(finalizeReq)

	// We've signed our PSBT now, let's pass it to the intent again.
	carol.RPC.FundingStateStep(&lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtFinalize{
			PsbtFinalize: &lnrpc.FundingPsbtFinalize{
				PendingChanId: pendingChanID,
				SignedPsbt:    finalizeRes.SignedPsbt,
			},
		},
	})

	// Consume the "channel pending" update. This waits until the funding
	// transaction was fully compiled.
	updateResp := ht.ReceiveOpenChannelUpdate(chanUpdates)
	upd, ok := updateResp.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.True(ht, ok)
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: upd.ChanPending.Txid,
		},
		OutputIndex: upd.ChanPending.OutputIndex,
	}

	var finalTx wire.MsgTx
	err = finalTx.Deserialize(bytes.NewReader(finalizeRes.RawFinalTx))
	require.NoError(ht, err)

	txHash := finalTx.TxHash()
	block := ht.MineBlocksAndAssertNumTxes(6, 1)[0]
	ht.AssertTxInBlock(block, txHash)
	ht.AssertChannelInGraph(carol, chanPoint)

	// Next, to make sure the channel functions as normal, we'll make some
	// payments within the channel.
	payAmt := btcutil.Amount(100000)
	invoice := &lnrpc.Invoice{
		Memo:  "new chans",
		Value: int64(payAmt),
	}
	resp := dave.RPC.AddInvoice(invoice)
	ht.CompletePaymentRequests(carol, []string{resp.PaymentRequest})
}

// testSignPsbt tests that the SignPsbt RPC works correctly.
func testSignPsbt(ht *lntest.HarnessTest) {
	psbtTestRunners := []struct {
		name   string
		runner func(*lntest.HarnessTest, *node.HarnessNode)
	}{
		{
			name:   "sign psbt segwit v0 P2WPKH",
			runner: runSignPsbtSegWitV0P2WKH,
		},
		{
			name:   "sign psbt segwit v0 P2WSH",
			runner: runSignPsbtSegWitV0NP2WKH,
		},
		{
			name:   "sign psbt segwit v1 key spend bip86",
			runner: runSignPsbtSegWitV1KeySpendBip86,
		},
		{
			name:   "sign psbt segwit v1 key spend root hash",
			runner: runSignPsbtSegWitV1KeySpendRootHash,
		},
		{
			name:   "sign psbt segwit v1 script spend",
			runner: runSignPsbtSegWitV1ScriptSpend,
		},
		{
			// The above tests all make sure we can sign for keys
			// that aren't in the wallet. But we also want to make
			// sure we can fund and then sign PSBTs from our
			// wallet.
			name:   "fund and sign psbt",
			runner: runFundAndSignPsbt,
		},
	}

	for _, tc := range psbtTestRunners {
		succeed := ht.Run(tc.name, func(t *testing.T) {
			st := ht.Subtest(t)
			alice := st.NewNodeWithCoins("Alice", nil)
			tc.runner(st, alice)
		})

		// Abort the test if failed.
		if !succeed {
			return
		}
	}
}

// runSignPsbtSegWitV0P2WKH tests that the SignPsbt RPC works correctly for a
// SegWit v0 p2wkh input.
func runSignPsbtSegWitV0P2WKH(ht *lntest.HarnessTest, alice *node.HarnessNode) {
	// We test that we can sign a PSBT that spends funds from an input that
	// the wallet doesn't know about. To set up that test case, we first
	// derive an address manually that the wallet won't be watching on
	// chain. We can do that by exporting the account xpub of lnd's main
	// account.
	accounts := alice.RPC.ListAccounts(&walletrpc.ListAccountsRequest{})
	require.NotEmpty(ht, accounts.Accounts)

	// We also need to parse the accounts, so we have easy access to the
	// parsed derivation paths.
	parsedAccounts, err := walletrpc.AccountsToWatchOnly(accounts.Accounts)
	require.NoError(ht, err)

	account := parsedAccounts[0]
	xpub, err := hdkeychain.NewKeyFromString(account.Xpub)
	require.NoError(ht, err)

	const (
		changeIndex = 1
		addrIndex   = 1337
	)
	fullDerivationPath := []uint32{
		hdkeychain.HardenedKeyStart + account.Purpose,
		hdkeychain.HardenedKeyStart + account.CoinType,
		hdkeychain.HardenedKeyStart + account.Account,
		changeIndex,
		addrIndex,
	}

	// Let's simulate a change address.
	change, err := xpub.DeriveNonStandard(changeIndex)
	require.NoError(ht, err)

	// At an index that we are certainly not watching in the wallet.
	addrKey, err := change.DeriveNonStandard(addrIndex)
	require.NoError(ht, err)

	addrPubKey, err := addrKey.ECPubKey()
	require.NoError(ht, err)
	pubKeyHash := btcutil.Hash160(addrPubKey.SerializeCompressed())
	witnessAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		pubKeyHash, harnessNetParams,
	)
	require.NoError(ht, err)

	pkScript, err := txscript.PayToAddrScript(witnessAddr)
	require.NoError(ht, err)

	// Send some funds to the output and then try to get a signature through
	// the SignPsbt RPC to spend that output again.
	assertPsbtSpend(
		ht, alice, pkScript,
		func(packet *psbt.Packet) {
			in := &packet.Inputs[0]
			in.Bip32Derivation = []*psbt.Bip32Derivation{{
				PubKey:    addrPubKey.SerializeCompressed(),
				Bip32Path: fullDerivationPath,
			}}
			in.SighashType = txscript.SigHashAll
		},
		func(packet *psbt.Packet) {
			require.Len(ht, packet.Inputs, 1)
			require.Len(ht, packet.Inputs[0].PartialSigs, 1)

			partialSig := packet.Inputs[0].PartialSigs[0]
			require.Equal(
				ht, partialSig.PubKey,
				addrPubKey.SerializeCompressed(),
			)
			require.Greater(
				ht, len(partialSig.Signature), ecdsa.MinSigLen,
			)
		},
	)
}

// runSignPsbtSegWitV0NP2WKH tests that the SignPsbt RPC works correctly for a
// SegWit v0 np2wkh input.
func runSignPsbtSegWitV0NP2WKH(ht *lntest.HarnessTest,
	alice *node.HarnessNode) {

	// We test that we can sign a PSBT that spends funds from an input that
	// the wallet doesn't know about. To set up that test case, we first
	// derive an address manually that the wallet won't be watching on
	// chain. We can do that by exporting the account xpub of lnd's main
	// account.
	accounts := alice.RPC.ListAccounts(&walletrpc.ListAccountsRequest{})
	require.NotEmpty(ht, accounts.Accounts)

	// We also need to parse the accounts, so we have easy access to the
	// parsed derivation paths.
	parsedAccounts, err := walletrpc.AccountsToWatchOnly(accounts.Accounts)
	require.NoError(ht, err)

	account := parsedAccounts[0]
	xpub, err := hdkeychain.NewKeyFromString(account.Xpub)
	require.NoError(ht, err)

	const (
		changeIndex = 1
		addrIndex   = 1337
	)
	fullDerivationPath := []uint32{
		hdkeychain.HardenedKeyStart + account.Purpose,
		hdkeychain.HardenedKeyStart + account.CoinType,
		hdkeychain.HardenedKeyStart + account.Account,
		changeIndex,
		addrIndex,
	}

	// Let's simulate a change address.
	change, err := xpub.DeriveNonStandard(changeIndex)
	require.NoError(ht, err)

	// At an index that we are certainly not watching in the wallet.
	addrKey, err := change.DeriveNonStandard(addrIndex)
	require.NoError(ht, err)

	addrPubKey, err := addrKey.ECPubKey()
	require.NoError(ht, err)
	pubKeyHash := btcutil.Hash160(addrPubKey.SerializeCompressed())
	witnessAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		pubKeyHash, harnessNetParams,
	)
	require.NoError(ht, err)

	witnessProgram, err := txscript.PayToAddrScript(witnessAddr)
	require.NoError(ht, err)
	np2wkhAddr, err := btcutil.NewAddressScriptHash(
		witnessProgram, harnessNetParams,
	)
	require.NoError(ht, err)

	pkScript, err := txscript.PayToAddrScript(np2wkhAddr)
	require.NoError(ht, err)

	// Send some funds to the output and then try to get a signature through
	// the SignPsbt RPC to spend that output again.
	assertPsbtSpend(
		ht, alice, pkScript,
		func(packet *psbt.Packet) {
			in := &packet.Inputs[0]
			in.RedeemScript = witnessProgram
			in.Bip32Derivation = []*psbt.Bip32Derivation{{
				PubKey:    addrPubKey.SerializeCompressed(),
				Bip32Path: fullDerivationPath,
			}}
			in.SighashType = txscript.SigHashAll
		},
		func(packet *psbt.Packet) {
			require.Len(ht, packet.Inputs, 1)
			require.Len(ht, packet.Inputs[0].PartialSigs, 1)

			partialSig := packet.Inputs[0].PartialSigs[0]
			require.Equal(
				ht, partialSig.PubKey,
				addrPubKey.SerializeCompressed(),
			)
			require.Greater(
				ht, len(partialSig.Signature), ecdsa.MinSigLen,
			)
		},
	)
}

// runSignPsbtSegWitV1KeySpendBip86 tests that the SignPsbt RPC works correctly
// for a SegWit v1 p2tr key spend BIP-0086 input.
func runSignPsbtSegWitV1KeySpendBip86(ht *lntest.HarnessTest,
	alice *node.HarnessNode) {

	// Derive a key we can use for signing.
	keyDesc, internalKey, fullDerivationPath := deriveInternalKey(ht, alice)

	// Our taproot key is a BIP0086 key spend only construction that just
	// commits to the internal key and no root hash.
	taprootKey := txscript.ComputeTaprootKeyNoScript(internalKey)
	tapScriptAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey), harnessNetParams,
	)
	require.NoError(ht, err)
	p2trPkScript, err := txscript.PayToAddrScript(tapScriptAddr)
	require.NoError(ht, err)

	// Send some funds to the output and then try to get a signature through
	// the SignPsbt RPC to spend that output again.
	assertPsbtSpend(
		ht, alice, p2trPkScript,
		func(packet *psbt.Packet) {
			in := &packet.Inputs[0]
			in.Bip32Derivation = []*psbt.Bip32Derivation{{
				PubKey:    keyDesc.RawKeyBytes,
				Bip32Path: fullDerivationPath,
			}}
			p2trDerivation := []*psbt.TaprootBip32Derivation{{
				XOnlyPubKey: keyDesc.RawKeyBytes[1:],
				Bip32Path:   fullDerivationPath,
			}}
			in.TaprootBip32Derivation = p2trDerivation
			in.SighashType = txscript.SigHashDefault
		},
		func(packet *psbt.Packet) {
			require.Len(ht, packet.Inputs, 1)
			require.Len(
				ht, packet.Inputs[0].TaprootKeySpendSig, 64,
			)
		},
	)
}

// runSignPsbtSegWitV1KeySpendRootHash tests that the SignPsbt RPC works
// correctly for a SegWit v1 p2tr key spend that also commits to a script tree
// root hash.
func runSignPsbtSegWitV1KeySpendRootHash(ht *lntest.HarnessTest,
	alice *node.HarnessNode) {

	// Derive a key we can use for signing.
	keyDesc, internalKey, fullDerivationPath := deriveInternalKey(ht, alice)

	// Let's create a taproot script output now. This is a hash lock with a
	// simple preimage of "foobar".
	leaf1 := testScriptHashLock(ht.T, []byte("foobar"))

	rootHash := leaf1.TapHash()
	taprootKey := txscript.ComputeTaprootOutputKey(internalKey, rootHash[:])
	tapScriptAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey), harnessNetParams,
	)
	require.NoError(ht, err)
	p2trPkScript, err := txscript.PayToAddrScript(tapScriptAddr)
	require.NoError(ht, err)

	// Send some funds to the output and then try to get a signature through
	// the SignPsbt RPC to spend that output again.
	assertPsbtSpend(
		ht, alice, p2trPkScript,
		func(packet *psbt.Packet) {
			in := &packet.Inputs[0]
			in.Bip32Derivation = []*psbt.Bip32Derivation{{
				PubKey:    keyDesc.RawKeyBytes,
				Bip32Path: fullDerivationPath,
			}}
			p2trDerivation := []*psbt.TaprootBip32Derivation{{
				XOnlyPubKey: keyDesc.RawKeyBytes[1:],
				Bip32Path:   fullDerivationPath,
			}}
			in.TaprootBip32Derivation = p2trDerivation
			in.TaprootMerkleRoot = rootHash[:]
			in.SighashType = txscript.SigHashDefault
		},
		func(packet *psbt.Packet) {
			require.Len(ht, packet.Inputs, 1)
			require.Len(
				ht, packet.Inputs[0].TaprootKeySpendSig, 64,
			)
		},
	)
}

// runSignPsbtSegWitV1ScriptSpend tests that the SignPsbt RPC works correctly
// for a SegWit v1 p2tr script spend.
func runSignPsbtSegWitV1ScriptSpend(ht *lntest.HarnessTest,
	alice *node.HarnessNode) {

	// Derive a key we can use for signing.
	keyDesc, internalKey, fullDerivationPath := deriveInternalKey(ht, alice)

	// Let's create a taproot script output now. This is a hash lock with a
	// simple preimage of "foobar".
	leaf1 := testScriptSchnorrSig(ht.T, internalKey)

	rootHash := leaf1.TapHash()
	taprootKey := txscript.ComputeTaprootOutputKey(internalKey, rootHash[:])
	tapScriptAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey), harnessNetParams,
	)
	require.NoError(ht, err)
	p2trPkScript, err := txscript.PayToAddrScript(tapScriptAddr)
	require.NoError(ht, err)

	// We need to assemble the control block to be able to spend through the
	// script path.
	tapscript := input.TapscriptPartialReveal(internalKey, leaf1, nil)
	controlBlockBytes, err := tapscript.ControlBlock.ToBytes()
	require.NoError(ht, err)

	// Send some funds to the output and then try to get a signature through
	// the SignPsbt RPC to spend that output again.
	assertPsbtSpend(
		ht, alice, p2trPkScript,
		func(packet *psbt.Packet) {
			in := &packet.Inputs[0]
			in.Bip32Derivation = []*psbt.Bip32Derivation{{
				PubKey:    keyDesc.RawKeyBytes,
				Bip32Path: fullDerivationPath,
			}}
			p2trDerivation := []*psbt.TaprootBip32Derivation{{
				XOnlyPubKey: keyDesc.RawKeyBytes[1:],
				Bip32Path:   fullDerivationPath,
				LeafHashes:  [][]byte{rootHash[:]},
			}}
			in.TaprootBip32Derivation = p2trDerivation
			in.SighashType = txscript.SigHashDefault
			in.TaprootLeafScript = []*psbt.TaprootTapLeafScript{{
				ControlBlock: controlBlockBytes,
				Script:       leaf1.Script,
				LeafVersion:  leaf1.LeafVersion,
			}}
		},
		func(packet *psbt.Packet) {
			require.Len(ht, packet.Inputs, 1)
			require.Len(
				ht, packet.Inputs[0].TaprootScriptSpendSig, 1,
			)

			spendSig := packet.Inputs[0].TaprootScriptSpendSig[0]
			require.Len(ht, spendSig.Signature, 64)
		},
	)
}

// runFundAndSignPsbt makes sure we can sign PSBTs that were funded by our
// internal wallet.
func runFundAndSignPsbt(ht *lntest.HarnessTest, alice *node.HarnessNode) {
	alice.AddToLogf("================ runFundAndSignPsbt ===============")

	// We'll be using a "main" address where we send the funds to and from
	// several times.
	mainAddrResp := alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	})

	fundOutputs := map[string]uint64{
		mainAddrResp.Address: 999000,
	}
	spendAddrTypes := []lnrpc.AddressType{
		lnrpc.AddressType_NESTED_PUBKEY_HASH,
		lnrpc.AddressType_WITNESS_PUBKEY_HASH,
		lnrpc.AddressType_TAPROOT_PUBKEY,
	}
	changeAddrTypes := []walletrpc.ChangeAddressType{
		walletrpc.ChangeAddressType_CHANGE_ADDRESS_TYPE_UNSPECIFIED,
		walletrpc.ChangeAddressType_CHANGE_ADDRESS_TYPE_P2TR,
	}

	for _, addrType := range spendAddrTypes {
		for _, changeType := range changeAddrTypes {
			ht.Logf("testing with address type %s and "+
				"change address type %s", addrType, changeType)

			// First, spend all the coins in the wallet to an
			// address of the given type so that UTXO will be picked
			// when funding a PSBT.
			sendAllCoinsToAddrType(ht, alice, addrType)

			// Let's fund a PSBT now where we want to send a few
			// sats to our main address.
			assertPsbtFundSignSpend(
				ht, alice, fundOutputs, changeType, false,
			)

			// Send all coins back to a single address once again.
			sendAllCoinsToAddrType(ht, alice, addrType)

			// And now make sure the alternate way of signing a
			// PSBT, which is calling FinalizePsbt directly, also
			// works for this address type.
			assertPsbtFundSignSpend(
				ht, alice, fundOutputs, changeType, true,
			)
		}
	}
}

// testFundPsbt tests the FundPsbt RPC use cases that aren't covered by the PSBT
// channel funding tests above. These specifically are the use cases of funding
// a PSBT that already specifies an input but where the user still wants the
// wallet to perform coin selection.
func testFundPsbt(ht *lntest.HarnessTest) {
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)

	runFundPsbt(ht, alice, bob)
}

// runFundPsbt tests the FundPsbt RPC use case where we want to fund a PSBT
// that already has an input specified. This is a pay-join scenario where Bob
// wants to send Alice some coins, but he wants to do so in a way that doesn't
// reveal the full amount he is sending.
func runFundPsbt(ht *lntest.HarnessTest, alice, bob *node.HarnessNode) {
	// We test a pay-join between Alice and Bob. Bob wants to send Alice
	// 5 million Satoshis in a non-obvious way. So Bob selects a UTXO that's
	// bigger than 5 million Satoshis and expects the change minus the send
	// amount back. Alice then funds the PSBT with coins of her own and
	// combines her change with the 5 million Satoshis from Bob. With this
	// Alice ends up paying the fees for a transfer to her.
	const sendAmount = 5_000_000
	aliceAddr := alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_TAPROOT_PUBKEY,
	})
	bobAddr := bob.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_TAPROOT_PUBKEY,
	})

	alice.UpdateState()
	bob.UpdateState()
	aliceStartBalance := alice.State.Wallet.TotalBalance
	bobStartBalance := bob.State.Wallet.TotalBalance

	var bobUtxo *lnrpc.Utxo
	bobUnspent := bob.RPC.ListUnspent(&walletrpc.ListUnspentRequest{})
	for _, utxo := range bobUnspent.Utxos {
		if utxo.AmountSat > sendAmount {
			bobUtxo = utxo
			break
		}
	}
	if bobUtxo == nil {
		ht.Fatalf("Bob doesn't have a UTXO of at least %d sats",
			sendAmount)
	}

	bobUtxoTxHash, err := chainhash.NewHash(bobUtxo.Outpoint.TxidBytes)
	require.NoError(ht, err)

	tx := wire.NewMsgTx(2)
	tx.TxIn = append(tx.TxIn, &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  *bobUtxoTxHash,
			Index: bobUtxo.Outpoint.OutputIndex,
		},
	})
	tx.TxOut = append(tx.TxOut, &wire.TxOut{
		// Change going back to Bob.
		PkScript: addressToPkScript(ht, bobAddr.Address),
		Value:    bobUtxo.AmountSat - sendAmount,
	}, &wire.TxOut{
		// Amount to be sent to Alice, but we'll also send her change
		// here.
		PkScript: addressToPkScript(ht, aliceAddr.Address),
		Value:    sendAmount,
	})

	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(ht, err)

	derivation, trDerivation := getAddressBip32Derivation(
		ht, bobUtxo.Address, bob,
	)

	bobUtxoPkScript, _ := hex.DecodeString(bobUtxo.PkScript)
	firstInput := &packet.Inputs[0]
	firstInput.WitnessUtxo = &wire.TxOut{
		PkScript: bobUtxoPkScript,
		Value:    bobUtxo.AmountSat,
	}
	firstInput.Bip32Derivation = []*psbt.Bip32Derivation{derivation}
	firstInput.TaprootBip32Derivation = []*psbt.TaprootBip32Derivation{
		trDerivation,
	}
	if txscript.IsPayToWitnessPubKeyHash(bobUtxoPkScript) {
		packet.Inputs[0].SighashType = txscript.SigHashAll
	}

	// We have the template now. Bob basically funds the 5 million Sats to
	// send to Alice and Alice now only needs to coin select to pay for the
	// fees.
	fundedPacket := fundPsbtCoinSelect(ht, alice, packet, 1)
	txFee, err := fundedPacket.GetTxFee()
	require.NoError(ht, err)

	// We now let Bob sign the transaction.
	signedPacket := signPacket(ht, bob, fundedPacket)

	// And then Alice, which should give us a fully signed TX.
	signedPacket = signPacket(ht, alice, signedPacket)

	// We should be able to finalize the PSBT and extract the final TX now.
	extractPublishAndMine(ht, alice, signedPacket)

	// Make sure the new wallet balances are reflected correctly.
	ht.AssertActiveNodesSynced()
	alice.UpdateState()
	bob.UpdateState()

	require.Equal(
		ht, aliceStartBalance+sendAmount-int64(txFee),
		alice.State.Wallet.TotalBalance,
	)
	require.Equal(
		ht, bobStartBalance-sendAmount,
		bob.State.Wallet.TotalBalance,
	)
}

// addressToPkScript parses the given address string and returns the pkScript
// for the regtest environment.
func addressToPkScript(t testing.TB, addr string) []byte {
	parsed, err := btcutil.DecodeAddress(addr, harnessNetParams)
	require.NoError(t, err)

	pkScript, err := txscript.PayToAddrScript(parsed)
	require.NoError(t, err)

	return pkScript
}

// getAddressBip32Derivation returns the PSBT BIP-0032 derivation info of an
// address.
func getAddressBip32Derivation(t testing.TB, addr string,
	node *node.HarnessNode) (*psbt.Bip32Derivation,
	*psbt.TaprootBip32Derivation) {

	// We can't query a single address directly, so we just query all wallet
	// addresses.
	addresses := node.RPC.ListAddresses(
		&walletrpc.ListAddressesRequest{},
	)

	var (
		path        []uint32
		pubKeyBytes []byte
		err         error
	)
	for _, account := range addresses.AccountWithAddresses {
		for _, address := range account.Addresses {
			if address.Address == addr {
				path, err = lntest.ParseDerivationPath(
					address.DerivationPath,
				)
				require.NoError(t, err)

				pubKeyBytes = address.PublicKey
			}
		}
	}

	if len(path) != 5 || len(pubKeyBytes) == 0 {
		t.Fatalf("Derivation path for address %s not found or invalid",
			addr)
	}

	// The actual derivation path in a PSBT needs to be using the hardened
	// uint32 notation for the first three elements.
	path[0] += hdkeychain.HardenedKeyStart
	path[1] += hdkeychain.HardenedKeyStart
	path[2] += hdkeychain.HardenedKeyStart

	return &psbt.Bip32Derivation{
			PubKey:    pubKeyBytes,
			Bip32Path: path,
		}, &psbt.TaprootBip32Derivation{
			XOnlyPubKey: pubKeyBytes[1:],
			Bip32Path:   path,
		}
}

// fundPsbtCoinSelect calls the FundPsbt RPC on the given node using the coin
// selection with template PSBT mode.
func fundPsbtCoinSelect(t testing.TB, node *node.HarnessNode,
	packet *psbt.Packet, changeIndex int32) *psbt.Packet {

	var buf bytes.Buffer
	err := packet.Serialize(&buf)
	require.NoError(t, err)

	cs := &walletrpc.PsbtCoinSelect{
		Psbt: buf.Bytes(),
	}
	if changeIndex >= 0 {
		cs.ChangeOutput = &walletrpc.PsbtCoinSelect_ExistingOutputIndex{
			ExistingOutputIndex: 1,
		}
	} else {
		cs.ChangeOutput = &walletrpc.PsbtCoinSelect_Add{
			Add: true,
		}
	}

	fundResp := node.RPC.FundPsbt(&walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_CoinSelect{
			CoinSelect: cs,
		},
		Fees: &walletrpc.FundPsbtRequest_TargetConf{
			TargetConf: 1,
		},
	})

	fundedPacket, err := psbt.NewFromRawBytes(
		bytes.NewReader(fundResp.FundedPsbt), false,
	)
	require.NoError(t, err)

	return fundedPacket
}

// signPacket calls the SignPsbt RPC on the given node.
func signPacket(t testing.TB, node *node.HarnessNode,
	packet *psbt.Packet) *psbt.Packet {

	var buf bytes.Buffer
	err := packet.Serialize(&buf)
	require.NoError(t, err)

	signResp := node.RPC.SignPsbt(&walletrpc.SignPsbtRequest{
		FundedPsbt: buf.Bytes(),
	})

	// Let's make sure we have a partial signature.
	signedPacket, err := psbt.NewFromRawBytes(
		bytes.NewReader(signResp.SignedPsbt), false,
	)
	require.NoError(t, err)

	return signedPacket
}

// extractAndPublish extracts the final transaction from the packet and
// publishes it with the given node, mines a block and asserts the TX was mined
// successfully.
func extractPublishAndMine(ht *lntest.HarnessTest, node *node.HarnessNode,
	packet *psbt.Packet) *wire.MsgTx {

	err := psbt.MaybeFinalizeAll(packet)
	require.NoError(ht, err)

	finalTx, err := psbt.Extract(packet)
	require.NoError(ht, err)

	var buf bytes.Buffer
	err = finalTx.Serialize(&buf)
	require.NoError(ht, err)

	// Publish the second transaction and then mine both of them.
	node.RPC.PublishTransaction(&walletrpc.Transaction{TxHex: buf.Bytes()})

	// Mine one block which should contain two transactions.
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	txHash := finalTx.TxHash()
	ht.AssertTxInBlock(block, txHash)

	return finalTx
}

// assertPsbtSpend creates an output with the given pkScript on chain and then
// attempts to create a sweep transaction that is signed using the SignPsbt RPC
// that spends that output again.
func assertPsbtSpend(ht *lntest.HarnessTest, alice *node.HarnessNode,
	pkScript []byte, decorateUnsigned func(*psbt.Packet),
	verifySigned func(*psbt.Packet)) {

	// Let's send some coins to that address now.
	utxo := &wire.TxOut{
		Value:    600_000,
		PkScript: pkScript,
	}
	req := &walletrpc.SendOutputsRequest{
		Outputs: []*signrpc.TxOut{{
			Value:    utxo.Value,
			PkScript: utxo.PkScript,
		}},
		MinConfs:         0,
		SpendUnconfirmed: true,
		SatPerKw:         2500,
	}
	resp := alice.RPC.SendOutputs(req)

	prevTx := wire.NewMsgTx(2)
	err := prevTx.Deserialize(bytes.NewReader(resp.RawTx))
	require.NoError(ht, err)

	prevOut := -1
	for idx, txOut := range prevTx.TxOut {
		if bytes.Equal(txOut.PkScript, pkScript) {
			prevOut = idx
		}
	}
	require.Greater(ht, prevOut, -1)

	// Okay, we have everything we need to create a PSBT now.
	pendingTx := &wire.MsgTx{
		Version: 2,
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: wire.OutPoint{
				Hash:  prevTx.TxHash(),
				Index: uint32(prevOut),
			},
		}},
		// We send to the same address again, but deduct some fees.
		TxOut: []*wire.TxOut{{
			Value:    utxo.Value - 600,
			PkScript: utxo.PkScript,
		}},
	}
	packet, err := psbt.NewFromUnsignedTx(pendingTx)
	require.NoError(ht, err)

	// We first try to sign the psbt without the necessary input data
	// which should fail with the expected error.
	var buf bytes.Buffer
	err = packet.Serialize(&buf)
	require.NoError(ht, err)

	signReq := &walletrpc.SignPsbtRequest{FundedPsbt: buf.Bytes()}
	err = alice.RPC.SignPsbtErr(signReq)
	require.ErrorContains(ht, err, "input (index=0) doesn't specify "+
		"any UTXO info", "error does not match")

	// Now let's add the meta information that we need for signing.
	packet.Inputs[0].WitnessUtxo = utxo
	packet.Inputs[0].NonWitnessUtxo = prevTx
	decorateUnsigned(packet)

	// That's it, we should be able to sign the PSBT now.
	signedPacket := signPacket(ht, alice, packet)

	// Allow the caller to also verify (and potentially move) some of the
	// returned fields.
	verifySigned(signedPacket)

	// We should be able to finalize the PSBT and extract the final TX now.
	err = psbt.MaybeFinalizeAll(signedPacket)
	require.NoError(ht, err)

	finalTx, err := psbt.Extract(signedPacket)
	require.NoError(ht, err)

	// Make sure we can also sign a second time. This makes sure any key
	// tweaking that happened for the signing didn't affect any keys in the
	// cache.
	signedPacket2 := signPacket(ht, alice, packet)
	verifySigned(signedPacket2)

	buf.Reset()
	err = finalTx.Serialize(&buf)
	require.NoError(ht, err)

	// Publish the second transaction and then mine both of them.
	txReq := &walletrpc.Transaction{TxHex: buf.Bytes()}
	alice.RPC.PublishTransaction(txReq)

	// Mine one block which should contain two transactions.
	block := ht.MineBlocksAndAssertNumTxes(1, 2)[0]
	firstTxHash := prevTx.TxHash()
	secondTxHash := finalTx.TxHash()
	ht.AssertTxInBlock(block, firstTxHash)
	ht.AssertTxInBlock(block, secondTxHash)
}

// assertPsbtFundSignSpend funds a PSBT from the internal wallet and then
// attempts to sign it by using the SignPsbt or FinalizePsbt method.
func assertPsbtFundSignSpend(ht *lntest.HarnessTest, alice *node.HarnessNode,
	fundOutputs map[string]uint64, changeType walletrpc.ChangeAddressType,
	useFinalize bool) {

	fundResp := alice.RPC.FundPsbt(&walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_Raw{
			Raw: &walletrpc.TxTemplate{
				Outputs: fundOutputs,
			},
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: 2,
		},
		MinConfs:   1,
		ChangeType: changeType,
	},
	)
	require.GreaterOrEqual(ht, fundResp.ChangeOutputIndex, int32(-1))

	// Make sure our change output has all the meta information required for
	// signing.
	fundedPacket, err := psbt.NewFromRawBytes(
		bytes.NewReader(fundResp.FundedPsbt), false,
	)
	require.NoError(ht, err)

	pOut := fundedPacket.Outputs[fundResp.ChangeOutputIndex]
	require.NotEmpty(ht, pOut.Bip32Derivation)
	derivation := pOut.Bip32Derivation[0]
	_, err = btcec.ParsePubKey(derivation.PubKey)
	require.NoError(ht, err)
	require.Len(ht, derivation.Bip32Path, 5)

	// Ensure we get the change output properly decorated with all the new
	// Taproot related fields, if it is a Taproot output.
	if changeType == walletrpc.ChangeAddressType_CHANGE_ADDRESS_TYPE_P2TR {
		require.NotEmpty(ht, pOut.TaprootBip32Derivation)
		require.NotEmpty(ht, pOut.TaprootInternalKey)

		trDerivation := pOut.TaprootBip32Derivation[0]
		require.Equal(
			ht, trDerivation.XOnlyPubKey, pOut.TaprootInternalKey,
		)
		_, err := schnorr.ParsePubKey(pOut.TaprootInternalKey)
		require.NoError(ht, err)
	}

	var signedPsbt []byte
	if useFinalize {
		finalizeResp := alice.RPC.FinalizePsbt(
			&walletrpc.FinalizePsbtRequest{
				FundedPsbt: fundResp.FundedPsbt,
			},
		)

		signedPsbt = finalizeResp.SignedPsbt
	} else {
		signResp := alice.RPC.SignPsbt(
			&walletrpc.SignPsbtRequest{
				FundedPsbt: fundResp.FundedPsbt,
			},
		)

		signedPsbt = signResp.SignedPsbt
	}

	// Let's make sure we have a partial signature.
	signedPacket, err := psbt.NewFromRawBytes(
		bytes.NewReader(signedPsbt), false,
	)
	require.NoError(ht, err)

	// We should be able to finalize the PSBT, extract and publish the final
	// TX now.
	finalTx := extractPublishAndMine(ht, alice, signedPacket)

	// Check type of the change script depending on the change address
	// type we provided in FundPsbt.
	changeScript := finalTx.TxOut[fundResp.ChangeOutputIndex].PkScript
	assertChangeScriptType(ht, changeScript, changeType)
}

// assertChangeScriptType checks if the given script has the right type given
// the change address type we used in FundPsbt. By default, the script should
// be a P2WPKH one.
func assertChangeScriptType(ht *lntest.HarnessTest, script []byte,
	fundChangeType walletrpc.ChangeAddressType) {

	switch fundChangeType {
	case walletrpc.ChangeAddressType_CHANGE_ADDRESS_TYPE_P2TR:
		require.True(ht, txscript.IsPayToTaproot(script))

	default:
		require.True(ht, txscript.IsPayToWitnessPubKeyHash(script))
	}
}

// deriveInternalKey derives a signing key and returns its descriptor, full
// derivation path and parsed public key.
func deriveInternalKey(ht *lntest.HarnessTest,
	alice *node.HarnessNode) (*signrpc.KeyDescriptor, *btcec.PublicKey,
	[]uint32) {

	// For the next step, we need a public key. Let's use a special family
	// for this.
	req := &walletrpc.KeyReq{KeyFamily: testTaprootKeyFamily}
	keyDesc := alice.RPC.DeriveNextKey(req)

	// The DeriveNextKey returns a key from the internal 1017 scope.
	fullDerivationPath := []uint32{
		hdkeychain.HardenedKeyStart + keychain.BIP0043Purpose,
		hdkeychain.HardenedKeyStart + harnessNetParams.HDCoinType,
		hdkeychain.HardenedKeyStart + uint32(keyDesc.KeyLoc.KeyFamily),
		0,
		uint32(keyDesc.KeyLoc.KeyIndex),
	}

	parsedPubKey, err := btcec.ParsePubKey(keyDesc.RawKeyBytes)
	require.NoError(ht, err)

	return keyDesc, parsedPubKey, fullDerivationPath
}

// sendAllCoinsToAddrType sweeps all coins from the wallet and sends them to a
// new address of the given type.
func sendAllCoinsToAddrType(ht *lntest.HarnessTest,
	hn *node.HarnessNode, addrType lnrpc.AddressType) {

	resp := hn.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: addrType,
	})

	hn.RPC.SendCoins(&lnrpc.SendCoinsRequest{
		Addr:             resp.Address,
		SendAll:          true,
		SpendUnconfirmed: true,
		TargetConf:       6,
	})

	ht.MineBlocksAndAssertNumTxes(1, 1)
}

// testPsbtChanFundingFailFlow tests the failing of a funding flow by the
// remote peer and that the initiator receives the expected error and aborts
// the channel opening. The psbt funding flow is used to simulate this behavior
// because we can easily let the remote peer run into the timeout.
func testPsbtChanFundingFailFlow(ht *lntest.HarnessTest) {
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)

	const chanSize = funding.MaxBtcFundingAmount

	// Decrease the timeout window for the remote peer to accelerate the
	// funding fail process.
	args := []string{
		"--dev.reservationtimeout=1s",
		"--dev.zombiesweeperinterval=1s",
	}
	ht.RestartNodeWithExtraArgs(bob, args)

	// Before we start the test, we'll ensure both sides are connected so
	// the funding flow can be properly executed.
	ht.EnsureConnected(alice, bob)

	// At this point, we can begin our PSBT channel funding workflow. We'll
	// start by generating a pending channel ID externally that will be used
	// to track this new funding type.
	pendingChanID := ht.Random32Bytes()

	// Now that we have the pending channel ID, Alice will open the channel
	// by specifying a PSBT shim.
	chanUpdates, _ := ht.OpenChannelPsbt(
		alice, bob, lntest.OpenChannelParams{
			Amt: chanSize,
			FundingShim: &lnrpc.FundingShim{
				Shim: &lnrpc.FundingShim_PsbtShim{
					PsbtShim: &lnrpc.PsbtShim{
						PendingChanId: pendingChanID,
					},
				},
			},
		},
	)

	// We received the AcceptChannel msg from our peer but we are not going
	// to fund this channel but instead wait for our peer to fail the
	// funding workflow with an internal error.
	ht.ReceiveOpenChannelError(chanUpdates, chanfunding.ErrRemoteCanceled)
}

// testPsbtChanFundingWithUnstableUtxos tests that channel openings with
// unstable utxos, in this case in particular unconfirmed utxos still in use by
// the sweeper subsystem, are not considered when opening a channel. They bear
// the risk of being RBFed and are therefore not safe to open a channel with.
func testPsbtChanFundingWithUnstableUtxos(ht *lntest.HarnessTest) {
	fundingAmt := btcutil.Amount(2_000_000)

	// First, we'll create two new nodes that we'll use to open channel
	// between for this test.
	carol := ht.NewNode("carol", nil)
	dave := ht.NewNode("dave", nil)
	ht.EnsureConnected(carol, dave)

	// Fund Carol's wallet with a confirmed utxo.
	ht.FundCoins(fundingAmt, carol)

	ht.AssertNumUTXOs(carol, 1)

	// Now spend the coins to create an unconfirmed transaction. This is
	// necessary to test also the neutrino behaviour. For neutrino nodes
	// only unconfirmed transactions originating from this node will be
	// recognized as unconfirmed.
	req := &lnrpc.NewAddressRequest{Type: AddrTypeTaprootPubkey}
	resp := carol.RPC.NewAddress(req)

	sendCoinsResp := carol.RPC.SendCoins(&lnrpc.SendCoinsRequest{
		Addr:        resp.Address,
		SendAll:     true,
		SatPerVbyte: 1,
	})

	walletUtxo := ht.AssertNumUTXOsUnconfirmed(carol, 1)[0]
	require.EqualValues(ht, sendCoinsResp.Txid, walletUtxo.Outpoint.TxidStr)

	chanSize := btcutil.Amount(walletUtxo.AmountSat / 2)

	// We use STATIC_REMOTE_KEY channels to easily generate sweeps without
	// anchor sweeps interfering.
	cType := lnrpc.CommitmentType_STATIC_REMOTE_KEY

	// We open a normal channel so that we can force-close it and produce
	// a sweeper originating utxo.
	update := ht.OpenChannelAssertPending(carol, dave,
		lntest.OpenChannelParams{
			Amt:              chanSize,
			SpendUnconfirmed: true,
		})
	channelPoint := lntest.ChanPointFromPendingUpdate(update)
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// Now force close the channel by dave to generate a utxo which is
	// swept by the sweeper. We have STATIC_REMOTE_KEY Channel Types.
	ht.CloseChannelAssertPending(dave, channelPoint, true)
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Make sure Carol sees her to_remote output from the force close tx.
	ht.AssertNumPendingSweeps(carol, 1)

	// We wait for the to_remote sweep tx.
	ht.AssertNumUTXOsUnconfirmed(carol, 1)

	// We need the maximum funding amount to ensure we are opening the next
	// channel with all available utxos.
	carolBalance := carol.RPC.WalletBalance()

	// The max chan size needs to account for the fee opening the channel
	// itself.
	// NOTE: We need to always account for a change here, because their is
	// an inaccurarcy in the backend code.
	chanSize = btcutil.Amount(carolBalance.TotalBalance) -
		fundingFee(2, true)

	// Now open a channel of this amount via a psbt workflow.
	// At this point, we can begin our PSBT channel funding workflow. We'll
	// start by generating a pending channel ID externally that will be used
	// to track this new funding type.
	pendingChanID := ht.Random32Bytes()

	// Now that we have the pending channel ID, Carol will open the channel
	// by specifying a PSBT shim. We expect it to fail because we try to
	// fund a channel with the maximum amount of our wallet, which also
	// includes an unstable utxo originating from the sweeper.
	chanUpdates, tempPsbt := ht.OpenChannelPsbt(
		carol, dave, lntest.OpenChannelParams{
			Amt: chanSize,
			FundingShim: &lnrpc.FundingShim{
				Shim: &lnrpc.FundingShim_PsbtShim{
					PsbtShim: &lnrpc.PsbtShim{
						PendingChanId: pendingChanID,
					},
				},
			},
			CommitmentType:   cType,
			SpendUnconfirmed: true,
		},
	)

	fundReq := &walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_Psbt{
			Psbt: tempPsbt,
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: 50,
		},
		MinConfs:         0,
		SpendUnconfirmed: true,
	}
	carol.RPC.FundPsbtAssertErr(fundReq)

	// We confirm the sweep transaction and make sure we see it as confirmed
	// from the perspective of the underlying wallet.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// We expect 2 confirmed utxos, the change of the prior successful
	// channel opening and the confirmed to_remote output.
	ht.AssertNumUTXOsConfirmed(carol, 2)

	// We fund the psbt request again and now all utxo are stable and can
	// finally be used to fund the channel.
	fundResp := carol.RPC.FundPsbt(fundReq)

	// We verify the psbt before finalizing it.
	carol.RPC.FundingStateStep(&lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: pendingChanID,
				FundedPsbt:    fundResp.FundedPsbt,
			},
		},
	})

	// Now we'll ask Carol's wallet to sign the PSBT so we can finish the
	// funding flow.
	finalizeReq := &walletrpc.FinalizePsbtRequest{
		FundedPsbt: fundResp.FundedPsbt,
	}
	finalizeRes := carol.RPC.FinalizePsbt(finalizeReq)

	// We've signed our PSBT now, let's pass it to the intent again.
	carol.RPC.FundingStateStep(&lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtFinalize{
			PsbtFinalize: &lnrpc.FundingPsbtFinalize{
				PendingChanId: pendingChanID,
				SignedPsbt:    finalizeRes.SignedPsbt,
			},
		},
	})

	// Consume the "channel pending" update. This waits until the funding
	// transaction was fully compiled.
	updateResp := ht.ReceiveOpenChannelUpdate(chanUpdates)
	upd, ok := updateResp.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.True(ht, ok)
	channelPoint2 := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: upd.ChanPending.Txid,
		},
		OutputIndex: upd.ChanPending.OutputIndex,
	}

	var finalTx wire.MsgTx
	err := finalTx.Deserialize(bytes.NewReader(finalizeRes.RawFinalTx))
	require.NoError(ht, err)

	txHash := finalTx.TxHash()
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.AssertTxInBlock(block, txHash)

	// Now we do the same but instead use preselected utxos to verify that
	// these utxos respects the utxo restrictions on sweeper unconfirmed
	// inputs as well.

	// Now force close the channel by dave to generate a utxo which is
	// swept by the sweeper. We have STATIC_REMOTE_KEY Channel Types.
	ht.CloseChannelAssertPending(dave, channelPoint2, true)
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Make sure Carol sees her to_remote output from the force close tx.
	ht.AssertNumPendingSweeps(carol, 1)

	// We wait for the to_remote sweep tx of channelPoint2.
	utxos := ht.AssertNumUTXOsUnconfirmed(carol, 1)

	// We need the maximum funding amount to ensure we are opening the next
	// channel with all available utxos.
	carolBalance = carol.RPC.WalletBalance()

	// The max chan size needs to account for the fee opening the channel
	// itself.
	// NOTE: We need to always account for a change here, because their is
	// an inaccurarcy in the backend code calculating the fee of a 1 input
	// one output transaction, it always account for a channge in that case
	// as well.
	chanSize = btcutil.Amount(carolBalance.TotalBalance) -
		fundingFee(2, true)

	// Now open a channel of this amount via a psbt workflow.
	// At this point, we can begin our PSBT channel funding workflow. We'll
	// start by generating a pending channel ID externally that will be used
	// to track this new funding type.
	pendingChanID = ht.Random32Bytes()

	// Now that we have the pending channel ID, Carol will open the channel
	// by specifying a PSBT shim. We expect it to fail because we try to
	// fund a channel with the maximum amount of our wallet, which also
	// includes an unstable utxo originating from the sweeper.
	chanUpdates, tempPsbt = ht.OpenChannelPsbt(
		carol, dave, lntest.OpenChannelParams{
			Amt: chanSize,
			FundingShim: &lnrpc.FundingShim{
				Shim: &lnrpc.FundingShim_PsbtShim{
					PsbtShim: &lnrpc.PsbtShim{
						PendingChanId: pendingChanID,
					},
				},
			},
			CommitmentType:   cType,
			SpendUnconfirmed: true,
		},
	)
	// Add selected utxos to the funding intent.
	decodedPsbt, err := psbt.NewFromRawBytes(
		bytes.NewReader(tempPsbt), false,
	)
	require.NoError(ht, err)

	for _, input := range utxos {
		txHash, err := chainhash.NewHashFromStr(input.Outpoint.TxidStr)
		require.NoError(ht, err)
		decodedPsbt.UnsignedTx.TxIn = append(
			decodedPsbt.UnsignedTx.TxIn, &wire.TxIn{
				PreviousOutPoint: wire.OutPoint{
					Hash:  *txHash,
					Index: input.Outpoint.OutputIndex,
				},
			})

		// The inputs we are using to fund the transaction are known to
		// the internal wallet that's why we just append an empty input
		// element so that the parsing of the psbt package succeeds.
		decodedPsbt.Inputs = append(decodedPsbt.Inputs, psbt.PInput{})
	}

	var psbtBytes bytes.Buffer
	err = decodedPsbt.Serialize(&psbtBytes)
	require.NoError(ht, err)

	fundReq = &walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_Psbt{
			Psbt: psbtBytes.Bytes(),
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: 50,
		},
		MinConfs:         0,
		SpendUnconfirmed: true,
	}
	carol.RPC.FundPsbtAssertErr(fundReq)

	ht.MineBlocksAndAssertNumTxes(1, 1)

	// We expect 2 confirmed utxos, the change of the last successful
	// channel opening and the confirmed to_remote output of channelPoint2.
	ht.AssertNumUTXOsConfirmed(carol, 2)

	// After the confirmation of the sweep to_remote output the funding
	// will now proceed.
	fundResp = carol.RPC.FundPsbt(fundReq)

	// We verify the funded psbt.
	carol.RPC.FundingStateStep(&lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: pendingChanID,
				FundedPsbt:    fundResp.FundedPsbt,
			},
		},
	})

	// Now we'll ask Carol's wallet to sign the PSBT so we can finish the
	// funding flow.
	finalizeReq = &walletrpc.FinalizePsbtRequest{
		FundedPsbt: fundResp.FundedPsbt,
	}
	finalizeRes = carol.RPC.FinalizePsbt(finalizeReq)

	// We've signed our PSBT now, let's pass it to the intent again.
	carol.RPC.FundingStateStep(&lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtFinalize{
			PsbtFinalize: &lnrpc.FundingPsbtFinalize{
				PendingChanId: pendingChanID,
				SignedPsbt:    finalizeRes.SignedPsbt,
			},
		},
	})

	// Consume the "channel pending" update. This waits until the funding
	// transaction was fully compiled.
	updateResp = ht.ReceiveOpenChannelUpdate(chanUpdates)
	upd, ok = updateResp.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.True(ht, ok)

	err = finalTx.Deserialize(bytes.NewReader(finalizeRes.RawFinalTx))
	require.NoError(ht, err)

	txHash = finalTx.TxHash()
	block = ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.AssertTxInBlock(block, txHash)
}

// testFundPsbtCustomLock verifies that FundPsbt correctly locks inputs
// using a custom lock ID and expiration time.
func testFundPsbtCustomLock(ht *lntest.HarnessTest) {
	alice := ht.NewNodeWithCoins("Alice", nil)

	// Define a custom lock ID and a short expiration for testing.
	customLockID := ht.Random32Bytes()
	lockDurationSeconds := uint64(30)

	ht.Logf("Using custom lock ID: %x with expiration: %d seconds",
		customLockID, lockDurationSeconds)

	// Generate an address for the output.
	aliceAddr := alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	})
	outputs := map[string]uint64{
		aliceAddr.Address: 100_000,
	}

	// Build the FundPsbt request using custom lock parameters.
	req := &walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_Raw{
			Raw: &walletrpc.TxTemplate{Outputs: outputs},
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: 2,
		},
		MinConfs:              1,
		CustomLockId:          customLockID,
		LockExpirationSeconds: lockDurationSeconds,
	}

	// Capture the current time for later expiration validation.
	callTime := time.Now()

	// Execute the FundPsbt call and validate the response.
	fundResp := alice.RPC.FundPsbt(req)
	require.NotEmpty(ht, fundResp.FundedPsbt)

	// Ensure the response includes at least one locked UTXO.
	require.GreaterOrEqual(ht, len(fundResp.LockedUtxos), 1)

	// Parse the PSBT and map locked outpoints for quick lookup.
	fundedPacket, err := psbt.NewFromRawBytes(
		bytes.NewReader(fundResp.FundedPsbt), false,
	)
	require.NoError(ht, err)

	lockedOutpointsMap := make(map[string]struct{})
	for _, utxo := range fundResp.LockedUtxos {
		lockedOutpointsMap[lntest.LnrpcOutpointToStr(utxo.Outpoint)] =
			struct{}{}
	}

	// Check that all PSBT inputs are among the locked UTXOs.
	require.Len(ht, fundedPacket.UnsignedTx.TxIn, len(lockedOutpointsMap))
	for _, txIn := range fundedPacket.UnsignedTx.TxIn {
		_, ok := lockedOutpointsMap[txIn.PreviousOutPoint.String()]
		require.True(
			ht, ok, "Missing locked input: %v",
			txIn.PreviousOutPoint,
		)
	}

	// Verify leases via ListLeases call.
	ht.Logf("Verifying leases via ListLeases...")
	leasesResp := alice.RPC.ListLeases()
	require.NoError(ht, err)
	require.Len(ht, leasesResp.LockedUtxos, len(lockedOutpointsMap))

	for _, lease := range leasesResp.LockedUtxos {
		// Validate that the lease matches our locked UTXOs.
		require.Contains(
			ht, lockedOutpointsMap,
			lntest.LnrpcOutpointToStr(lease.Outpoint),
		)

		// Confirm lock ID and expiration.
		require.EqualValues(ht, customLockID, lease.Id)

		expectedExpiration := callTime.Unix() +
			int64(lockDurationSeconds)

		// Validate that the expiration time is within a small delta (5
		// seconds) of the expected value. This accounts for any latency
		// in the RPC call or processing time (to avoid flakes in CI).
		const leaseExpirationDelta = 5.0
		require.InDelta(
			ht, expectedExpiration, lease.Expiration,
			leaseExpirationDelta,
		)
	}

	// We use this extra wait time to ensure the lock is released after the
	// expiration time.
	const extraWaitSeconds = 2

	// Wait for the lock to expire, then confirm it's released.
	waitDuration := time.Duration(
		lockDurationSeconds+extraWaitSeconds,
	) * time.Second
	ht.Logf("Waiting %v for lock to expire...", waitDuration)
	time.Sleep(waitDuration)

	ht.Logf("Verifying lease expiration...")
	leasesRespAfter := alice.RPC.ListLeases()
	require.Empty(ht, leasesRespAfter.LockedUtxos)
}
