package itest

import (
	"bytes"

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
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testPsbtChanFunding makes sure a channel can be opened between carol and dave
// by using a Partially Signed Bitcoin Transaction that funds the channel
// multisig funding output.
func testPsbtChanFunding(ht *lntest.HarnessTest) {
	// First, we'll create two new nodes that we'll use to open channels
	// between for this test. Dave gets some coins that will be used to
	// fund the PSBT, just to make sure that Carol has an empty wallet.
	carol := ht.NewNode("carol", nil)
	defer ht.Shutdown(carol)

	dave := ht.NewNode("dave", nil)
	defer ht.Shutdown(dave)

	runPsbtChanFunding(ht, carol, dave)
}

// runPsbtChanFunding makes sure a channel can be opened between carol and dave
// by using a Partially Signed Bitcoin Transaction that funds the channel
// multisig funding output.
func runPsbtChanFunding(ht *lntest.HarnessTest, carol,
	dave *lntest.HarnessNode) {

	const chanSize = funding.MaxBtcFundingAmount
	ht.SendCoins(btcutil.SatoshiPerBitcoin, dave)

	// Before we start the test, we'll ensure both sides are connected so
	// the funding flow can be properly executed.
	alice := ht.Alice()
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
	fundResp := ht.FundPsbt(dave, fundReq)

	// We have a PSBT that has no witness data yet, which is exactly what we
	// need for the next step: Verify the PSBT with the funding intents.
	ht.FundingStateStep(carol, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: pendingChanID,
				FundedPsbt:    fundResp.FundedPsbt,
			},
		},
	})
	ht.FundingStateStep(carol, &lnrpc.FundingTransitionMsg{
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
	finalizeRes := ht.FinalizePsbt(dave, finalizeReq)

	// We've signed our PSBT now, let's pass it to the intent again.
	ht.FundingStateStep(carol, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtFinalize{
			PsbtFinalize: &lnrpc.FundingPsbtFinalize{
				PendingChanId: pendingChanID,
				SignedPsbt:    finalizeRes.SignedPsbt,
			},
		},
	})

	// Consume the "channel pending" update. This waits until the funding
	// transaction was fully compiled.
	updateResp := ht.ReceiveChanUpdate(chanUpdates)
	upd, ok := updateResp.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.True(ht, ok)
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: upd.ChanPending.Txid,
		},
		OutputIndex: upd.ChanPending.OutputIndex,
	}

	// No transaction should have been published yet.
	mempool := ht.GetRawMempool()
	require.Empty(ht, mempool)

	// Let's progress the second channel now. This time we'll use the raw
	// wire format transaction directly.
	ht.FundingStateStep(carol, &lnrpc.FundingTransitionMsg{
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
	updateResp2 := ht.ReceiveChanUpdate(chanUpdates2)
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
	block := ht.MineBlocksAndAssertTx(6, 1)[0]
	ht.AssertTxInBlock(block, &txHash)
	ht.AssertChannelOpen(carol, chanPoint)
	ht.AssertChannelOpen(carol, chanPoint2)

	// With the channel open, ensure that it is counted towards Carol's
	// total channel balance.
	balRes := ht.GetChannelBalance(carol)
	require.NotZero(ht, balRes.LocalBalance.Sat)

	// Next, to make sure the channel functions as normal, we'll make some
	// payments within the channel.
	payAmt := btcutil.Amount(100000)
	invoice := &lnrpc.Invoice{
		Memo:  "new chans",
		Value: int64(payAmt),
	}
	resp := ht.AddInvoice(invoice, dave)
	ht.CompletePaymentRequests(carol, []string{resp.PaymentRequest}, true)

	// To conclude, we'll close the newly created channel between Carol and
	// Dave. This function will also block until the channel is closed and
	// will additionally assert the relevant channel closing post
	// conditions.
	ht.CloseChannel(carol, chanPoint, false)
}

// testPsbtChanFundingExternal makes sure a channel can be opened between carol
// and dave by using a Partially Signed Bitcoin Transaction that funds the
// channel multisig funding output and is fully funded by an external third
// party.
func testPsbtChanFundingExternal(ht *lntest.HarnessTest) {
	const chanSize = funding.MaxBtcFundingAmount

	// First, we'll create two new nodes that we'll use to open channels
	// between for this test. Both these nodes have an empty wallet as Alice
	// will be funding the channel.
	carol := ht.NewNode("carol", nil)
	defer ht.Shutdown(carol)

	dave := ht.NewNode("dave", nil)
	defer ht.Shutdown(dave)

	// Before we start the test, we'll ensure both sides are connected so
	// the funding flow can be properly executed.
	alice := ht.Alice()
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
	fundResp := ht.FundPsbt(alice, fundReq)

	// We have a PSBT that has no witness data yet, which is exactly what we
	// need for the next step: Verify the PSBT with the funding intents.
	// We tell the PSBT intent to skip the finalize step because we know the
	// final transaction will not be broadcast by Carol herself but by
	// Alice. And we assume that Alice is a third party that is not in
	// direct communication with Carol and won't send the signed TX to her
	// before broadcasting it. So we cannot call the finalize step but
	// instead just tell lnd to wait for a TX to be published/confirmed.
	ht.FundingStateStep(carol, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: pendingChanID,
				FundedPsbt:    fundResp.FundedPsbt,
				SkipFinalize:  true,
			},
		},
	})
	ht.FundingStateStep(carol, &lnrpc.FundingTransitionMsg{
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
	updateResp := ht.ReceiveChanUpdate(chanUpdates)
	upd, ok := updateResp.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.True(ht, ok)
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: upd.ChanPending.Txid,
		},
		OutputIndex: upd.ChanPending.OutputIndex,
	}
	updateResp2 := ht.ReceiveChanUpdate(chanUpdates2)
	upd2, ok := updateResp2.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.True(ht, ok)
	chanPoint2 := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: upd2.ChanPending.Txid,
		},
		OutputIndex: upd2.ChanPending.OutputIndex,
	}
	pendings := ht.GetPendingChannels(carol)
	require.Len(ht, pendings.PendingOpenChannels, 2)

	// Now we'll ask Alice's wallet to sign the PSBT so we can finish the
	// funding flow.
	finalizeReq := &walletrpc.FinalizePsbtRequest{
		FundedPsbt: fundResp.FundedPsbt,
	}
	finalizeRes := ht.FinalizePsbt(alice, finalizeReq)

	// No transaction should have been published yet.
	mempool := ht.GetRawMempool()
	require.Empty(ht, mempool)

	// Great, now let's publish the final raw transaction.
	var finalTx wire.MsgTx
	err := finalTx.Deserialize(bytes.NewReader(finalizeRes.RawFinalTx))
	require.NoError(ht, err)

	txHash := finalTx.TxHash()
	_, err = ht.Miner().Client.SendRawTransaction(&finalTx, false)
	require.NoError(ht, err)

	// Now we can mine a block to get the transaction confirmed, then wait
	// for the new channel to be propagated through the network.
	block := ht.MineBlocksAndAssertTx(6, 1)[0]
	ht.AssertTxInBlock(block, &txHash)
	ht.AssertChannelOpen(carol, chanPoint)
	ht.AssertChannelOpen(carol, chanPoint2)

	// With the channel open, ensure that it is counted towards Carol's
	// total channel balance.
	balRes := ht.GetChannelBalance(carol)
	require.NotZero(ht, balRes.LocalBalance.Sat)

	// Next, to make sure the channel functions as normal, we'll make some
	// payments within the channel.
	payAmt := btcutil.Amount(100000)
	invoice := &lnrpc.Invoice{
		Memo:  "new chans",
		Value: int64(payAmt),
	}
	resp := ht.AddInvoice(invoice, dave)
	ht.CompletePaymentRequests(carol, []string{resp.PaymentRequest}, true)

	// To conclude, we'll close the newly created channel between Carol and
	// Dave. This function will also block until the channels are closed and
	// will additionally assert the relevant channel closing post
	// conditions.
	ht.CloseChannel(carol, chanPoint, false)
	ht.CloseChannel(carol, chanPoint2, false)
}

// testPsbtChanFundingSingleStep checks whether PSBT funding works also when
// the wallet of both nodes are empty and one of them uses PSBT and an external
// wallet to fund the channel while creating reserve output in the same
// transaction.
func testPsbtChanFundingSingleStep(ht *lntest.HarnessTest) {
	const chanSize = funding.MaxBtcFundingAmount

	args := nodeArgsForCommitType(lnrpc.CommitmentType_ANCHORS)

	// First, we'll create two new nodes that we'll use to open channels
	// between for this test. But in this case both nodes have an empty
	// wallet.
	carol := ht.NewNode("carol", args)
	defer ht.Shutdown(carol)

	dave := ht.NewNode("dave", args)
	defer ht.Shutdown(dave)

	ht.SendCoins(btcutil.SatoshiPerBitcoin, ht.Alice())

	// Get new address for anchor reserve.
	addrResp := ht.NewAddress(carol, lnrpc.AddressType_WITNESS_PUBKEY_HASH)
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
	fundResp := ht.FundPsbt(ht.Alice(), fundReq)

	// Make sure the wallets are actually empty
	unspentCarol := ht.ListUnspent(carol, "", 0, 0)
	require.Len(ht, unspentCarol.Utxos, 0)

	unspentDave := ht.ListUnspent(dave, "", 0, 0)
	require.Len(ht, unspentDave.Utxos, 0)

	// We have a PSBT that has no witness data yet, which is exactly what we
	// need for the next step: Verify the PSBT with the funding intents.
	ht.FundingStateStep(carol, &lnrpc.FundingTransitionMsg{
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
	finalizeRes := ht.FinalizePsbt(ht.Alice(), finalizeReq)

	// We've signed our PSBT now, let's pass it to the intent again.
	ht.FundingStateStep(carol, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtFinalize{
			PsbtFinalize: &lnrpc.FundingPsbtFinalize{
				PendingChanId: pendingChanID,
				SignedPsbt:    finalizeRes.SignedPsbt,
			},
		},
	})

	// Consume the "channel pending" update. This waits until the funding
	// transaction was fully compiled.
	updateResp := ht.ReceiveChanUpdate(chanUpdates)
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
	block := ht.MineBlocksAndAssertTx(6, 1)[0]
	ht.AssertTxInBlock(block, &txHash)
	ht.AssertChannelOpen(carol, chanPoint)

	// Next, to make sure the channel functions as normal, we'll make some
	// payments within the channel.
	payAmt := btcutil.Amount(100000)
	invoice := &lnrpc.Invoice{
		Memo:  "new chans",
		Value: int64(payAmt),
	}
	resp := ht.AddInvoice(invoice, dave)
	ht.CompletePaymentRequests(carol, []string{resp.PaymentRequest}, true)

	// To conclude, we'll close the newly created channel between Carol and
	// Dave. This function will also block until the channel is closed and
	// will additionally assert the relevant channel closing post
	// conditions.
	ht.CloseChannel(carol, chanPoint, false)
}

// testSignPsbt tests that the SignPsbt RPC works correctly.
func testSignPsbt(ht *lntest.HarnessTest) {
	alice := ht.Alice()
	runSignPsbtSegWitV0P2WKH(ht, alice)
	runSignPsbtSegWitV1KeySpendBip86(ht, alice)
	runSignPsbtSegWitV1KeySpendRootHash(ht, alice)
	runSignPsbtSegWitV1ScriptSpend(ht, alice)
}

// runSignPsbtSegWitV0P2WKH tests that the SignPsbt RPC works correctly for a
// SegWit v0 p2wkh input.
func runSignPsbtSegWitV0P2WKH(ht *lntest.HarnessTest,
	alice *lntest.HarnessNode) {

	// We test that we can sign a PSBT that spends funds from an input that
	// the wallet doesn't know about. To set up that test case, we first
	// derive an address manually that the wallet won't be watching on
	// chain. We can do that by exporting the account xpub of lnd's main
	// account.
	accounts := ht.ListAccounts(alice, &walletrpc.ListAccountsRequest{})
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
	change, err := xpub.DeriveNonStandard(changeIndex) // nolint:staticcheck
	require.NoError(ht, err)

	// At an index that we are certainly not watching in the wallet.
	addrKey, err := change.DeriveNonStandard(addrIndex) // nolint:staticcheck
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

// runSignPsbtSegWitV1KeySpendBip86 tests that the SignPsbt RPC works correctly
// for a SegWit v1 p2tr key spend BIP-0086 input.
func runSignPsbtSegWitV1KeySpendBip86(ht *lntest.HarnessTest,
	alice *lntest.HarnessNode) {

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
			in.TaprootBip32Derivation = []*psbt.TaprootBip32Derivation{{
				XOnlyPubKey: keyDesc.RawKeyBytes[1:],
				Bip32Path:   fullDerivationPath,
			}}
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
	alice *lntest.HarnessNode) {

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
			in.TaprootBip32Derivation = []*psbt.TaprootBip32Derivation{{
				XOnlyPubKey: keyDesc.RawKeyBytes[1:],
				Bip32Path:   fullDerivationPath,
			}}
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
	alice *lntest.HarnessNode) {

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
			require.Len(ht, packet.Inputs, 1)
			require.Len(
				ht, packet.Inputs[0].TaprootScriptSpendSig, 1,
			)

			scriptSpendSig := packet.Inputs[0].TaprootScriptSpendSig[0]
			require.Len(ht, scriptSpendSig.Signature, 64)
		},
	)
}

// assertPsbtSpend creates an output with the given pkScript on chain and then
// attempts to create a sweep transaction that is signed using the SignPsbt RPC
// that spends that output again.
func assertPsbtSpend(ht *lntest.HarnessTest, alice *lntest.HarnessNode,
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
	resp := ht.SendOutputs(alice, req)

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

	// Now let's add the meta information that we need for signing.
	packet.Inputs[0].WitnessUtxo = utxo
	packet.Inputs[0].NonWitnessUtxo = prevTx
	decorateUnsigned(packet)

	// That's it, we should be able to sign the PSBT now.
	var buf bytes.Buffer
	err = packet.Serialize(&buf)
	require.NoError(ht, err)

	signReq := &walletrpc.SignPsbtRequest{FundedPsbt: buf.Bytes()}
	signResp := ht.SignPsbt(alice, signReq)

	// Let's make sure we have a partial signature.
	signedPacket, err := psbt.NewFromRawBytes(
		bytes.NewReader(signResp.SignedPsbt), false,
	)
	require.NoError(ht, err)

	// Allow the caller to also verify (and potentially move) some of the
	// returned fields.
	verifySigned(signedPacket)

	// We should be able to finalize the PSBT and extract the final TX now.
	err = psbt.MaybeFinalizeAll(signedPacket)
	require.NoError(ht, err)

	finalTx, err := psbt.Extract(signedPacket)
	require.NoError(ht, err)

	buf.Reset()
	err = finalTx.Serialize(&buf)
	require.NoError(ht, err)

	// Publish the second transaction and then mine both of them.
	txReq := &walletrpc.Transaction{TxHex: buf.Bytes()}
	ht.PublishTransaction(alice, txReq)

	// Mine one block which should contain two transactions.
	block := ht.MineBlocksAndAssertTx(1, 2)[0]
	firstTxHash := prevTx.TxHash()
	secondTxHash := finalTx.TxHash()
	ht.AssertTxInBlock(block, &firstTxHash)
	ht.AssertTxInBlock(block, &secondTxHash)
}

// deriveInternalKey derives a signing key and returns its descriptor, full
// derivation path and parsed public key.
func deriveInternalKey(ht *lntest.HarnessTest,
	alice *lntest.HarnessNode) (*signrpc.KeyDescriptor, *btcec.PublicKey,
	[]uint32) {

	// For the next step, we need a public key. Let's use a special family
	// for this.
	req := &walletrpc.KeyReq{KeyFamily: testTaprootKeyFamily}
	keyDesc := ht.DeriveNextKey(alice, req)

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
