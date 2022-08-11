package itest

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntemp"
	"github.com/lightningnetwork/lnd/lntemp/node"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testPsbtChanFunding makes sure a channel can be opened between carol and dave
// by using a Partially Signed Bitcoin Transaction that funds the channel
// multisig funding output.
func testPsbtChanFunding(ht *lntemp.HarnessTest) {
	// First, we'll create two new nodes that we'll use to open channels
	// between for this test. Dave gets some coins that will be used to
	// fund the PSBT, just to make sure that Carol has an empty wallet.
	carol := ht.NewNode("carol", nil)
	dave := ht.NewNode("dave", nil)

	runPsbtChanFunding(ht, carol, dave)
}

// runPsbtChanFunding makes sure a channel can be opened between carol and dave
// by using a Partially Signed Bitcoin Transaction that funds the channel
// multisig funding output.
func runPsbtChanFunding(ht *lntemp.HarnessTest, carol, dave *node.HarnessNode) {
	const chanSize = funding.MaxBtcFundingAmount
	ht.FundCoins(btcutil.SatoshiPerBitcoin, dave)

	// Before we start the test, we'll ensure both sides are connected so
	// the funding flow can be properly executed.
	alice := ht.Alice
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
		carol, dave, lntemp.OpenChannelParams{
			Amt: chanSize,
			FundingShim: &lnrpc.FundingShim{
				Shim: &lnrpc.FundingShim_PsbtShim{
					PsbtShim: &lnrpc.PsbtShim{
						PendingChanId: pendingChanID,
						NoPublish:     true,
					},
				},
			},
		},
	)

	// Let's add a second channel to the batch. This time between Carol and
	// Alice. We will publish the batch TX once this channel funding is
	// complete.
	chanUpdates2, psbtBytes2 := ht.OpenChannelPsbt(
		carol, alice, lntemp.OpenChannelParams{
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
	ht.Miner.AssertNumTxsInMempool(0)

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
	ht.Miner.AssertTxInBlock(block, &txHash)
	ht.AssertTopologyChannelOpen(carol, chanPoint)
	ht.AssertTopologyChannelOpen(carol, chanPoint2)

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

	// TODO(yy): remove the sleep once the following bug is fixed. When the
	// payment is reported as settled by Carol, it's expected the
	// commitment dance is finished and all subsequent states have been
	// updated. Yet we'd receive the error `cannot co-op close channel with
	// active htlcs` or `link failed to shutdown` if we close the channel.
	// We need to investigate the order of settling the payments and
	// updating commitments to understand and fix .
	time.Sleep(2 * time.Second)

	// To conclude, we'll close the newly created channel between Carol and
	// Dave. This function will also block until the channel is closed and
	// will additionally assert the relevant channel closing post
	// conditions.
	ht.CloseChannel(carol, chanPoint)
	ht.CloseChannel(carol, chanPoint2)
}

// testPsbtChanFundingExternal makes sure a channel can be opened between carol
// and dave by using a Partially Signed Bitcoin Transaction that funds the
// channel multisig funding output and is fully funded by an external third
// party.
func testPsbtChanFundingExternal(ht *lntemp.HarnessTest) {
	const chanSize = funding.MaxBtcFundingAmount

	// First, we'll create two new nodes that we'll use to open channels
	// between for this test. Both these nodes have an empty wallet as Alice
	// will be funding the channel.
	carol := ht.NewNode("carol", nil)
	dave := ht.NewNode("dave", nil)

	// Before we start the test, we'll ensure both sides are connected so
	// the funding flow can be properly executed.
	alice := ht.Alice
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
		carol, dave, lntemp.OpenChannelParams{
			Amt: chanSize,
			FundingShim: &lnrpc.FundingShim{
				Shim: &lnrpc.FundingShim_PsbtShim{
					PsbtShim: &lnrpc.PsbtShim{
						PendingChanId: pendingChanID,
						NoPublish:     true,
					},
				},
			},
		},
	)

	// Let's add a second channel to the batch. This time between Carol and
	// Alice. We will publish the batch TX once this channel funding is
	// complete.
	chanUpdates2, psbtBytes2 := ht.OpenChannelPsbt(
		carol, alice, lntemp.OpenChannelParams{
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
	ht.Miner.AssertNumTxsInMempool(0)

	// Great, now let's publish the final raw transaction.
	var finalTx wire.MsgTx
	err := finalTx.Deserialize(bytes.NewReader(finalizeRes.RawFinalTx))
	require.NoError(ht, err)

	txHash := finalTx.TxHash()
	_, err = ht.Miner.Client.SendRawTransaction(&finalTx, false)
	require.NoError(ht, err)

	// Now we can mine a block to get the transaction confirmed, then wait
	// for the new channel to be propagated through the network.
	block := ht.MineBlocksAndAssertNumTxes(6, 1)[0]
	ht.Miner.AssertTxInBlock(block, &txHash)
	ht.AssertTopologyChannelOpen(carol, chanPoint)
	ht.AssertTopologyChannelOpen(carol, chanPoint2)

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

	// TODO(yy): remove the sleep once the following bug is fixed. When the
	// payment is reported as settled by Carol, it's expected the
	// commitment dance is finished and all subsequent states have been
	// updated. Yet we'd receive the error `cannot co-op close channel with
	// active htlcs` or `link failed to shutdown` if we close the channel.
	// We need to investigate the order of settling the payments and
	// updating commitments to understand and fix .
	time.Sleep(2 * time.Second)

	// To conclude, we'll close the newly created channel between Carol and
	// Dave. This function will also block until the channels are closed and
	// will additionally assert the relevant channel closing post
	// conditions.
	ht.CloseChannel(carol, chanPoint)
	ht.CloseChannel(carol, chanPoint2)
}

// testPsbtChanFundingSingleStep checks whether PSBT funding works also when
// the wallet of both nodes are empty and one of them uses PSBT and an external
// wallet to fund the channel while creating reserve output in the same
// transaction.
func testPsbtChanFundingSingleStep(ht *lntemp.HarnessTest) {
	const chanSize = funding.MaxBtcFundingAmount

	args := nodeArgsForCommitType(lnrpc.CommitmentType_ANCHORS)

	// First, we'll create two new nodes that we'll use to open channels
	// between for this test. But in this case both nodes have an empty
	// wallet.
	carol := ht.NewNode("carol", args)
	defer ht.Shutdown(carol)

	dave := ht.NewNode("dave", args)
	defer ht.Shutdown(dave)

	alice := ht.Alice
	ht.FundCoins(btcutil.SatoshiPerBitcoin, alice)

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
		carol, dave, lntemp.OpenChannelParams{
			Amt: chanSize,
			FundingShim: &lnrpc.FundingShim{
				Shim: &lnrpc.FundingShim_PsbtShim{
					PsbtShim: &lnrpc.PsbtShim{
						PendingChanId: pendingChanID,
						NoPublish:     false,
					},
				},
			},
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
	ht.Miner.AssertTxInBlock(block, &txHash)
	ht.AssertTopologyChannelOpen(carol, chanPoint)

	// Next, to make sure the channel functions as normal, we'll make some
	// payments within the channel.
	payAmt := btcutil.Amount(100000)
	invoice := &lnrpc.Invoice{
		Memo:  "new chans",
		Value: int64(payAmt),
	}
	resp := dave.RPC.AddInvoice(invoice)
	ht.CompletePaymentRequests(carol, []string{resp.PaymentRequest})

	// TODO(yy): remove the sleep once the following bug is fixed. When the
	// payment is reported as settled by Carol, it's expected the
	// commitment dance is finished and all subsequent states have been
	// updated. Yet we'd receive the error `cannot co-op close channel with
	// active htlcs` or `link failed to shutdown` if we close the channel.
	// We need to investigate the order of settling the payments and
	// updating commitments to understand and fix .
	time.Sleep(2 * time.Second)

	// To conclude, we'll close the newly created channel between Carol and
	// Dave. This function will also block until the channel is closed and
	// will additionally assert the relevant channel closing post
	// conditions.
	ht.CloseChannel(carol, chanPoint)
}

// testSignPsbt tests that the SignPsbt RPC works correctly.
func testSignPsbt(net *lntest.NetworkHarness, t *harnessTest) {
	runSignPsbtSegWitV0P2WKH(t, net, net.Alice)
	runSignPsbtSegWitV0NP2WKH(t, net, net.Alice)
	runSignPsbtSegWitV1KeySpendBip86(t, net, net.Alice)
	runSignPsbtSegWitV1KeySpendRootHash(t, net, net.Alice)
	runSignPsbtSegWitV1ScriptSpend(t, net, net.Alice)

	// The above tests all make sure we can sign for keys that aren't in the
	// wallet. But we also want to make sure we can fund and then sign PSBTs
	// from our wallet.
	runFundAndSignPsbt(t, net, net.Alice)
}

// runSignPsbtSegWitV0P2WKH tests that the SignPsbt RPC works correctly for a
// SegWit v0 p2wkh input.
func runSignPsbtSegWitV0P2WKH(t *harnessTest, net *lntest.NetworkHarness,
	alice *lntest.HarnessNode) {

	// Everything we do here should be done within a second or two, so we
	// can just keep a single timeout context around for all calls.
	ctxb := context.Background()
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

	// Send some funds to the output and then try to get a signature through
	// the SignPsbt RPC to spend that output again.
	assertPsbtSpend(
		ctxt, t, net, alice, pkScript,
		func(packet *psbt.Packet) {
			in := &packet.Inputs[0]
			in.Bip32Derivation = []*psbt.Bip32Derivation{{
				PubKey:    addrPubKey.SerializeCompressed(),
				Bip32Path: fullDerivationPath,
			}}
			in.SighashType = txscript.SigHashAll
		},
		func(packet *psbt.Packet) {
			require.Len(t.t, packet.Inputs, 1)
			require.Len(t.t, packet.Inputs[0].PartialSigs, 1)

			partialSig := packet.Inputs[0].PartialSigs[0]
			require.Equal(
				t.t, partialSig.PubKey,
				addrPubKey.SerializeCompressed(),
			)
			require.Greater(
				t.t, len(partialSig.Signature), ecdsa.MinSigLen,
			)
		},
	)
}

// runSignPsbtSegWitV0NP2WKH tests that the SignPsbt RPC works correctly for a
// SegWit v0 np2wkh input.
func runSignPsbtSegWitV0NP2WKH(t *harnessTest, net *lntest.NetworkHarness,
	alice *lntest.HarnessNode) {

	// Everything we do here should be done within a second or two, so we
	// can just keep a single timeout context around for all calls.
	ctxb := context.Background()
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

	witnessProgram, err := txscript.PayToAddrScript(witnessAddr)
	require.NoError(t.t, err)
	np2wkhAddr, err := btcutil.NewAddressScriptHash(
		witnessProgram, harnessNetParams,
	)
	require.NoError(t.t, err)

	pkScript, err := txscript.PayToAddrScript(np2wkhAddr)
	require.NoError(t.t, err)

	// Send some funds to the output and then try to get a signature through
	// the SignPsbt RPC to spend that output again.
	assertPsbtSpend(
		ctxt, t, net, alice, pkScript,
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
			require.Len(t.t, packet.Inputs, 1)
			require.Len(t.t, packet.Inputs[0].PartialSigs, 1)

			partialSig := packet.Inputs[0].PartialSigs[0]
			require.Equal(
				t.t, partialSig.PubKey,
				addrPubKey.SerializeCompressed(),
			)
			require.Greater(
				t.t, len(partialSig.Signature), ecdsa.MinSigLen,
			)
		},
	)
}

// runSignPsbtSegWitV1KeySpendBip86 tests that the SignPsbt RPC works correctly
// for a SegWit v1 p2tr key spend BIP-0086 input.
func runSignPsbtSegWitV1KeySpendBip86(t *harnessTest, net *lntest.NetworkHarness,
	alice *lntest.HarnessNode) {

	// Everything we do here should be done within a second or two, so we
	// can just keep a single timeout context around for all calls.
	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// Derive a key we can use for signing.
	keyDesc, internalKey, fullDerivationPath := deriveInternalKey(
		ctxt, t, alice,
	)

	// Our taproot key is a BIP0086 key spend only construction that just
	// commits to the internal key and no root hash.
	taprootKey := txscript.ComputeTaprootKeyNoScript(internalKey)
	tapScriptAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey), harnessNetParams,
	)
	require.NoError(t.t, err)
	p2trPkScript, err := txscript.PayToAddrScript(tapScriptAddr)
	require.NoError(t.t, err)

	// Send some funds to the output and then try to get a signature through
	// the SignPsbt RPC to spend that output again.
	assertPsbtSpend(
		ctxt, t, net, alice, p2trPkScript,
		func(packet *psbt.Packet) {
			in := &packet.Inputs[0]
			in.Bip32Derivation = []*psbt.Bip32Derivation{{
				PubKey:    keyDesc.RawKeyBytes,
				Bip32Path: fullDerivationPath,
			}}
			in.TaprootBip32Derivation = []*psbt.TaprootBip32Derivation{{
				XOnlyPubKey: keyDesc.RawKeyBytes[1:],
				Bip32Path:   fullDerivationPath,
			}}
			in.SighashType = txscript.SigHashDefault
		},
		func(packet *psbt.Packet) {
			require.Len(t.t, packet.Inputs, 1)
			require.Len(
				t.t, packet.Inputs[0].TaprootKeySpendSig, 64,
			)
		},
	)
}

// runSignPsbtSegWitV1KeySpendRootHash tests that the SignPsbt RPC works
// correctly for a SegWit v1 p2tr key spend that also commits to a script tree
// root hash.
func runSignPsbtSegWitV1KeySpendRootHash(t *harnessTest,
	net *lntest.NetworkHarness, alice *lntest.HarnessNode) {

	// Everything we do here should be done within a second or two, so we
	// can just keep a single timeout context around for all calls.
	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// Derive a key we can use for signing.
	keyDesc, internalKey, fullDerivationPath := deriveInternalKey(
		ctxt, t, alice,
	)

	// Let's create a taproot script output now. This is a hash lock with a
	// simple preimage of "foobar".
	leaf1 := testScriptHashLock(t.t, []byte("foobar"))

	rootHash := leaf1.TapHash()
	taprootKey := txscript.ComputeTaprootOutputKey(internalKey, rootHash[:])
	tapScriptAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey), harnessNetParams,
	)
	require.NoError(t.t, err)
	p2trPkScript, err := txscript.PayToAddrScript(tapScriptAddr)
	require.NoError(t.t, err)

	// Send some funds to the output and then try to get a signature through
	// the SignPsbt RPC to spend that output again.
	assertPsbtSpend(
		ctxt, t, net, alice, p2trPkScript,
		func(packet *psbt.Packet) {
			in := &packet.Inputs[0]
			in.Bip32Derivation = []*psbt.Bip32Derivation{{
				PubKey:    keyDesc.RawKeyBytes,
				Bip32Path: fullDerivationPath,
			}}
			in.TaprootBip32Derivation = []*psbt.TaprootBip32Derivation{{
				XOnlyPubKey: keyDesc.RawKeyBytes[1:],
				Bip32Path:   fullDerivationPath,
			}}
			in.TaprootMerkleRoot = rootHash[:]
			in.SighashType = txscript.SigHashDefault
		},
		func(packet *psbt.Packet) {
			require.Len(t.t, packet.Inputs, 1)
			require.Len(
				t.t, packet.Inputs[0].TaprootKeySpendSig, 64,
			)
		},
	)
}

// runSignPsbtSegWitV1ScriptSpend tests that the SignPsbt RPC works correctly
// for a SegWit v1 p2tr script spend.
func runSignPsbtSegWitV1ScriptSpend(t *harnessTest,
	net *lntest.NetworkHarness, alice *lntest.HarnessNode) {

	// Everything we do here should be done within a second or two, so we
	// can just keep a single timeout context around for all calls.
	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// Derive a key we can use for signing.
	keyDesc, internalKey, fullDerivationPath := deriveInternalKey(
		ctxt, t, alice,
	)

	// Let's create a taproot script output now. This is a hash lock with a
	// simple preimage of "foobar".
	leaf1 := testScriptSchnorrSig(t.t, internalKey)

	rootHash := leaf1.TapHash()
	taprootKey := txscript.ComputeTaprootOutputKey(internalKey, rootHash[:])
	tapScriptAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(taprootKey), harnessNetParams,
	)
	require.NoError(t.t, err)
	p2trPkScript, err := txscript.PayToAddrScript(tapScriptAddr)
	require.NoError(t.t, err)

	// We need to assemble the control block to be able to spend through the
	// script path.
	tapscript := input.TapscriptPartialReveal(internalKey, leaf1, nil)
	controlBlockBytes, err := tapscript.ControlBlock.ToBytes()
	require.NoError(t.t, err)

	// Send some funds to the output and then try to get a signature through
	// the SignPsbt RPC to spend that output again.
	assertPsbtSpend(
		ctxt, t, net, alice, p2trPkScript,
		func(packet *psbt.Packet) {
			in := &packet.Inputs[0]
			in.Bip32Derivation = []*psbt.Bip32Derivation{{
				PubKey:    keyDesc.RawKeyBytes,
				Bip32Path: fullDerivationPath,
			}}
			in.TaprootBip32Derivation = []*psbt.TaprootBip32Derivation{{
				XOnlyPubKey: keyDesc.RawKeyBytes[1:],
				Bip32Path:   fullDerivationPath,
				LeafHashes:  [][]byte{rootHash[:]},
			}}
			in.SighashType = txscript.SigHashDefault
			in.TaprootLeafScript = []*psbt.TaprootTapLeafScript{{
				ControlBlock: controlBlockBytes,
				Script:       leaf1.Script,
				LeafVersion:  leaf1.LeafVersion,
			}}
		},
		func(packet *psbt.Packet) {
			require.Len(t.t, packet.Inputs, 1)
			require.Len(
				t.t, packet.Inputs[0].TaprootScriptSpendSig, 1,
			)

			scriptSpendSig := packet.Inputs[0].TaprootScriptSpendSig[0]
			require.Len(t.t, scriptSpendSig.Signature, 64)
		},
	)
}

// runFundAndSignPsbt makes sure we can sign PSBTs that were funded by our
// internal wallet.
func runFundAndSignPsbt(t *harnessTest, net *lntest.NetworkHarness,
	alice *lntest.HarnessNode) {

	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// We'll be using a "main" address where we send the funds to and from
	// several times.
	mainAddrResp, err := alice.NewAddress(ctxt, &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	})
	require.NoError(t.t, err)

	fundOutputs := map[string]uint64{
		mainAddrResp.Address: 999000,
	}
	spendAddrTypes := []lnrpc.AddressType{
		lnrpc.AddressType_NESTED_PUBKEY_HASH,
		lnrpc.AddressType_WITNESS_PUBKEY_HASH,
		lnrpc.AddressType_TAPROOT_PUBKEY,
	}

	for _, addrType := range spendAddrTypes {
		ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)

		// First, spend all the coins in the wallet to an address of the
		// given type so that UTXO will be picked when funding a PSBT.
		sendAllCoinsToAddrType(ctxt, t, net, alice, addrType)

		// Let's fund a PSBT now where we want to send a few sats to our
		// main address.
		assertPsbtFundSignSpend(ctxt, t, net, alice, fundOutputs, false)

		// Send all coins back to a single address once again.
		sendAllCoinsToAddrType(ctxt, t, net, alice, addrType)

		// And now make sure the alternate way of signing a PSBT, which
		// is calling FinalizePsbt directly, also works for this address
		// type.
		assertPsbtFundSignSpend(ctxt, t, net, alice, fundOutputs, true)

		cancel()
	}
}

// assertPsbtSpend creates an output with the given pkScript on chain and then
// attempts to create a sweep transaction that is signed using the SignPsbt RPC
// that spends that output again.
func assertPsbtSpend(ctx context.Context, t *harnessTest,
	net *lntest.NetworkHarness, alice *lntest.HarnessNode, pkScript []byte,
	decorateUnsigned func(*psbt.Packet), verifySigned func(*psbt.Packet)) {

	// Let's send some coins to that address now.
	utxo := &wire.TxOut{
		Value:    600_000,
		PkScript: pkScript,
	}
	resp, err := alice.WalletKitClient.SendOutputs(
		ctx, &walletrpc.SendOutputsRequest{
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
	packet.Inputs[0].WitnessUtxo = utxo
	packet.Inputs[0].NonWitnessUtxo = prevTx
	decorateUnsigned(packet)

	// That's it, we should be able to sign the PSBT now.
	var buf bytes.Buffer
	err = packet.Serialize(&buf)
	require.NoError(t.t, err)

	signResp, err := alice.WalletKitClient.SignPsbt(
		ctx, &walletrpc.SignPsbtRequest{
			FundedPsbt: buf.Bytes(),
		},
	)
	require.NoError(t.t, err)

	// Let's make sure we have a partial signature.
	signedPacket, err := psbt.NewFromRawBytes(
		bytes.NewReader(signResp.SignedPsbt), false,
	)
	require.NoError(t.t, err)

	// Allow the caller to also verify (and potentially move) some of the
	// returned fields.
	verifySigned(signedPacket)

	// We should be able to finalize the PSBT and extract the final TX now.
	err = psbt.MaybeFinalizeAll(signedPacket)
	require.NoError(t.t, err)

	finalTx, err := psbt.Extract(signedPacket)
	require.NoError(t.t, err)

	// Make sure we can also sign a second time. This makes sure any key
	// tweaking that happened for the signing didn't affect any keys in the
	// cache.
	signResp2, err := alice.WalletKitClient.SignPsbt(
		ctx, &walletrpc.SignPsbtRequest{
			FundedPsbt: buf.Bytes(),
		},
	)
	require.NoError(t.t, err)
	signedPacket2, err := psbt.NewFromRawBytes(
		bytes.NewReader(signResp2.SignedPsbt), false,
	)
	require.NoError(t.t, err)
	verifySigned(signedPacket2)

	buf.Reset()
	err = finalTx.Serialize(&buf)
	require.NoError(t.t, err)

	// Publish the second transaction and then mine both of them.
	_, err = alice.WalletKitClient.PublishTransaction(
		ctx, &walletrpc.Transaction{
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

// assertPsbtFundSignSpend funds a PSBT from the internal wallet and then
// attempts to sign it by using the SignPsbt or FinalizePsbt method.
func assertPsbtFundSignSpend(ctx context.Context, t *harnessTest,
	net *lntest.NetworkHarness, alice *lntest.HarnessNode,
	fundOutputs map[string]uint64, useFinalize bool) {

	fundResp, err := alice.WalletKitClient.FundPsbt(
		ctx, &walletrpc.FundPsbtRequest{
			Template: &walletrpc.FundPsbtRequest_Raw{
				Raw: &walletrpc.TxTemplate{
					Outputs: fundOutputs,
				},
			},
			Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
				SatPerVbyte: 2,
			},
			MinConfs: 1,
		},
	)
	require.NoError(t.t, err)
	require.GreaterOrEqual(
		t.t, fundResp.ChangeOutputIndex, int32(-1),
	)

	var signedPsbt []byte
	if useFinalize {
		finalizeResp, err := alice.WalletKitClient.FinalizePsbt(
			ctx, &walletrpc.FinalizePsbtRequest{
				FundedPsbt: fundResp.FundedPsbt,
			},
		)
		require.NoError(t.t, err)

		signedPsbt = finalizeResp.SignedPsbt
	} else {
		signResp, err := alice.WalletKitClient.SignPsbt(
			ctx, &walletrpc.SignPsbtRequest{
				FundedPsbt: fundResp.FundedPsbt,
			},
		)
		require.NoError(t.t, err)

		signedPsbt = signResp.SignedPsbt
	}

	// Let's make sure we have a partial signature.
	signedPacket, err := psbt.NewFromRawBytes(
		bytes.NewReader(signedPsbt), false,
	)
	require.NoError(t.t, err)

	// We should be able to finalize the PSBT and extract the final
	// TX now.
	err = psbt.MaybeFinalizeAll(signedPacket)
	require.NoError(t.t, err)

	finalTx, err := psbt.Extract(signedPacket)
	require.NoError(t.t, err)

	var buf bytes.Buffer
	err = finalTx.Serialize(&buf)
	require.NoError(t.t, err)

	// Publish the second transaction and then mine both of them.
	_, err = alice.WalletKitClient.PublishTransaction(
		ctx, &walletrpc.Transaction{
			TxHex: buf.Bytes(),
		},
	)
	require.NoError(t.t, err)

	// Mine one block which should contain two transactions.
	block := mineBlocks(t, net, 1, 1)[0]
	finalTxHash := finalTx.TxHash()
	assertTxInBlock(t, block, &finalTxHash)
}

// deriveInternalKey derives a signing key and returns its descriptor, full
// derivation path and parsed public key.
func deriveInternalKey(ctx context.Context, t *harnessTest,
	alice *lntest.HarnessNode) (*signrpc.KeyDescriptor, *btcec.PublicKey,
	[]uint32) {

	// For the next step, we need a public key. Let's use a special family
	// for this.
	keyDesc, err := alice.WalletKitClient.DeriveNextKey(
		ctx, &walletrpc.KeyReq{KeyFamily: testTaprootKeyFamily},
	)
	require.NoError(t.t, err)

	// The DeriveNextKey returns a key from the internal 1017 scope.
	fullDerivationPath := []uint32{
		hdkeychain.HardenedKeyStart + keychain.BIP0043Purpose,
		hdkeychain.HardenedKeyStart + harnessNetParams.HDCoinType,
		hdkeychain.HardenedKeyStart + uint32(keyDesc.KeyLoc.KeyFamily),
		0,
		uint32(keyDesc.KeyLoc.KeyIndex),
	}

	parsedPubKey, err := btcec.ParsePubKey(keyDesc.RawKeyBytes)
	require.NoError(t.t, err)

	return keyDesc, parsedPubKey, fullDerivationPath
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

// sendAllCoinsToAddrType sweeps all coins from the wallet and sends them to a
// new address of the given type.
func sendAllCoinsToAddrType(ctx context.Context, t *harnessTest,
	net *lntest.NetworkHarness, node *lntest.HarnessNode,
	addrType lnrpc.AddressType) {

	resp, err := node.NewAddress(ctx, &lnrpc.NewAddressRequest{
		Type: addrType,
	})
	require.NoError(t.t, err)

	_, err = node.SendCoins(ctx, &lnrpc.SendCoinsRequest{
		Addr:    resp.Address,
		SendAll: true,
	})
	require.NoError(t.t, err)

	_ = mineBlocks(t, net, 1, 1)[0]
	err = node.WaitForBlockchainSync()
	require.NoError(t.t, err)
}
