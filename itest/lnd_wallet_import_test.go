package itest

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// walletImportAccountTestCases tests that an imported account can fund
// transactions and channels through PSBTs, by having one node (the one with
// the imported account) craft the transactions and another node act as the
// signer.
//
//nolint:ll
var walletImportAccountTestCases = []*lntest.TestCase{
	{
		Name: "standard BIP-0049",
		TestFunc: func(ht *lntest.HarnessTest) {
			testWalletImportAccountScenario(
				ht, walletrpc.AddressType_NESTED_WITNESS_PUBKEY_HASH,
			)
		},
	},
	{
		Name: "lnd BIP-0049 variant",
		TestFunc: func(ht *lntest.HarnessTest) {
			testWalletImportAccountScenario(
				ht, walletrpc.AddressType_HYBRID_NESTED_WITNESS_PUBKEY_HASH,
			)
		},
	},
	{
		Name: "standard BIP-0084",
		TestFunc: func(ht *lntest.HarnessTest) {
			testWalletImportAccountScenario(
				ht, walletrpc.AddressType_WITNESS_PUBKEY_HASH,
			)
		},
	},
	{
		Name: "standard BIP-0086",
		TestFunc: func(ht *lntest.HarnessTest) {
			testWalletImportAccountScenario(
				ht, walletrpc.AddressType_TAPROOT_PUBKEY,
			)
		},
	},
}

const (
	defaultAccount         = lnwallet.DefaultAccountName
	defaultImportedAccount = waddrmgr.ImportedAddrAccountName
)

// walletToLNAddrType maps walletrpc.AddressType to lnrpc.AddressType.
func walletToLNAddrType(t *testing.T,
	addrType walletrpc.AddressType) lnrpc.AddressType {

	switch addrType {
	case walletrpc.AddressType_NESTED_WITNESS_PUBKEY_HASH,
		walletrpc.AddressType_HYBRID_NESTED_WITNESS_PUBKEY_HASH:

		return lnrpc.AddressType_NESTED_PUBKEY_HASH

	case walletrpc.AddressType_WITNESS_PUBKEY_HASH:
		return lnrpc.AddressType_WITNESS_PUBKEY_HASH

	case walletrpc.AddressType_TAPROOT_PUBKEY:
		return lnrpc.AddressType_TAPROOT_PUBKEY

	default:
		t.Fatalf("unhandled addr type %v", addrType)
		return 0
	}
}

// newExternalAddr generates a new external address of an imported account for a
// pair of nodes, where one acts as the funder and the other as the signer.
func newExternalAddr(ht *lntest.HarnessTest, funder, signer *node.HarnessNode,
	importedAccount string, addrType walletrpc.AddressType) string {

	// We'll generate a new address for Carol from Dave's node to receive
	// and fund a new channel.
	req := &lnrpc.NewAddressRequest{
		Type:    walletToLNAddrType(ht.T, addrType),
		Account: importedAccount,
	}
	funderResp := funder.RPC.NewAddress(req)

	// Carol also needs to generate the address for the sake of this test
	// to be able to sign the channel funding input.
	req = &lnrpc.NewAddressRequest{
		Type: walletToLNAddrType(ht.T, addrType),
	}
	signerResp := signer.RPC.NewAddress(req)

	// Sanity check that the generated addresses match.
	require.Equal(ht, funderResp.Address, signerResp.Address)
	assertExternalAddrType(ht.T, funderResp.Address, addrType)

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

	case walletrpc.AddressType_TAPROOT_PUBKEY:
		require.IsType(t, addr, &btcutil.AddressTaproot{})

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

// psbtSendFromImportedAccount attempts to fund a PSBT from the given imported
// account, originating from the source node to the destination.
func psbtSendFromImportedAccount(ht *lntest.HarnessTest, srcNode, destNode,
	signer *node.HarnessNode, account string,
	accountAddrType walletrpc.AddressType) {

	balanceResp := srcNode.RPC.WalletBalance()
	require.Contains(ht, balanceResp.AccountBalance, account)
	confBalance := balanceResp.AccountBalance[account].ConfirmedBalance

	destAmt := confBalance - 10000
	destAddrResp := destNode.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	})

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
	fundResp := srcNode.RPC.FundPsbt(fundReq)

	// Have Carol sign the PSBT input since Dave doesn't have any private
	// key information.
	finalizeReq := &walletrpc.FinalizePsbtRequest{
		FundedPsbt: fundResp.FundedPsbt,
	}
	finalizeResp := signer.RPC.FinalizePsbt(finalizeReq)

	// With the PSBT signed, we can broadcast the resulting transaction.
	publishReq := &walletrpc.Transaction{
		TxHex: finalizeResp.RawFinalTx,
	}
	srcNode.RPC.PublishTransaction(publishReq)

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

	case walletrpc.AddressType_TAPROOT_PUBKEY:
		if account != defaultImportedAccount {
			expTxFee = 143
			expChangeScriptType = txscript.WitnessV1TaprootTy
			break
		}

		// Spends from the default imported account fall back to a P2WKH
		// change. We'll want to change that, but in a separate PR.
		expTxFee = 131
		expChangeScriptType = txscript.WitnessV0PubKeyHashTy

	default:
		ht.Fatalf("unsupported addr type %v", accountAddrType)
	}
	changeUtxoAmt := confBalance - destAmt - expTxFee

	// If the transaction was created from the default imported account,
	// then any change produced is moved to the default wallet account.
	accountWithBalance := account
	if account == defaultImportedAccount {
		accountWithBalance = defaultAccount
	}
	ht.AssertWalletAccountBalance(
		srcNode, accountWithBalance, 0, changeUtxoAmt,
	)
	ht.MineBlocksAndAssertNumTxes(1, 1)
	ht.AssertWalletAccountBalance(
		srcNode, accountWithBalance, changeUtxoAmt, 0,
	)

	// Finally, assert that the transaction has the expected change address
	// type based on the account.
	var tx wire.MsgTx
	err := tx.Deserialize(bytes.NewReader(finalizeResp.RawFinalTx))
	require.NoError(ht, err)
	assertOutputScriptType(ht.T, expChangeScriptType, &tx, changeUtxoAmt)
}

// fundChanAndCloseFromImportedAccount attempts to a fund a channel from the
// given imported account, originating from the source node to the destination
// node. To ensure the channel is operational before closing it, a test payment
// is made. Several balance assertions are made along the way for the sake of
// correctness.
func fundChanAndCloseFromImportedAccount(ht *lntest.HarnessTest, srcNode,
	destNode, signer *node.HarnessNode, account string,
	accountAddrType walletrpc.AddressType, utxoAmt, chanSize int64) {

	// Retrieve the current confirmed balance to make some assertions later
	// on.
	balanceResp := srcNode.RPC.WalletBalance()
	require.Contains(ht, balanceResp.AccountBalance, account)
	accountConfBalance := balanceResp.
		AccountBalance[account].ConfirmedBalance
	defaultAccountConfBalance := balanceResp.
		AccountBalance[defaultAccount].ConfirmedBalance

	// Now, start the channel funding process. We'll need to connect both
	// nodes first.
	ht.EnsureConnected(srcNode, destNode)

	// The source node will then fund the channel through a PSBT shim.
	pendingChanID := ht.Random32Bytes()
	chanUpdates, rawPsbt := ht.OpenChannelPsbt(
		srcNode, destNode, lntest.OpenChannelParams{
			Amt: btcutil.Amount(chanSize),
			FundingShim: &lnrpc.FundingShim{
				Shim: &lnrpc.FundingShim_PsbtShim{
					PsbtShim: &lnrpc.PsbtShim{
						PendingChanId: pendingChanID,
					},
				},
			},
		},
	)

	fundReq := &walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_Psbt{
			Psbt: rawPsbt,
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: 1,
		},
		Account: account,
	}
	fundResp := srcNode.RPC.FundPsbt(fundReq)

	srcNode.RPC.FundingStateStep(&lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				PendingChanId: pendingChanID,
				FundedPsbt:    fundResp.FundedPsbt,
			},
		},
	})

	// Now that we have a PSBT to fund the channel, our signer needs to sign
	// it.
	finalizeReq := &walletrpc.FinalizePsbtRequest{
		FundedPsbt: fundResp.FundedPsbt,
	}
	finalizeResp := signer.RPC.FinalizePsbt(finalizeReq)

	// The source node can then submit the signed PSBT and complete the
	// channel funding process.
	srcNode.RPC.FundingStateStep(&lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtFinalize{
			PsbtFinalize: &lnrpc.FundingPsbtFinalize{
				PendingChanId: pendingChanID,
				SignedPsbt:    finalizeResp.SignedPsbt,
			},
		},
	})

	// We should receive a notification of the channel funding transaction
	// being broadcast.
	updateResp := ht.ReceiveOpenChannelUpdate(chanUpdates)
	upd, ok := updateResp.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
	require.True(ht, ok)

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

	case walletrpc.AddressType_TAPROOT_PUBKEY:
		if account != defaultImportedAccount {
			expChanTxFee = 155
			expChangeScriptType = txscript.WitnessV1TaprootTy
			break
		}

		// Spends from the default imported account fall back to a P2WKH
		// change. We'll want to change that, but in a separate PR.
		expChanTxFee = 143
		expChangeScriptType = txscript.WitnessV0PubKeyHashTy

	default:
		ht.Fatalf("unsupported addr type %v", accountAddrType)
	}
	chanChangeUtxoAmt := utxoAmt - chanSize - expChanTxFee
	txHash, err := chainhash.NewHash(upd.ChanPending.Txid)
	require.NoError(ht, err)

	// If we're spending from the default imported account, then any change
	// outputs produced are moved to the default wallet account, so we
	// should expect to see balances there.
	var confBalanceAfterChan int64
	if account == defaultImportedAccount {
		confBalanceAfterChan = defaultAccountConfBalance
		ht.AssertWalletAccountBalance(srcNode, account, 0, 0)
		ht.AssertWalletAccountBalance(
			srcNode, defaultAccount, defaultAccountConfBalance,
			chanChangeUtxoAmt,
		)

		block := ht.MineBlocksAndAssertNumTxes(6, 1)[0]
		ht.AssertTxInBlock(block, *txHash)

		confBalanceAfterChan += chanChangeUtxoAmt
		ht.AssertWalletAccountBalance(srcNode, account, 0, 0)
		ht.AssertWalletAccountBalance(
			srcNode, defaultAccount, confBalanceAfterChan, 0,
		)
	} else {
		// Otherwise, all interactions remain within Carol's imported
		// account.
		confBalanceAfterChan = accountConfBalance - utxoAmt
		ht.AssertWalletAccountBalance(
			srcNode, account, confBalanceAfterChan,
			chanChangeUtxoAmt,
		)

		block := ht.MineBlocksAndAssertNumTxes(6, 1)[0]
		ht.AssertTxInBlock(block, *txHash)

		confBalanceAfterChan += chanChangeUtxoAmt
		ht.AssertWalletAccountBalance(
			srcNode, account, confBalanceAfterChan, 0,
		)
	}

	// Assert that the transaction has the expected change address type
	// based on the account.
	var tx wire.MsgTx
	err = tx.Deserialize(bytes.NewReader(finalizeResp.RawFinalTx))
	require.NoError(ht, err)
	assertOutputScriptType(
		ht.T, expChangeScriptType, &tx, chanChangeUtxoAmt,
	)

	// Wait for the channel to be announced by both parties.
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: upd.ChanPending.Txid,
		},
		OutputIndex: upd.ChanPending.OutputIndex,
	}
	ht.AssertChannelInGraph(srcNode, chanPoint)
	ht.AssertChannelInGraph(destNode, chanPoint)

	// Send a test payment to ensure the channel is operating as normal.
	const invoiceAmt = 100000
	invoice := &lnrpc.Invoice{
		Memo:  "psbt import chan",
		Value: invoiceAmt,
	}
	resp := destNode.RPC.AddInvoice(invoice)

	ht.CompletePaymentRequests(srcNode, []string{resp.PaymentRequest})

	// Now that we've confirmed the opened channel works, we'll close it.
	ht.CloseChannel(srcNode, chanPoint)

	// Since the channel still had funds left on the source node's side,
	// they must've been redeemed after the close. Without a pre-negotiated
	// close address, the funds will go into the source node's wallet
	// instead of the imported account.
	const chanCloseTxFee = 9650
	balanceFromClosedChan := chanSize - invoiceAmt - chanCloseTxFee

	if account == defaultImportedAccount {
		ht.AssertWalletAccountBalance(srcNode, account, 0, 0)
		ht.AssertWalletAccountBalance(
			srcNode, defaultAccount,
			confBalanceAfterChan+balanceFromClosedChan, 0,
		)
	} else {
		ht.AssertWalletAccountBalance(
			srcNode, account, confBalanceAfterChan, 0,
		)
		ht.AssertWalletAccountBalance(
			srcNode, defaultAccount, balanceFromClosedChan, 0,
		)
	}
}

func testWalletImportAccountScenario(ht *lntest.HarnessTest,
	addrType walletrpc.AddressType) {

	// We'll start our test by having two nodes, Carol and Dave. Carol's
	// default wallet account will be imported into Dave's node.
	//
	// NOTE: we won't use standby nodes here since the test will change
	// each of the node's wallet state.
	carol := ht.NewNode("carol", nil)
	dave := ht.NewNode("dave", nil)

	runWalletImportAccountScenario(ht, addrType, carol, dave)
}

func runWalletImportAccountScenario(ht *lntest.HarnessTest,
	addrType walletrpc.AddressType, carol, dave *node.HarnessNode) {

	const utxoAmt int64 = btcutil.SatoshiPerBitcoin

	listReq := &walletrpc.ListAccountsRequest{
		Name:        "default",
		AddressType: addrType,
	}
	listResp := carol.RPC.ListAccounts(listReq)
	require.Len(ht, listResp.Accounts, 1)
	carolAccount := listResp.Accounts[0]

	const importedAccount = "carol"
	importReq := &walletrpc.ImportAccountRequest{
		Name:              importedAccount,
		ExtendedPublicKey: carolAccount.ExtendedPublicKey,
		AddressType:       addrType,
	}
	dave.RPC.ImportAccount(importReq)

	// Try to import an account with the same name but with a different
	// key scope. It should return an error.
	otherAddrType := walletrpc.AddressType_TAPROOT_PUBKEY
	if addrType == walletrpc.AddressType_TAPROOT_PUBKEY {
		otherAddrType--
	}

	listReq = &walletrpc.ListAccountsRequest{
		Name:        "default",
		AddressType: otherAddrType,
	}
	listResp = carol.RPC.ListAccounts(listReq)
	require.Len(ht, listResp.Accounts, 1)

	carolAccountOtherAddrType := listResp.Accounts[0]

	errAccountExists := fmt.Sprintf(
		"account '%s' already exists", importedAccount,
	)

	importReq = &walletrpc.ImportAccountRequest{
		Name:              importedAccount,
		ExtendedPublicKey: carolAccountOtherAddrType.ExtendedPublicKey,
		AddressType:       otherAddrType,
	}
	err := dave.RPC.ImportAccountAssertErr(importReq)
	require.ErrorContains(ht, err, errAccountExists)

	// We'll generate an address for Carol from Dave's node to receive some
	// funds.
	externalAddr := newExternalAddr(
		ht, dave, carol, importedAccount, addrType,
	)

	// Send coins to Carol's address and confirm them, making sure the
	// balance updates accordingly.
	alice := ht.NewNodeWithCoins("Alice", nil)
	req := &lnrpc.SendCoinsRequest{
		Addr:       externalAddr,
		Amount:     utxoAmt,
		SatPerByte: 1,
	}
	alice.RPC.SendCoins(req)

	ht.AssertWalletAccountBalance(dave, importedAccount, 0, utxoAmt)
	ht.MineBlocksAndAssertNumTxes(1, 1)
	ht.AssertWalletAccountBalance(dave, importedAccount, utxoAmt, 0)

	// To ensure that Dave can use Carol's account as watch-only, we'll
	// construct a PSBT that sends funds to Alice, which we'll then hand
	// over to Carol to sign.
	psbtSendFromImportedAccount(
		ht, dave, alice, carol, importedAccount, addrType,
	)

	// We'll generate a new address for Carol from Dave's node to receive
	// and fund a new channel.
	externalAddr = newExternalAddr(
		ht, dave, carol, importedAccount, addrType,
	)

	// Retrieve the current confirmed balance of the imported account for
	// some assertions we'll make later on.
	balanceResp := dave.RPC.WalletBalance()
	require.Contains(ht, balanceResp.AccountBalance, importedAccount)
	confBalance := balanceResp.AccountBalance[importedAccount].
		ConfirmedBalance

	// Send coins to Carol's address and confirm them, making sure the
	// balance updates accordingly.
	req = &lnrpc.SendCoinsRequest{
		Addr:       externalAddr,
		Amount:     utxoAmt,
		SatPerByte: 1,
	}
	alice.RPC.SendCoins(req)

	ht.AssertWalletAccountBalance(
		dave, importedAccount, confBalance, utxoAmt,
	)
	ht.MineBlocksAndAssertNumTxes(1, 1)
	ht.AssertWalletAccountBalance(
		dave, importedAccount, confBalance+utxoAmt, 0,
	)

	// Now that we have enough funds, it's time to fund the channel, make a
	// test payment, and close it. This contains several balance assertions
	// along the way.
	fundChanAndCloseFromImportedAccount(
		ht, dave, alice, carol, importedAccount, addrType, utxoAmt,
		int64(funding.MaxBtcFundingAmount),
	)
}

// testWalletImportPubKey tests that an imported public keys can fund
// transactions and channels through PSBTs, by having one node (the one with the
// imported account) craft the transactions and another node act as the signer.
func testWalletImportPubKey(ht *lntest.HarnessTest) {
	testCases := []struct {
		name     string
		addrType walletrpc.AddressType
	}{
		{
			name: "BIP-0049",
			addrType: walletrpc.
				AddressType_NESTED_WITNESS_PUBKEY_HASH,
		},
		{
			name:     "BIP-0084",
			addrType: walletrpc.AddressType_WITNESS_PUBKEY_HASH,
		},
		{
			name:     "BIP-0086",
			addrType: walletrpc.AddressType_TAPROOT_PUBKEY,
		},
	}

	for _, tc := range testCases {
		tc := tc
		success := ht.Run(tc.name, func(tt *testing.T) {
			testFunc := func(ht *lntest.HarnessTest) {
				testWalletImportPubKeyScenario(
					ht, tc.addrType,
				)
			}

			st := ht.Subtest(tt)

			st.RunTestCase(&lntest.TestCase{
				Name:     tc.name,
				TestFunc: testFunc,
			})
		})
		if !success {
			// Log failure time to help relate the lnd logs to the
			// failure.
			ht.Logf("Failure time: %v", time.Now().Format(
				"2006-01-02 15:04:05.000",
			))
			break
		}
	}
}

func testWalletImportPubKeyScenario(ht *lntest.HarnessTest,
	addrType walletrpc.AddressType) {

	const utxoAmt int64 = btcutil.SatoshiPerBitcoin
	alice := ht.NewNodeWithCoins("Alice", nil)

	// We'll start our test by having two nodes, Carol and Dave.
	//
	// NOTE: we won't use standby nodes here since the test will change
	// each of the node's wallet state.
	carol := ht.NewNode("carol", nil)
	dave := ht.NewNode("dave", nil)

	// We'll define a helper closure that we'll use throughout the test to
	// generate a new address of the given type from Carol's perspective,
	// import it into Dave's wallet, and fund it.
	importPubKey := func(keyIndex uint32, prevConfBalance,
		prevUnconfBalance int64) {

		// Retrieve Carol's account public key for the corresponding
		// address type.
		listReq := &walletrpc.ListAccountsRequest{
			Name:        "default",
			AddressType: addrType,
		}
		listResp := carol.RPC.ListAccounts(listReq)
		require.Len(ht, listResp.Accounts, 1)
		p2wkhAccount := listResp.Accounts[0]

		// Derive the external address at the given index.
		accountPubKey, err := hdkeychain.NewKeyFromString(
			p2wkhAccount.ExtendedPublicKey,
		)
		require.NoError(ht, err)
		externalAccountExtKey, err := accountPubKey.Derive(0)
		require.NoError(ht, err)
		externalAddrExtKey, err := externalAccountExtKey.Derive(
			keyIndex,
		)
		require.NoError(ht, err)
		externalAddrPubKey, err := externalAddrExtKey.ECPubKey()
		require.NoError(ht, err)

		// Serialize as 32-byte x-only pubkey for Taproot addresses.
		serializedPubKey := externalAddrPubKey.SerializeCompressed()
		if addrType == walletrpc.AddressType_TAPROOT_PUBKEY {
			serializedPubKey = schnorr.SerializePubKey(
				externalAddrPubKey,
			)
		}

		// Import the public key into Dave.
		importReq := &walletrpc.ImportPublicKeyRequest{
			PublicKey:   serializedPubKey,
			AddressType: addrType,
		}
		dave.RPC.ImportPublicKey(importReq)

		// We'll also generate the same address for Carol, as it'll be
		// required later when signing.
		carolAddrResp := carol.RPC.NewAddress(&lnrpc.NewAddressRequest{
			Type: walletToLNAddrType(ht.T, addrType),
		})

		// Send coins to Carol's address and confirm them, making sure
		// the balance updates accordingly.
		req := &lnrpc.SendCoinsRequest{
			Addr:       carolAddrResp.Address,
			Amount:     utxoAmt,
			SatPerByte: 1,
		}
		alice.RPC.SendCoins(req)

		ht.AssertWalletAccountBalance(
			dave, defaultImportedAccount, prevConfBalance,
			prevUnconfBalance+utxoAmt,
		)
		ht.MineBlocksAndAssertNumTxes(1, 1)
		ht.AssertWalletAccountBalance(
			dave, defaultImportedAccount,
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
		ht, dave, alice, carol, defaultImportedAccount, addrType,
	)

	// We'll now attempt to fund a channel.
	//
	// We'll have Carol generate another external address, which we'll
	// import into Dave again.
	balanceResp := dave.RPC.WalletBalance()
	require.Contains(ht, balanceResp.AccountBalance, defaultImportedAccount)
	confBalance := balanceResp.
		AccountBalance[defaultImportedAccount].ConfirmedBalance
	importPubKey(1, confBalance, 0)

	// Now that we have enough funds, it's time to fund the channel, make a
	// test payment, and close it. This contains several balance assertions
	// along the way.
	fundChanAndCloseFromImportedAccount(
		ht, dave, alice, carol, defaultImportedAccount, addrType,
		utxoAmt, int64(funding.MaxBtcFundingAmount),
	)
}
