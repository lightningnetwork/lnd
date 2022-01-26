package itest

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/hdkeychain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

const (
	defaultAccount         = lnwallet.DefaultAccountName
	defaultImportedAccount = waddrmgr.ImportedAddrAccountName
)

// walletToLNAddrType maps walletrpc.AddressType to lnrpc.AddressType.
func walletToLNAddrType(t *testing.T, addrType walletrpc.AddressType) lnrpc.AddressType {
	switch addrType {
	case walletrpc.AddressType_NESTED_WITNESS_PUBKEY_HASH,
		walletrpc.AddressType_HYBRID_NESTED_WITNESS_PUBKEY_HASH:

		return lnrpc.AddressType_NESTED_PUBKEY_HASH

	case walletrpc.AddressType_WITNESS_PUBKEY_HASH:
		return lnrpc.AddressType_WITNESS_PUBKEY_HASH

	default:
		t.Fatalf("unhandled addr type %v", addrType)
		return 0
	}
}

// newExternalAddr generates a new external address of an imported account for a
// pair of nodes, where one acts as the funder and the other as the signer.
func newExternalAddr(t *testing.T, funder, signer *lntest.HarnessNode,
	importedAccount string, addrType walletrpc.AddressType) string {

	// We'll generate a new address for Carol from Dave's node to receive
	// and fund a new channel.
	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	funderResp, err := funder.NewAddress(ctxt, &lnrpc.NewAddressRequest{
		Type:    walletToLNAddrType(t, addrType),
		Account: importedAccount,
	})
	require.NoError(t, err)

	// Carol also needs to generate the address for the sake of this test to
	// be able to sign the channel funding input.
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	signerResp, err := signer.NewAddress(ctxt, &lnrpc.NewAddressRequest{
		Type: walletToLNAddrType(t, addrType),
	})
	require.NoError(t, err)

	// Sanity check that the generated addresses match.
	require.Equal(t, funderResp.Address, signerResp.Address)
	assertExternalAddrType(t, funderResp.Address, addrType)

	return funderResp.Address
}

// assertExternalAddrType asserts that an external address generated for an
// imported account is of the expected type.
func assertExternalAddrType(t *testing.T, addrStr string,
	accountAddrType walletrpc.AddressType) {

	addr, err := btcutil.DecodeAddress(addrStr, harnessNetParams)
	require.NoError(t, err)

	switch accountAddrType {
	case walletrpc.AddressType_WITNESS_PUBKEY_HASH:
		require.IsType(t, addr, &btcutil.AddressWitnessPubKeyHash{})

	case walletrpc.AddressType_NESTED_WITNESS_PUBKEY_HASH,
		walletrpc.AddressType_HYBRID_NESTED_WITNESS_PUBKEY_HASH:

		require.IsType(t, addr, &btcutil.AddressScriptHash{})

	default:
		t.Fatalf("unsupported account addr type %v", accountAddrType)
	}
}

// assertOutputScriptType asserts that a transaction's output, indicated by the
// output with the given amount, has a script of the expected type. This assumes
// all transaction outputs have unique amounts.
func assertOutputScriptType(t *testing.T, expType txscript.ScriptClass,
	tx *wire.MsgTx, outputAmt int64) {

	for _, txOut := range tx.TxOut {
		if txOut.Value != outputAmt {
			continue
		}

		pkScript, err := txscript.ParsePkScript(txOut.PkScript)
		require.NoError(t, err)
		require.Equal(t, expType, pkScript.Class())
		return
	}

	// No output with the given amount was found.
	t.Fatalf("output with amount %v not found in transaction %v", outputAmt,
		spew.Sdump(tx))
}

// assertAccountBalance asserts that the unconfirmed and confirmed balance for
// the given account is satisfied by the WalletBalance and ListUnspent RPCs. The
// unconfirmed balance is not checked for neutrino nodes.
func assertAccountBalance(t *testing.T, node *lntest.HarnessNode, account string,
	confirmedBalance, unconfirmedBalance int64) {

	err := wait.NoError(func() error {
		balanceResp, err := node.WalletBalance(
			context.Background(), &lnrpc.WalletBalanceRequest{},
		)
		if err != nil {
			return err
		}
		require.Contains(t, balanceResp.AccountBalance, account)
		accountBalance := balanceResp.AccountBalance[account]

		// Check confirmed balance.
		if accountBalance.ConfirmedBalance != confirmedBalance {
			return fmt.Errorf("expected confirmed balance %v, "+
				"got %v", confirmedBalance,
				accountBalance.ConfirmedBalance)
		}
		listUtxosReq := &lnrpc.ListUnspentRequest{
			MinConfs: 1,
			MaxConfs: math.MaxInt32,
			Account:  account,
		}
		confirmedUtxosResp, err := node.ListUnspent(
			context.Background(), listUtxosReq,
		)
		if err != nil {
			return err
		}
		var totalConfirmedVal int64
		for _, utxo := range confirmedUtxosResp.Utxos {
			totalConfirmedVal += utxo.AmountSat
		}
		if totalConfirmedVal != confirmedBalance {
			return fmt.Errorf("expected total confirmed utxo "+
				"balance %v, got %v", confirmedBalance,
				totalConfirmedVal)
		}

		// Skip unconfirmed balance checks for neutrino nodes.
		if node.Cfg.BackendCfg.Name() == lntest.NeutrinoBackendName {
			return nil
		}

		// Check unconfirmed balance.
		if accountBalance.UnconfirmedBalance != unconfirmedBalance {
			return fmt.Errorf("expected unconfirmed balance %v, "+
				"got %v", unconfirmedBalance,
				accountBalance.UnconfirmedBalance)
		}
		listUtxosReq.MinConfs = 0
		listUtxosReq.MaxConfs = 0
		unconfirmedUtxosResp, err := node.ListUnspent(
			context.Background(), listUtxosReq,
		)
		require.NoError(t, err)
		var totalUnconfirmedVal int64
		for _, utxo := range unconfirmedUtxosResp.Utxos {
			totalUnconfirmedVal += utxo.AmountSat
		}
		if totalUnconfirmedVal != unconfirmedBalance {
			return fmt.Errorf("expected total unconfirmed utxo "+
				"balance %v, got %v", unconfirmedBalance,
				totalUnconfirmedVal)
		}

		return nil
	}, defaultTimeout)
	require.NoError(t, err)
}

// psbtSendFromImportedAccount attempts to fund a PSBT from the given imported
// account, originating from the source node to the destination.
func psbtSendFromImportedAccount(t *harnessTest, srcNode, destNode,
	signer *lntest.HarnessNode, account string,
	accountAddrType walletrpc.AddressType) {

	ctxb := context.Background()

	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	balanceResp, err := srcNode.WalletBalance(ctxt, &lnrpc.WalletBalanceRequest{})
	require.NoError(t.t, err)
	require.Contains(t.t, balanceResp.AccountBalance, account)
	confBalance := balanceResp.AccountBalance[account].ConfirmedBalance

	destAmt := confBalance - 10000
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	destAddrResp, err := destNode.NewAddress(ctxt, &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	})
	require.NoError(t.t, err)

	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	fundReq := &walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_Raw{
			Raw: &walletrpc.TxTemplate{
				Outputs: map[string]uint64{
					destAddrResp.Address: uint64(destAmt),
				},
			},
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: 1,
		},
		Account: account,
	}
	fundResp, err := srcNode.WalletKitClient.FundPsbt(ctxt, fundReq)
	require.NoError(t.t, err)

	// Have Carol sign the PSBT input since Dave doesn't have any private
	// key information.
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	finalizeReq := &walletrpc.FinalizePsbtRequest{
		FundedPsbt: fundResp.FundedPsbt,
	}
	finalizeResp, err := signer.WalletKitClient.FinalizePsbt(ctxt, finalizeReq)
	require.NoError(t.t, err)

	// With the PSBT signed, we can broadcast the resulting transaction.
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	publishReq := &walletrpc.Transaction{
		TxHex: finalizeResp.RawFinalTx,
	}
	_, err = srcNode.WalletKitClient.PublishTransaction(ctxt, publishReq)
	require.NoError(t.t, err)

	// Carol's balance from Dave's perspective should update accordingly.
	var (
		expTxFee            int64
		expChangeScriptType txscript.ScriptClass
	)
	switch accountAddrType {
	case walletrpc.AddressType_WITNESS_PUBKEY_HASH:
		expTxFee = 141
		expChangeScriptType = txscript.WitnessV0PubKeyHashTy

	case walletrpc.AddressType_NESTED_WITNESS_PUBKEY_HASH:
		if account != defaultImportedAccount {
			expTxFee = 165
			expChangeScriptType = txscript.ScriptHashTy
			break
		}

		// Spends from the default NP2WKH imported account have the same
		// fee rate as the hybrid address type since a NP2WKH input is
		// spent and a P2WKH change output is created.
		fallthrough

	case walletrpc.AddressType_HYBRID_NESTED_WITNESS_PUBKEY_HASH:
		expTxFee = 164
		expChangeScriptType = txscript.WitnessV0PubKeyHashTy

	default:
		t.Fatalf("unsupported addr type %v", accountAddrType)
	}
	changeUtxoAmt := confBalance - destAmt - expTxFee

	// If the transaction was created from the default imported account,
	// then any change produced is moved to the default wallet account.
	accountWithBalance := account
	if account == defaultImportedAccount {
		accountWithBalance = defaultAccount
	}
	assertAccountBalance(t.t, srcNode, accountWithBalance, 0, changeUtxoAmt)
	_ = mineBlocks(t, t.lndHarness, 1, 1)
	assertAccountBalance(t.t, srcNode, accountWithBalance, changeUtxoAmt, 0)

	// Finally, assert that the transaction has the expected change address
	// type based on the account.
	var tx wire.MsgTx
	err = tx.Deserialize(bytes.NewReader(finalizeResp.RawFinalTx))
	require.NoError(t.t, err)
	assertOutputScriptType(t.t, expChangeScriptType, &tx, changeUtxoAmt)
}

// fundChanAndCloseFromImportedAccount attempts to a fund a channel from the
// given imported account, originating from the source node to the destination
// node. To ensure the channel is operational before closing it, a test payment
// is made. Several balance assertions are made along the way for the sake of
// correctness.
func fundChanAndCloseFromImportedAccount(t *harnessTest, srcNode, destNode,
	signer *lntest.HarnessNode, account string,
	accountAddrType walletrpc.AddressType, utxoAmt, chanSize int64) {

	ctxb := context.Background()

	// Retrieve the current confirmed balance to make some assertions later
	// on.
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	balanceResp, err := srcNode.WalletBalance(ctxt, &lnrpc.WalletBalanceRequest{})
	require.NoError(t.t, err)
	require.Contains(t.t, balanceResp.AccountBalance, account)
	accountConfBalance := balanceResp.
		AccountBalance[account].ConfirmedBalance
	defaultAccountConfBalance := balanceResp.
		AccountBalance[defaultAccount].ConfirmedBalance

	// Now, start the channel funding process. We'll need to connect both
	// nodes first.
	t.lndHarness.EnsureConnected(t.t, srcNode, destNode)

	// The source node will then fund the channel through a PSBT shim.
	var pendingChanID [32]byte
	_, err = rand.Read(pendingChanID[:])
	require.NoError(t.t, err)

	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	chanUpdates, rawPsbt, err := openChannelPsbt(
		ctxt, srcNode, destNode, lntest.OpenChannelParams{
			Amt: btcutil.Amount(chanSize),
			FundingShim: &lnrpc.FundingShim{
				Shim: &lnrpc.FundingShim_PsbtShim{
					PsbtShim: &lnrpc.PsbtShim{
						PendingChanId: pendingChanID[:],
					},
				},
			},
		},
	)
	require.NoError(t.t, err)

	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	fundReq := &walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_Psbt{
			Psbt: rawPsbt,
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: 1,
		},
		Account: account,
	}
	fundResp, err := srcNode.WalletKitClient.FundPsbt(ctxt, fundReq)
	require.NoError(t.t, err)

	_, err = srcNode.FundingStateStep(ctxb, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: pendingChanID[:],
				FundedPsbt:    fundResp.FundedPsbt,
			},
		},
	})
	require.NoError(t.t, err)

	// Now that we have a PSBT to fund the channel, our signer needs to sign
	// it.
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	finalizeReq := &walletrpc.FinalizePsbtRequest{
		FundedPsbt: fundResp.FundedPsbt,
	}
	finalizeResp, err := signer.WalletKitClient.FinalizePsbt(ctxt, finalizeReq)
	require.NoError(t.t, err)

	// The source node can then submit the signed PSBT and complete the
	// channel funding process.
	_, err = srcNode.FundingStateStep(ctxb, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtFinalize{
			PsbtFinalize: &lnrpc.FundingPsbtFinalize{
				PendingChanId: pendingChanID[:],
				SignedPsbt:    finalizeResp.SignedPsbt,
			},
		},
	})
	require.NoError(t.t, err)

	// We should receive a notification of the channel funding transaction
	// being broadcast.
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	updateResp, err := receiveChanUpdate(ctxt, chanUpdates)
	require.NoError(t.t, err)
	upd, ok := updateResp.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.True(t.t, ok)

	// Mine enough blocks to announce the channel to the network, making
	// balance assertions along the way.
	var (
		expChanTxFee        int64
		expChangeScriptType txscript.ScriptClass
	)
	switch accountAddrType {
	case walletrpc.AddressType_WITNESS_PUBKEY_HASH:
		expChanTxFee = 153
		expChangeScriptType = txscript.WitnessV0PubKeyHashTy

	case walletrpc.AddressType_NESTED_WITNESS_PUBKEY_HASH:
		if account != defaultImportedAccount {
			expChanTxFee = 177
			expChangeScriptType = txscript.ScriptHashTy
			break
		}

		// Spends from the default NP2WKH imported account have the same
		// fee rate as the hybrid address type since a NP2WKH input is
		// spent and a P2WKH change output is created.
		fallthrough

	case walletrpc.AddressType_HYBRID_NESTED_WITNESS_PUBKEY_HASH:
		expChanTxFee = 176
		expChangeScriptType = txscript.WitnessV0PubKeyHashTy

	default:
		t.Fatalf("unsupported addr type %v", accountAddrType)
	}
	chanChangeUtxoAmt := utxoAmt - chanSize - expChanTxFee
	txHash, err := chainhash.NewHash(upd.ChanPending.Txid)
	require.NoError(t.t, err)

	// If we're spending from the default imported account, then any change
	// outputs produced are moved to the default wallet account, so we
	// should expect to see balances there.
	var confBalanceAfterChan int64
	if account == defaultImportedAccount {
		confBalanceAfterChan = defaultAccountConfBalance
		assertAccountBalance(t.t, srcNode, account, 0, 0)
		assertAccountBalance(
			t.t, srcNode, defaultAccount, defaultAccountConfBalance,
			chanChangeUtxoAmt,
		)

		block := mineBlocks(t, t.lndHarness, 6, 1)[0]
		assertTxInBlock(t, block, txHash)

		confBalanceAfterChan += chanChangeUtxoAmt
		assertAccountBalance(t.t, srcNode, account, 0, 0)
		assertAccountBalance(
			t.t, srcNode, defaultAccount, confBalanceAfterChan, 0,
		)
	} else {
		// Otherwise, all interactions remain within Carol's imported
		// account.
		confBalanceAfterChan = accountConfBalance - utxoAmt
		assertAccountBalance(
			t.t, srcNode, account, confBalanceAfterChan,
			chanChangeUtxoAmt,
		)

		block := mineBlocks(t, t.lndHarness, 6, 1)[0]
		assertTxInBlock(t, block, txHash)

		confBalanceAfterChan += chanChangeUtxoAmt
		assertAccountBalance(
			t.t, srcNode, account, confBalanceAfterChan, 0,
		)
	}

	// Assert that the transaction has the expected change address type
	// based on the account.
	var tx wire.MsgTx
	err = tx.Deserialize(bytes.NewReader(finalizeResp.RawFinalTx))
	require.NoError(t.t, err)
	assertOutputScriptType(t.t, expChangeScriptType, &tx, chanChangeUtxoAmt)

	// Wait for the channel to be announced by both parties.
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: upd.ChanPending.Txid,
		},
		OutputIndex: upd.ChanPending.OutputIndex,
	}
	err = srcNode.WaitForNetworkChannelOpen(chanPoint)
	require.NoError(t.t, err)
	err = destNode.WaitForNetworkChannelOpen(chanPoint)
	require.NoError(t.t, err)

	// Send a test payment to ensure the channel is operating as normal.
	const invoiceAmt = 100000
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	resp, err := destNode.AddInvoice(ctxt, &lnrpc.Invoice{
		Memo:  "psbt import chan",
		Value: invoiceAmt,
	})
	require.NoError(t.t, err)

	err = completePaymentRequests(
		srcNode, srcNode.RouterClient,
		[]string{resp.PaymentRequest}, true,
	)
	require.NoError(t.t, err)

	// Now that we've confirmed the opened channel works, we'll close it.
	closeChannelAndAssert(t, t.lndHarness, srcNode, chanPoint, false)

	// Since the channel still had funds left on the source node's side,
	// they must've been redeemed after the close. Without a pre-negotiated
	// close address, the funds will go into the source node's wallet
	// instead of the imported account.
	const chanCloseTxFee = 9050
	balanceFromClosedChan := chanSize - invoiceAmt - chanCloseTxFee

	if account == defaultImportedAccount {
		assertAccountBalance(t.t, srcNode, account, 0, 0)
		assertAccountBalance(
			t.t, srcNode, defaultAccount,
			confBalanceAfterChan+balanceFromClosedChan, 0,
		)
	} else {
		assertAccountBalance(
			t.t, srcNode, account, confBalanceAfterChan, 0,
		)
		assertAccountBalance(
			t.t, srcNode, defaultAccount, balanceFromClosedChan, 0,
		)
	}
}

// testWalletImportAccount tests that an imported account can fund transactions
// and channels through PSBTs, by having one node (the one with the imported
// account) craft the transactions and another node act as the signer.
func testWalletImportAccount(net *lntest.NetworkHarness, t *harnessTest) {
	testCases := []struct {
		name     string
		addrType walletrpc.AddressType
	}{
		{
			name:     "standard BIP-0049",
			addrType: walletrpc.AddressType_NESTED_WITNESS_PUBKEY_HASH,
		},
		{
			name:     "lnd BIP-0049 variant",
			addrType: walletrpc.AddressType_HYBRID_NESTED_WITNESS_PUBKEY_HASH,
		},
		{
			name:     "standard BIP-0084",
			addrType: walletrpc.AddressType_WITNESS_PUBKEY_HASH,
		},
	}

	for _, tc := range testCases {
		tc := tc
		success := t.t.Run(tc.name, func(tt *testing.T) {
			ht := newHarnessTest(tt, net)
			ht.RunTestCase(&testCase{
				name: tc.name,
				test: func(net1 *lntest.NetworkHarness, t1 *harnessTest) {
					testWalletImportAccountScenario(
						net, t, tc.addrType,
					)
				},
			})
		})
		if !success {
			// Log failure time to help relate the lnd logs to the
			// failure.
			t.Logf("Failure time: %v", time.Now().Format(
				"2006-01-02 15:04:05.000",
			))
			break
		}
	}
}

func testWalletImportAccountScenario(net *lntest.NetworkHarness, t *harnessTest,
	addrType walletrpc.AddressType) {

	// We'll start our test by having two nodes, Carol and Dave. Carol's
	// default wallet account will be imported into Dave's node.
	carol := net.NewNode(t.t, "carol", nil)
	defer shutdownAndAssert(net, t, carol)

	dave := net.NewNode(t.t, "dave", nil)
	defer shutdownAndAssert(net, t, dave)

	runWalletImportAccountScenario(net, t, addrType, carol, dave)
}

func runWalletImportAccountScenario(net *lntest.NetworkHarness, t *harnessTest,
	addrType walletrpc.AddressType, carol, dave *lntest.HarnessNode) {

	ctxb := context.Background()
	const utxoAmt int64 = btcutil.SatoshiPerBitcoin

	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	listReq := &walletrpc.ListAccountsRequest{
		Name:        "default",
		AddressType: addrType,
	}
	listResp, err := carol.WalletKitClient.ListAccounts(ctxt, listReq)
	require.NoError(t.t, err)
	require.Equal(t.t, len(listResp.Accounts), 1)
	carolAccount := listResp.Accounts[0]

	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	const importedAccount = "carol"
	importReq := &walletrpc.ImportAccountRequest{
		Name:              importedAccount,
		ExtendedPublicKey: carolAccount.ExtendedPublicKey,
		AddressType:       addrType,
	}
	_, err = dave.WalletKitClient.ImportAccount(ctxt, importReq)
	require.NoError(t.t, err)

	// We'll generate an address for Carol from Dave's node to receive some
	// funds.
	externalAddr := newExternalAddr(
		t.t, dave, carol, importedAccount, addrType,
	)

	// Send coins to Carol's address and confirm them, making sure the
	// balance updates accordingly.
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	_, err = net.Alice.SendCoins(ctxt, &lnrpc.SendCoinsRequest{
		Addr:       externalAddr,
		Amount:     utxoAmt,
		SatPerByte: 1,
	})
	require.NoError(t.t, err)

	assertAccountBalance(t.t, dave, importedAccount, 0, utxoAmt)
	_ = mineBlocks(t, net, 1, 1)
	assertAccountBalance(t.t, dave, importedAccount, utxoAmt, 0)

	// To ensure that Dave can use Carol's account as watch-only, we'll
	// construct a PSBT that sends funds to Alice, which we'll then hand
	// over to Carol to sign.
	psbtSendFromImportedAccount(
		t, dave, net.Alice, carol, importedAccount, addrType,
	)

	// We'll generate a new address for Carol from Dave's node to receive
	// and fund a new channel.
	externalAddr = newExternalAddr(
		t.t, dave, carol, importedAccount, addrType,
	)

	// Retrieve the current confirmed balance of the imported account for
	// some assertions we'll make later on.
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	balanceResp, err := dave.WalletBalance(ctxt, &lnrpc.WalletBalanceRequest{})
	require.NoError(t.t, err)
	require.Contains(t.t, balanceResp.AccountBalance, importedAccount)
	confBalance := balanceResp.AccountBalance[importedAccount].ConfirmedBalance

	// Send coins to Carol's address and confirm them, making sure the
	// balance updates accordingly.
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	_, err = net.Alice.SendCoins(ctxt, &lnrpc.SendCoinsRequest{
		Addr:       externalAddr,
		Amount:     utxoAmt,
		SatPerByte: 1,
	})
	require.NoError(t.t, err)

	assertAccountBalance(t.t, dave, importedAccount, confBalance, utxoAmt)
	_ = mineBlocks(t, net, 1, 1)
	assertAccountBalance(
		t.t, dave, importedAccount, confBalance+utxoAmt, 0,
	)

	// Now that we have enough funds, it's time to fund the channel, make a
	// test payment, and close it. This contains several balance assertions
	// along the way.
	fundChanAndCloseFromImportedAccount(
		t, dave, net.Alice, carol, importedAccount, addrType, utxoAmt,
		int64(funding.MaxBtcFundingAmount),
	)
}

// testWalletImportPubKey tests that an imported public keys can fund
// transactions and channels through PSBTs, by having one node (the one with the
// imported account) craft the transactions and another node act as the signer.
func testWalletImportPubKey(net *lntest.NetworkHarness, t *harnessTest) {
	testCases := []struct {
		name     string
		addrType walletrpc.AddressType
	}{
		{
			name:     "BIP-0049",
			addrType: walletrpc.AddressType_NESTED_WITNESS_PUBKEY_HASH,
		},
		{
			name:     "BIP-0084",
			addrType: walletrpc.AddressType_WITNESS_PUBKEY_HASH,
		},
	}

	for _, tc := range testCases {
		tc := tc
		success := t.t.Run(tc.name, func(tt *testing.T) {
			ht := newHarnessTest(tt, net)
			ht.RunTestCase(&testCase{
				name: tc.name,
				test: func(net1 *lntest.NetworkHarness, t1 *harnessTest) {
					testWalletImportPubKeyScenario(
						net, t, tc.addrType,
					)
				},
			})
		})
		if !success {
			// Log failure time to help relate the lnd logs to the
			// failure.
			t.Logf("Failure time: %v", time.Now().Format(
				"2006-01-02 15:04:05.000",
			))
			break
		}
	}
}

func testWalletImportPubKeyScenario(net *lntest.NetworkHarness, t *harnessTest,
	addrType walletrpc.AddressType) {

	ctxb := context.Background()
	const utxoAmt int64 = btcutil.SatoshiPerBitcoin

	// We'll start our test by having two nodes, Carol and Dave.
	carol := net.NewNode(t.t, "carol", nil)
	defer shutdownAndAssert(net, t, carol)

	dave := net.NewNode(t.t, "dave", nil)
	defer shutdownAndAssert(net, t, dave)

	// We'll define a helper closure that we'll use throughout the test to
	// generate a new address of the given type from Carol's perspective,
	// import it into Dave's wallet, and fund it.
	importPubKey := func(keyIndex uint32, prevConfBalance, prevUnconfBalance int64) {
		// Retrieve Carol's account public key for the corresponding
		// address type.
		ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
		defer cancel()
		listReq := &walletrpc.ListAccountsRequest{
			Name:        "default",
			AddressType: addrType,
		}
		listResp, err := carol.WalletKitClient.ListAccounts(ctxt, listReq)
		require.NoError(t.t, err)
		require.Equal(t.t, len(listResp.Accounts), 1)
		p2wkhAccount := listResp.Accounts[0]

		// Derive the external address at the given index.
		accountPubKey, err := hdkeychain.NewKeyFromString(
			p2wkhAccount.ExtendedPublicKey,
		)
		require.NoError(t.t, err)
		externalAccountExtKey, err := accountPubKey.Derive(0)
		require.NoError(t.t, err)
		externalAddrExtKey, err := externalAccountExtKey.Derive(keyIndex)
		require.NoError(t.t, err)
		externalAddrPubKey, err := externalAddrExtKey.ECPubKey()
		require.NoError(t.t, err)

		// Import the public key into Dave.
		ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
		defer cancel()
		importReq := &walletrpc.ImportPublicKeyRequest{
			PublicKey:   externalAddrPubKey.SerializeCompressed(),
			AddressType: addrType,
		}
		_, err = dave.WalletKitClient.ImportPublicKey(ctxt, importReq)
		require.NoError(t.t, err)

		// We'll also generate the same address for Carol, as it'll be
		// required later when signing.
		ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
		defer cancel()
		carolAddrResp, err := carol.NewAddress(ctxt, &lnrpc.NewAddressRequest{
			Type: walletToLNAddrType(t.t, addrType),
		})
		require.NoError(t.t, err)

		// Send coins to Carol's address and confirm them, making sure
		// the balance updates accordingly.
		ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
		defer cancel()
		_, err = net.Alice.SendCoins(ctxt, &lnrpc.SendCoinsRequest{
			Addr:       carolAddrResp.Address,
			Amount:     utxoAmt,
			SatPerByte: 1,
		})
		require.NoError(t.t, err)

		assertAccountBalance(
			t.t, dave, defaultImportedAccount, prevConfBalance,
			prevUnconfBalance+utxoAmt,
		)
		_ = mineBlocks(t, net, 1, 1)
		assertAccountBalance(
			t.t, dave, defaultImportedAccount,
			prevConfBalance+utxoAmt, prevUnconfBalance,
		)
	}

	// We'll have Carol generate a new external address, which we'll import
	// into Dave.
	importPubKey(0, 0, 0)

	// To ensure that Dave can use Carol's public key as watch-only, we'll
	// construct a PSBT that sends funds to Alice, which we'll then hand
	// over to Carol to sign.
	psbtSendFromImportedAccount(
		t, dave, net.Alice, carol, defaultImportedAccount, addrType,
	)

	// We'll now attempt to fund a channel.
	//
	// We'll have Carol generate another external address, which we'll
	// import into Dave again.
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	balanceResp, err := dave.WalletBalance(ctxt, &lnrpc.WalletBalanceRequest{})
	require.NoError(t.t, err)
	require.Contains(t.t, balanceResp.AccountBalance, defaultImportedAccount)
	confBalance := balanceResp.
		AccountBalance[defaultImportedAccount].ConfirmedBalance
	importPubKey(1, confBalance, 0)

	// Now that we have enough funds, it's time to fund the channel, make a
	// test payment, and close it. This contains several balance assertions
	// along the way.
	fundChanAndCloseFromImportedAccount(
		t, dave, net.Alice, carol, defaultImportedAccount, addrType,
		utxoAmt, int64(funding.MaxBtcFundingAmount),
	)
}
