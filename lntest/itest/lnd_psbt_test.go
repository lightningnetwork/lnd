package itest

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/hdkeychain"
	"github.com/btcsuite/btcutil/psbt"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testPsbtChanFunding makes sure a channel can be opened between carol and dave
// by using a Partially Signed Bitcoin Transaction that funds the channel
// multisig funding output.
func testPsbtChanFunding(net *lntest.NetworkHarness, t *harnessTest) {
	// First, we'll create two new nodes that we'll use to open channels
	// between for this test. Dave gets some coins that will be used to
	// fund the PSBT, just to make sure that Carol has an empty wallet.
	carol := net.NewNode(t.t, "carol", nil)
	defer shutdownAndAssert(net, t, carol)

	dave := net.NewNode(t.t, "dave", nil)
	defer shutdownAndAssert(net, t, dave)

	runPsbtChanFunding(net, t, carol, dave)
}

// runPsbtChanFunding makes sure a channel can be opened between carol and dave
// by using a Partially Signed Bitcoin Transaction that funds the channel
// multisig funding output.
func runPsbtChanFunding(net *lntest.NetworkHarness, t *harnessTest, carol,
	dave *lntest.HarnessNode) {

	ctxb := context.Background()

	// Everything we do here should be done within a second or two, so we
	// can just keep a single timeout context around for all calls.
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	const chanSize = funding.MaxBtcFundingAmount
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, dave)

	// Before we start the test, we'll ensure both sides are connected so
	// the funding flow can be properly executed.
	net.EnsureConnected(t.t, carol, dave)
	net.EnsureConnected(t.t, carol, net.Alice)

	// At this point, we can begin our PSBT channel funding workflow. We'll
	// start by generating a pending channel ID externally that will be used
	// to track this new funding type.
	var pendingChanID [32]byte
	_, err := rand.Read(pendingChanID[:])
	require.NoError(t.t, err)

	// We'll also test batch funding of two channels so we need another ID.
	var pendingChanID2 [32]byte
	_, err = rand.Read(pendingChanID2[:])
	require.NoError(t.t, err)

	// Now that we have the pending channel ID, Carol will open the channel
	// by specifying a PSBT shim. We use the NoPublish flag here to avoid
	// publishing the whole batch TX too early.
	chanUpdates, tempPsbt, err := openChannelPsbt(
		ctxt, carol, dave, lntest.OpenChannelParams{
			Amt: chanSize,
			FundingShim: &lnrpc.FundingShim{
				Shim: &lnrpc.FundingShim_PsbtShim{
					PsbtShim: &lnrpc.PsbtShim{
						PendingChanId: pendingChanID[:],
						NoPublish:     true,
					},
				},
			},
		},
	)
	require.NoError(t.t, err)

	// Let's add a second channel to the batch. This time between Carol and
	// Alice. We will publish the batch TX once this channel funding is
	// complete.
	chanUpdates2, psbtBytes2, err := openChannelPsbt(
		ctxt, carol, net.Alice, lntest.OpenChannelParams{
			Amt: chanSize,
			FundingShim: &lnrpc.FundingShim{
				Shim: &lnrpc.FundingShim_PsbtShim{
					PsbtShim: &lnrpc.PsbtShim{
						PendingChanId: pendingChanID2[:],
						NoPublish:     false,
						BasePsbt:      tempPsbt,
					},
				},
			},
		},
	)
	require.NoError(t.t, err)

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
	fundResp, err := dave.WalletKitClient.FundPsbt(ctxt, fundReq)
	require.NoError(t.t, err)

	// We have a PSBT that has no witness data yet, which is exactly what we
	// need for the next step: Verify the PSBT with the funding intents.
	_, err = carol.FundingStateStep(ctxb, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: pendingChanID[:],
				FundedPsbt:    fundResp.FundedPsbt,
			},
		},
	})
	require.NoError(t.t, err)
	_, err = carol.FundingStateStep(ctxb, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: pendingChanID2[:],
				FundedPsbt:    fundResp.FundedPsbt,
			},
		},
	})
	require.NoError(t.t, err)

	// Now we'll ask Dave's wallet to sign the PSBT so we can finish the
	// funding flow.
	finalizeReq := &walletrpc.FinalizePsbtRequest{
		FundedPsbt: fundResp.FundedPsbt,
	}
	finalizeRes, err := dave.WalletKitClient.FinalizePsbt(ctxt, finalizeReq)
	require.NoError(t.t, err)

	// We've signed our PSBT now, let's pass it to the intent again.
	_, err = carol.FundingStateStep(ctxb, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtFinalize{
			PsbtFinalize: &lnrpc.FundingPsbtFinalize{
				PendingChanId: pendingChanID[:],
				SignedPsbt:    finalizeRes.SignedPsbt,
			},
		},
	})
	require.NoError(t.t, err)

	// Consume the "channel pending" update. This waits until the funding
	// transaction was fully compiled.
	updateResp, err := receiveChanUpdate(ctxt, chanUpdates)
	require.NoError(t.t, err)
	upd, ok := updateResp.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.True(t.t, ok)
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: upd.ChanPending.Txid,
		},
		OutputIndex: upd.ChanPending.OutputIndex,
	}

	// No transaction should have been published yet.
	mempool, err := net.Miner.Client.GetRawMempool()
	require.NoError(t.t, err)
	require.Equal(t.t, 0, len(mempool))

	// Let's progress the second channel now. This time we'll use the raw
	// wire format transaction directly.
	require.NoError(t.t, err)
	_, err = carol.FundingStateStep(ctxb, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtFinalize{
			PsbtFinalize: &lnrpc.FundingPsbtFinalize{
				PendingChanId: pendingChanID2[:],
				FinalRawTx:    finalizeRes.RawFinalTx,
			},
		},
	})
	require.NoError(t.t, err)

	// Consume the "channel pending" update for the second channel. This
	// waits until the funding transaction was fully compiled and in this
	// case published.
	updateResp2, err := receiveChanUpdate(ctxt, chanUpdates2)
	require.NoError(t.t, err)
	upd2, ok := updateResp2.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.True(t.t, ok)
	chanPoint2 := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: upd2.ChanPending.Txid,
		},
		OutputIndex: upd2.ChanPending.OutputIndex,
	}

	// Great, now we can mine a block to get the transaction confirmed, then
	// wait for the new channel to be propagated through the network.
	var finalTx wire.MsgTx
	err = finalTx.Deserialize(bytes.NewReader(finalizeRes.RawFinalTx))
	require.NoError(t.t, err)

	txHash := finalTx.TxHash()
	block := mineBlocks(t, net, 6, 1)[0]
	assertTxInBlock(t, block, &txHash)
	err = carol.WaitForNetworkChannelOpen(chanPoint)
	require.NoError(t.t, err)
	err = carol.WaitForNetworkChannelOpen(chanPoint2)
	require.NoError(t.t, err)

	// With the channel open, ensure that it is counted towards Carol's
	// total channel balance.
	balReq := &lnrpc.ChannelBalanceRequest{}
	balRes, err := carol.ChannelBalance(ctxt, balReq)
	require.NoError(t.t, err)
	require.NotEqual(t.t, int64(0), balRes.LocalBalance.Sat)

	// Next, to make sure the channel functions as normal, we'll make some
	// payments within the channel.
	payAmt := btcutil.Amount(100000)
	invoice := &lnrpc.Invoice{
		Memo:  "new chans",
		Value: int64(payAmt),
	}
	resp, err := dave.AddInvoice(ctxt, invoice)
	require.NoError(t.t, err)
	err = completePaymentRequests(
		carol, carol.RouterClient, []string{resp.PaymentRequest}, true,
	)
	require.NoError(t.t, err)

	// To conclude, we'll close the newly created channel between Carol and
	// Dave. This function will also block until the channel is closed and
	// will additionally assert the relevant channel closing post
	// conditions.
	closeChannelAndAssert(t, net, carol, chanPoint, false)
}

// testPsbtChanFundingExternal makes sure a channel can be opened between carol
// and dave by using a Partially Signed Bitcoin Transaction that funds the
// channel multisig funding output and is fully funded by an external third
// party.
func testPsbtChanFundingExternal(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	const chanSize = funding.MaxBtcFundingAmount

	// Everything we do here should be done within a second or two, so we
	// can just keep a single timeout context around for all calls.
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// First, we'll create two new nodes that we'll use to open channels
	// between for this test. Both these nodes have an empty wallet as Alice
	// will be funding the channel.
	carol := net.NewNode(t.t, "carol", nil)
	defer shutdownAndAssert(net, t, carol)

	dave := net.NewNode(t.t, "dave", nil)
	defer shutdownAndAssert(net, t, dave)

	// Before we start the test, we'll ensure both sides are connected so
	// the funding flow can be properly executed.
	net.EnsureConnected(t.t, carol, dave)
	net.EnsureConnected(t.t, carol, net.Alice)

	// At this point, we can begin our PSBT channel funding workflow. We'll
	// start by generating a pending channel ID externally that will be used
	// to track this new funding type.
	var pendingChanID [32]byte
	_, err := rand.Read(pendingChanID[:])
	require.NoError(t.t, err)

	// We'll also test batch funding of two channels so we need another ID.
	var pendingChanID2 [32]byte
	_, err = rand.Read(pendingChanID2[:])
	require.NoError(t.t, err)

	// Now that we have the pending channel ID, Carol will open the channel
	// by specifying a PSBT shim. We use the NoPublish flag here to avoid
	// publishing the whole batch TX too early.
	chanUpdates, tempPsbt, err := openChannelPsbt(
		ctxt, carol, dave, lntest.OpenChannelParams{
			Amt: chanSize,
			FundingShim: &lnrpc.FundingShim{
				Shim: &lnrpc.FundingShim_PsbtShim{
					PsbtShim: &lnrpc.PsbtShim{
						PendingChanId: pendingChanID[:],
						NoPublish:     true,
					},
				},
			},
		},
	)
	require.NoError(t.t, err)

	// Let's add a second channel to the batch. This time between Carol and
	// Alice. We will publish the batch TX once this channel funding is
	// complete.
	chanUpdates2, psbtBytes2, err := openChannelPsbt(
		ctxt, carol, net.Alice, lntest.OpenChannelParams{
			Amt: chanSize,
			FundingShim: &lnrpc.FundingShim{
				Shim: &lnrpc.FundingShim_PsbtShim{
					PsbtShim: &lnrpc.PsbtShim{
						PendingChanId: pendingChanID2[:],
						NoPublish:     true,
						BasePsbt:      tempPsbt,
					},
				},
			},
		},
	)
	require.NoError(t.t, err)

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
	fundResp, err := net.Alice.WalletKitClient.FundPsbt(ctxt, fundReq)
	require.NoError(t.t, err)

	// We have a PSBT that has no witness data yet, which is exactly what we
	// need for the next step: Verify the PSBT with the funding intents.
	// We tell the PSBT intent to skip the finalize step because we know the
	// final transaction will not be broadcast by Carol herself but by
	// Alice. And we assume that Alice is a third party that is not in
	// direct communication with Carol and won't send the signed TX to her
	// before broadcasting it. So we cannot call the finalize step but
	// instead just tell lnd to wait for a TX to be published/confirmed.
	_, err = carol.FundingStateStep(ctxb, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: pendingChanID[:],
				FundedPsbt:    fundResp.FundedPsbt,
				SkipFinalize:  true,
			},
		},
	})
	require.NoError(t.t, err)
	_, err = carol.FundingStateStep(ctxb, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: pendingChanID2[:],
				FundedPsbt:    fundResp.FundedPsbt,
				SkipFinalize:  true,
			},
		},
	})
	require.NoError(t.t, err)

	// Consume the "channel pending" update. This waits until the funding
	// transaction was fully compiled for both channels.
	updateResp, err := receiveChanUpdate(ctxt, chanUpdates)
	require.NoError(t.t, err)
	upd, ok := updateResp.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.True(t.t, ok)
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: upd.ChanPending.Txid,
		},
		OutputIndex: upd.ChanPending.OutputIndex,
	}
	updateResp2, err := receiveChanUpdate(ctxt, chanUpdates2)
	require.NoError(t.t, err)
	upd2, ok := updateResp2.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.True(t.t, ok)
	chanPoint2 := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: upd2.ChanPending.Txid,
		},
		OutputIndex: upd2.ChanPending.OutputIndex,
	}
	numPending, err := numOpenChannelsPending(ctxt, carol)
	require.NoError(t.t, err)
	require.Equal(t.t, 2, numPending)

	// Now we'll ask Alice's wallet to sign the PSBT so we can finish the
	// funding flow.
	finalizeReq := &walletrpc.FinalizePsbtRequest{
		FundedPsbt: fundResp.FundedPsbt,
	}
	finalizeRes, err := net.Alice.WalletKitClient.FinalizePsbt(
		ctxt, finalizeReq,
	)
	require.NoError(t.t, err)

	// No transaction should have been published yet.
	mempool, err := net.Miner.Client.GetRawMempool()
	require.NoError(t.t, err)
	require.Equal(t.t, 0, len(mempool))

	// Great, now let's publish the final raw transaction.
	var finalTx wire.MsgTx
	err = finalTx.Deserialize(bytes.NewReader(finalizeRes.RawFinalTx))
	require.NoError(t.t, err)

	txHash := finalTx.TxHash()
	_, err = net.Miner.Client.SendRawTransaction(&finalTx, false)
	require.NoError(t.t, err)

	// Now we can mine a block to get the transaction confirmed, then wait
	// for the new channel to be propagated through the network.
	block := mineBlocks(t, net, 6, 1)[0]
	assertTxInBlock(t, block, &txHash)
	err = carol.WaitForNetworkChannelOpen(chanPoint)
	require.NoError(t.t, err)
	err = carol.WaitForNetworkChannelOpen(chanPoint2)
	require.NoError(t.t, err)

	// With the channel open, ensure that it is counted towards Carol's
	// total channel balance.
	balReq := &lnrpc.ChannelBalanceRequest{}
	balRes, err := carol.ChannelBalance(ctxt, balReq)
	require.NoError(t.t, err)
	require.NotEqual(t.t, int64(0), balRes.LocalBalance.Sat)

	// Next, to make sure the channel functions as normal, we'll make some
	// payments within the channel.
	payAmt := btcutil.Amount(100000)
	invoice := &lnrpc.Invoice{
		Memo:  "new chans",
		Value: int64(payAmt),
	}
	resp, err := dave.AddInvoice(ctxt, invoice)
	require.NoError(t.t, err)
	err = completePaymentRequests(
		carol, carol.RouterClient, []string{resp.PaymentRequest}, true,
	)
	require.NoError(t.t, err)

	// To conclude, we'll close the newly created channel between Carol and
	// Dave. This function will also block until the channels are closed and
	// will additionally assert the relevant channel closing post
	// conditions.
	closeChannelAndAssert(t, net, carol, chanPoint, false)
	closeChannelAndAssert(t, net, carol, chanPoint2, false)
}

// testPsbtChanFundingSingleStep checks whether PSBT funding works also when the
// wallet of both nodes are empty and one of them uses PSBT and an external
// wallet to fund the channel while creating reserve output in the same
// transaction.
func testPsbtChanFundingSingleStep(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	const chanSize = funding.MaxBtcFundingAmount

	// Everything we do here should be done within a second or two, so we
	// can just keep a single timeout context around for all calls.
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	args := nodeArgsForCommitType(lnrpc.CommitmentType_ANCHORS)

	// First, we'll create two new nodes that we'll use to open channels
	// between for this test. But in this case both nodes have an empty
	// wallet.
	carol := net.NewNode(t.t, "carol", args)
	defer shutdownAndAssert(net, t, carol)

	dave := net.NewNode(t.t, "dave", args)
	defer shutdownAndAssert(net, t, dave)

	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, net.Alice)

	// Get new address for anchor reserve.
	reserveAddrReq := &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	}
	addrResp, err := carol.NewAddress(ctxb, reserveAddrReq)
	require.NoError(t.t, err)
	reserveAddr, err := btcutil.DecodeAddress(addrResp.Address, harnessNetParams)
	require.NoError(t.t, err)
	reserveAddrScript, err := txscript.PayToAddrScript(reserveAddr)
	require.NoError(t.t, err)

	// Before we start the test, we'll ensure both sides are connected so
	// the funding flow can be properly executed.
	net.EnsureConnected(t.t, carol, dave)

	// At this point, we can begin our PSBT channel funding workflow. We'll
	// start by generating a pending channel ID externally that will be used
	// to track this new funding type.
	var pendingChanID [32]byte
	_, err = rand.Read(pendingChanID[:])
	require.NoError(t.t, err)

	// Now that we have the pending channel ID, Carol will open the channel
	// by specifying a PSBT shim.
	chanUpdates, tempPsbt, err := openChannelPsbt(
		ctxt, carol, dave, lntest.OpenChannelParams{
			Amt: chanSize,
			FundingShim: &lnrpc.FundingShim{
				Shim: &lnrpc.FundingShim_PsbtShim{
					PsbtShim: &lnrpc.PsbtShim{
						PendingChanId: pendingChanID[:],
						NoPublish:     false,
					},
				},
			},
		},
	)
	require.NoError(t.t, err)

	decodedPsbt, err := psbt.NewFromRawBytes(bytes.NewReader(tempPsbt), false)
	require.NoError(t.t, err)

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
	require.NoError(t.t, err)

	fundReq := &walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_Psbt{
			Psbt: psbtBytes.Bytes(),
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: 2,
		},
	}
	fundResp, err := net.Alice.WalletKitClient.FundPsbt(ctxt, fundReq)
	require.NoError(t.t, err)

	// Make sure the wallets are actually empty
	unspentCarol, err := carol.ListUnspent(ctxb, &lnrpc.ListUnspentRequest{})
	require.NoError(t.t, err)
	require.Len(t.t, unspentCarol.Utxos, 0)

	unspentDave, err := dave.ListUnspent(ctxb, &lnrpc.ListUnspentRequest{})
	require.NoError(t.t, err)
	require.Len(t.t, unspentDave.Utxos, 0)

	// We have a PSBT that has no witness data yet, which is exactly what we
	// need for the next step: Verify the PSBT with the funding intents.
	_, err = carol.FundingStateStep(ctxb, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: pendingChanID[:],
				FundedPsbt:    fundResp.FundedPsbt,
			},
		},
	})
	require.NoError(t.t, err)

	// Now we'll ask Alice's wallet to sign the PSBT so we can finish the
	// funding flow.
	finalizeReq := &walletrpc.FinalizePsbtRequest{
		FundedPsbt: fundResp.FundedPsbt,
	}
	finalizeRes, err := net.Alice.WalletKitClient.FinalizePsbt(ctxt, finalizeReq)
	require.NoError(t.t, err)

	// We've signed our PSBT now, let's pass it to the intent again.
	_, err = carol.FundingStateStep(ctxb, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtFinalize{
			PsbtFinalize: &lnrpc.FundingPsbtFinalize{
				PendingChanId: pendingChanID[:],
				SignedPsbt:    finalizeRes.SignedPsbt,
			},
		},
	})
	require.NoError(t.t, err)

	// Consume the "channel pending" update. This waits until the funding
	// transaction was fully compiled.
	updateResp, err := receiveChanUpdate(ctxt, chanUpdates)
	require.NoError(t.t, err)
	upd, ok := updateResp.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.True(t.t, ok)
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: upd.ChanPending.Txid,
		},
		OutputIndex: upd.ChanPending.OutputIndex,
	}

	var finalTx wire.MsgTx
	err = finalTx.Deserialize(bytes.NewReader(finalizeRes.RawFinalTx))
	require.NoError(t.t, err)

	txHash := finalTx.TxHash()
	block := mineBlocks(t, net, 6, 1)[0]
	assertTxInBlock(t, block, &txHash)
	err = carol.WaitForNetworkChannelOpen(chanPoint)
	require.NoError(t.t, err)

	// Next, to make sure the channel functions as normal, we'll make some
	// payments within the channel.
	payAmt := btcutil.Amount(100000)
	invoice := &lnrpc.Invoice{
		Memo:  "new chans",
		Value: int64(payAmt),
	}
	resp, err := dave.AddInvoice(ctxt, invoice)
	require.NoError(t.t, err)
	err = completePaymentRequests(
		carol, carol.RouterClient, []string{resp.PaymentRequest},
		true,
	)
	require.NoError(t.t, err)

	// To conclude, we'll close the newly created channel between Carol and
	// Dave. This function will also block until the channel is closed and
	// will additionally assert the relevant channel closing post
	// conditions.
	closeChannelAndAssert(t, net, carol, chanPoint, false)
}

// runSignPsbt tests that the SignPsbt RPC works correctly.
func testSignPsbt(net *lntest.NetworkHarness, t *harnessTest) {
	runSignPsbt(t, net, net.Alice)
}

// runSignPsbt tests that the SignPsbt RPC works correctly.
func runSignPsbt(t *harnessTest, net *lntest.NetworkHarness,
	alice *lntest.HarnessNode) {

	ctxb := context.Background()

	// Everything we do here should be done within a second or two, so we
	// can just keep a single timeout context around for all calls.
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// We test that we can sign a PSBT that spends funds from an input that
	// the wallet doesn't know about. To set up that test case, we first
	// derive an address manually that the wallet won't be watching on
	// chain. We can do that by exporting the account xpub of lnd's main
	// account.
	accounts, err := alice.WalletKitClient.ListAccounts(
		ctxt, &walletrpc.ListAccountsRequest{},
	)
	require.NoError(t.t, err)
	require.NotEmpty(t.t, accounts.Accounts)

	// We also need to parse the accounts, so we have easy access to the
	// parsed derivation paths.
	parsedAccounts, err := walletrpc.AccountsToWatchOnly(accounts.Accounts)
	require.NoError(t.t, err)

	account := parsedAccounts[0]
	xpub, err := hdkeychain.NewKeyFromString(account.Xpub)
	require.NoError(t.t, err)

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
	change, err := xpub.DeriveNonStandard(changeIndex) // nolint:staticcheck
	require.NoError(t.t, err)

	// At an index that we are certainly not watching in the wallet.
	addrKey, err := change.DeriveNonStandard(addrIndex) // nolint:staticcheck
	require.NoError(t.t, err)

	addrPubKey, err := addrKey.ECPubKey()
	require.NoError(t.t, err)
	pubKeyHash := btcutil.Hash160(addrPubKey.SerializeCompressed())
	witnessAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		pubKeyHash, harnessNetParams,
	)
	require.NoError(t.t, err)

	pkScript, err := txscript.PayToAddrScript(witnessAddr)
	require.NoError(t.t, err)

	// Let's send some coins to that address now.
	utxo := &wire.TxOut{
		Value:    600_000,
		PkScript: pkScript,
	}
	resp, err := alice.WalletKitClient.SendOutputs(
		ctxt, &walletrpc.SendOutputsRequest{
			Outputs: []*signrpc.TxOut{{
				Value:    utxo.Value,
				PkScript: utxo.PkScript,
			}},
			MinConfs:         0,
			SpendUnconfirmed: true,
			SatPerKw:         2500,
		},
	)
	require.NoError(t.t, err)

	prevTx := wire.NewMsgTx(2)
	err = prevTx.Deserialize(bytes.NewReader(resp.RawTx))
	require.NoError(t.t, err)

	prevOut := -1
	for idx, txOut := range prevTx.TxOut {
		if bytes.Equal(txOut.PkScript, pkScript) {
			prevOut = idx
		}
	}
	require.Greater(t.t, prevOut, -1)

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
	require.NoError(t.t, err)

	// Now let's add the meta information that we need for signing.
	packet.Inputs[0].Bip32Derivation = []*psbt.Bip32Derivation{{
		PubKey:    addrPubKey.SerializeCompressed(),
		Bip32Path: fullDerivationPath,
	}}
	packet.Inputs[0].WitnessUtxo = utxo
	packet.Inputs[0].NonWitnessUtxo = prevTx
	packet.Inputs[0].SighashType = txscript.SigHashAll

	// That's it, we should be able to sign the PSBT now.
	var buf bytes.Buffer
	err = packet.Serialize(&buf)
	require.NoError(t.t, err)

	signResp, err := alice.WalletKitClient.SignPsbt(
		ctxt, &walletrpc.SignPsbtRequest{
			FundedPsbt: buf.Bytes(),
		},
	)
	require.NoError(t.t, err)

	// Let's make sure we have a partial signature.
	signedPacket, err := psbt.NewFromRawBytes(
		bytes.NewReader(signResp.SignedPsbt), false,
	)
	require.NoError(t.t, err)

	require.Len(t.t, signedPacket.Inputs, 1)
	require.Len(t.t, signedPacket.Inputs[0].PartialSigs, 1)

	partialSig := signedPacket.Inputs[0].PartialSigs[0]
	require.Equal(t.t, partialSig.PubKey, addrPubKey.SerializeCompressed())
	require.Greater(t.t, len(partialSig.Signature), btcec.MinSigLen)

	// We should be able to finalize the PSBT and extract the final TX now.
	err = psbt.MaybeFinalizeAll(signedPacket)
	require.NoError(t.t, err)

	finalTx, err := psbt.Extract(signedPacket)
	require.NoError(t.t, err)

	buf.Reset()
	err = finalTx.Serialize(&buf)
	require.NoError(t.t, err)

	// Publish the second transaction and then mine both of them.
	_, err = alice.WalletKitClient.PublishTransaction(
		ctxt, &walletrpc.Transaction{
			TxHex: buf.Bytes(),
		},
	)
	require.NoError(t.t, err)

	// Mine one block which should contain two transactions.
	block := mineBlocks(t, net, 1, 2)[0]
	firstTxHash := prevTx.TxHash()
	secondTxHash := finalTx.TxHash()
	assertTxInBlock(t, block, &firstTxHash)
	assertTxInBlock(t, block, &secondTxHash)
}

// openChannelPsbt attempts to open a channel between srcNode and destNode with
// the passed channel funding parameters. If the passed context has a timeout,
// then if the timeout is reached before the channel pending notification is
// received, an error is returned. An error is returned if the expected step
// of funding the PSBT is not received from the source node.
func openChannelPsbt(ctx context.Context, srcNode, destNode *lntest.HarnessNode,
	p lntest.OpenChannelParams) (lnrpc.Lightning_OpenChannelClient, []byte,
	error) {

	// Wait until srcNode and destNode have the latest chain synced.
	// Otherwise, we may run into a check within the funding manager that
	// prevents any funding workflows from being kicked off if the chain
	// isn't yet synced.
	if err := srcNode.WaitForBlockchainSync(); err != nil {
		return nil, nil, fmt.Errorf("unable to sync srcNode chain: %v",
			err)
	}
	if err := destNode.WaitForBlockchainSync(); err != nil {
		return nil, nil, fmt.Errorf("unable to sync destNode chain: %v",
			err)
	}

	// Send the request to open a channel to the source node now. This will
	// open a long-lived stream where we'll receive status updates about the
	// progress of the channel.
	respStream, err := srcNode.OpenChannel(ctx, &lnrpc.OpenChannelRequest{
		NodePubkey:         destNode.PubKey[:],
		LocalFundingAmount: int64(p.Amt),
		PushSat:            int64(p.PushAmt),
		Private:            p.Private,
		SpendUnconfirmed:   p.SpendUnconfirmed,
		MinHtlcMsat:        int64(p.MinHtlc),
		FundingShim:        p.FundingShim,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("unable to open channel between "+
			"source and dest: %v", err)
	}

	// Consume the "PSBT funding ready" update. This waits until the node
	// notifies us that the PSBT can now be funded.
	resp, err := receiveChanUpdate(ctx, respStream)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to consume channel update "+
			"message: %v", err)
	}
	upd, ok := resp.Update.(*lnrpc.OpenStatusUpdate_PsbtFund)
	if !ok {
		return nil, nil, fmt.Errorf("expected PSBT funding update, "+
			"instead got %v", resp)
	}
	return respStream, upd.PsbtFund.Psbt, nil
}

// receiveChanUpdate waits until a message is received on the stream or the
// context is canceled. The context must have a timeout or must be canceled
// in case no message is received, otherwise this function will block forever.
func receiveChanUpdate(ctx context.Context,
	stream lnrpc.Lightning_OpenChannelClient) (*lnrpc.OpenStatusUpdate,
	error) {

	chanMsg := make(chan *lnrpc.OpenStatusUpdate)
	errChan := make(chan error)
	go func() {
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		chanMsg <- resp
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("timeout reached before chan pending " +
			"update sent")

	case err := <-errChan:
		return nil, err

	case updateMsg := <-chanMsg:
		return updateMsg, nil
	}
}
