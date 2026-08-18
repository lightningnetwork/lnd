package itest

import (
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

const (
	// createAccountName is the account these tests create and spend from.
	createAccountName = "custom"

	// precedingAccountName is created first so the account under test is
	// not the first custom account in its key scope. Recovery has to
	// recreate accounts in the original order.
	precedingAccountName = "preceding"

	// defaultCreateAccountFeeRate is the sat/vB rate the miner uses when
	// funding the account under test.
	defaultCreateAccountFeeRate = btcutil.Amount(10)

	// maxCreateAccountSpendFee bounds what the account's own spend may
	// cost. The transaction is one input and two outputs at 5 sat/vB, so
	// a few thousand sats is a generous ceiling; the point is only to
	// distinguish "paid a fee" from "the money went somewhere else".
	maxCreateAccountSpendFee = btcutil.Amount(10_000)
)

// testXCreateAccount asserts the end-to-end behaviour of an account created
// from the wallet's own master key: it is not watch-only, its funds are
// reported against it rather than the default account, and — the property
// that distinguishes it from an imported account — the wallet can sign for
// it.
func testXCreateAccount(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", nil)

	account := alice.RPC.XCreateAccount(&walletrpc.XCreateAccountRequest{
		Name:        createAccountName,
		AddressType: walletrpc.AddressType_TAPROOT_PUBKEY,
	}).GetAccount()

	require.Equal(ht, createAccountName, account.GetName())
	require.Equal(
		ht, walletrpc.AddressType_TAPROOT_PUBKEY,
		account.GetAddressType(),
	)

	// The whole point of this RPC: unlike an imported account, the wallet
	// holds the keys, so it can spend what the account receives.
	require.False(ht, account.GetWatchOnly(), "account must be spendable")

	// It shows up in ListAccounts under the scope it was created in.
	listed := alice.RPC.ListAccounts(&walletrpc.ListAccountsRequest{
		Name:        createAccountName,
		AddressType: walletrpc.AddressType_TAPROOT_PUBKEY,
	}).GetAccounts()
	require.Len(ht, listed, 1)
	require.Equal(ht, account.GetExtendedPublicKey(),
		listed[0].GetExtendedPublicKey())

	// Fund an address belonging to the new account. The address type has
	// to match the one the account was created with: lnd resolves a custom
	// account name inside the key scope the requested type implies.
	addr := alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type:    lnrpc.AddressType_TAPROOT_PUBKEY,
		Account: createAccountName,
	}).GetAddress()

	const fundAmt = btcutil.Amount(500_000)
	ht.SendOutputsWithoutChange(
		[]*wire.TxOut{{
			Value:    int64(fundAmt),
			PkScript: ht.PayToAddrScript(ht.DecodeAddress(addr)),
		}}, defaultCreateAccountFeeRate,
	)
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// The balance lands in the new account, and nowhere else. Both halves
	// matter: the account must see its own coins, and the default account
	// must not see them.
	ht.AssertWalletAccountBalance(
		alice, createAccountName, int64(fundAmt), 0,
	)
	ht.AssertWalletAccountBalance(
		alice, lnwallet.DefaultAccountName, 0, 0,
	)

	// Now prove the wallet can actually spend it. An imported (watch-only)
	// account gets this far too — funding a PSBT only needs public data —
	// but finalizing is where it fails, because the wallet has no private
	// key for it and silently signs nothing.
	dest := alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type:    lnrpc.AddressType_TAPROOT_PUBKEY,
		Account: createAccountName,
	}).GetAddress()

	funded := alice.RPC.FundPsbt(&walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_Raw{
			Raw: &walletrpc.TxTemplate{
				Outputs: map[string]uint64{
					dest: uint64(fundAmt / 2),
				},
			},
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: 5,
		},
		Account: createAccountName,
	})

	finalized := alice.RPC.FinalizePsbt(&walletrpc.FinalizePsbtRequest{
		FundedPsbt: funded.GetFundedPsbt(),
		Account:    createAccountName,
	})
	require.NotEmpty(ht, finalized.GetRawFinalTx(),
		"wallet produced no signed transaction for its own account")

	alice.RPC.PublishTransaction(&walletrpc.Transaction{
		TxHex: finalized.GetRawFinalTx(),
	})
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// The spend confirmed, and both halves of where the money went matter.
	// The account still holds its funds minus fees, which is what shows
	// the inputs were spent from it and the change came back to it rather
	// than leaking elsewhere; and the default account is still empty,
	// which shows "elsewhere" was not it.
	accounts := alice.RPC.WalletBalance().GetAccountBalance()
	after := btcutil.Amount(
		accounts[createAccountName].GetConfirmedBalance(),
	)
	require.Less(ht, after, fundAmt, "the spend should have paid a fee")
	require.Greater(ht, after, fundAmt-maxCreateAccountSpendFee,
		"the account should still hold its funds minus fees")

	ht.AssertWalletAccountBalance(
		alice, lnwallet.DefaultAccountName, 0, 0,
	)
}

// testXCreateAccountRejections asserts the requests lnd refuses, each of which
// would otherwise leave the caller with an account that does not behave the
// way it asked for.
func testXCreateAccountRejections(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", nil)

	alice.RPC.XCreateAccount(&walletrpc.XCreateAccountRequest{
		Name:        createAccountName,
		AddressType: walletrpc.AddressType_TAPROOT_PUBKEY,
	})

	// The same name a second time, even under a different address type.
	// Coin selection resolves a custom account name to whichever key scope
	// matches first, so a duplicate would make later funding ambiguous.
	err := alice.RPC.XCreateAccountAssertErr(
		&walletrpc.XCreateAccountRequest{
			Name:        createAccountName,
			AddressType: walletrpc.AddressType_WITNESS_PUBKEY_HASH,
		},
	)
	require.ErrorContains(ht, err, "already exists")

	// The wallet's own reserved account names.
	err = alice.RPC.XCreateAccountAssertErr(
		&walletrpc.XCreateAccountRequest{
			Name:        lnwallet.DefaultAccountName,
			AddressType: walletrpc.AddressType_TAPROOT_PUBKEY,
		},
	)
	require.ErrorContains(ht, err, "reserved")

	// An empty name.
	err = alice.RPC.XCreateAccountAssertErr(
		&walletrpc.XCreateAccountRequest{
			AddressType: walletrpc.AddressType_TAPROOT_PUBKEY,
		},
	)
	require.ErrorContains(ht, err, "account name is required")

	// The strict nested-witness scheme, which a wallet-derived account
	// cannot provide: it stores no address schema, so it would silently
	// behave as the hybrid scheme instead.
	err = alice.RPC.XCreateAccountAssertErr(
		&walletrpc.XCreateAccountRequest{
			Name: "nested",
			AddressType: walletrpc.
				AddressType_NESTED_WITNESS_PUBKEY_HASH,
		},
	)
	require.ErrorContains(ht, err, "cannot be created")
}

// testXCreateAccountBranchRecovery asserts the manual recovery procedure for
// a wallet-derived account: both address branches must be replayed, because
// NextAddr defaults to the external branch and FundPsbt change lives on the
// internal one.
func testXCreateAccountBranchRecovery(ht *lntest.HarnessTest) {
	password := []byte("The Magic Words are Squeamish Ossifrage")
	alice, mnemonic, _ := ht.NewNodeWithSeed(
		"Alice", nil, password, false,
	)

	// A preceding account so the target is not index 1 of the BIP-0086
	// scope. Recovery has to recreate every account in that scope in
	// order for the index counter to land on the same value.
	alice.RPC.XCreateAccount(&walletrpc.XCreateAccountRequest{
		Name:        precedingAccountName,
		AddressType: walletrpc.AddressType_TAPROOT_PUBKEY,
	})

	account := alice.RPC.XCreateAccount(&walletrpc.XCreateAccountRequest{
		Name:        createAccountName,
		AddressType: walletrpc.AddressType_TAPROOT_PUBKEY,
	}).GetAccount()

	// Fund an external address belonging to the target account.
	extAddr := alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type:    lnrpc.AddressType_TAPROOT_PUBKEY,
		Account: createAccountName,
	}).GetAddress()

	const fundAmt = btcutil.Amount(500_000)
	ht.SendOutputsWithoutChange(
		[]*wire.TxOut{{
			Value:    int64(fundAmt),
			PkScript: ht.PayToAddrScript(ht.DecodeAddress(extAddr)),
		}}, defaultCreateAccountFeeRate,
	)
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Spend with FundPsbt so leftover value sits on an internal change
	// address, while the destination stays on the external branch.
	dest := alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type:    lnrpc.AddressType_TAPROOT_PUBKEY,
		Account: createAccountName,
	}).GetAddress()

	funded := alice.RPC.FundPsbt(&walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_Raw{
			Raw: &walletrpc.TxTemplate{
				Outputs: map[string]uint64{
					dest: uint64(fundAmt / 2),
				},
			},
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: 5,
		},
		Account: createAccountName,
	})

	finalized := alice.RPC.FinalizePsbt(&walletrpc.FinalizePsbtRequest{
		FundedPsbt: funded.GetFundedPsbt(),
		Account:    createAccountName,
	})
	alice.RPC.PublishTransaction(&walletrpc.Transaction{
		TxHex: finalized.GetRawFinalTx(),
	})
	ht.MineBlocksAndAssertNumTxes(1, 1)

	listed := alice.RPC.ListAccounts(&walletrpc.ListAccountsRequest{
		Name:        createAccountName,
		AddressType: walletrpc.AddressType_TAPROOT_PUBKEY,
	}).GetAccounts()
	require.Len(ht, listed, 1)

	// Both counters must have moved. If the internal count is still
	// zero, FundPsbt did not produce a change output and this test
	// would not catch the recovery bug.
	extCount := listed[0].GetExternalKeyCount()
	intCount := listed[0].GetInternalKeyCount()
	require.Greater(ht, extCount, uint32(0),
		"external branch should have issued addresses")
	require.Greater(ht, intCount, uint32(0),
		"internal branch should have issued a change address")

	after := alice.RPC.WalletBalance().GetAccountBalance()
	wantBal := after[createAccountName].GetConfirmedBalance()
	require.Greater(ht, wantBal, int64(fundAmt/2),
		"change should have stayed in the account")

	xpub := account.GetExtendedPublicKey()
	path := account.GetDerivationPath()

	// Restore the seed into a fresh wallet. The recovery window only
	// rederives the default account, so the custom account's coins are
	// still invisible until we reconstruct it by hand.
	restored := ht.RestoreNodeWithSeed(
		"AliceRestore", nil, password, mnemonic, "", 0, nil,
	)

	restored.RPC.XCreateAccount(&walletrpc.XCreateAccountRequest{
		Name:        precedingAccountName,
		AddressType: walletrpc.AddressType_TAPROOT_PUBKEY,
	})
	restoredAcct := restored.RPC.XCreateAccount(
		&walletrpc.XCreateAccountRequest{
			Name:        createAccountName,
			AddressType: walletrpc.AddressType_TAPROOT_PUBKEY,
		},
	).GetAccount()

	require.Equal(ht, xpub, restoredAcct.GetExtendedPublicKey())
	require.Equal(ht, path, restoredAcct.GetDerivationPath())

	// Replay each branch separately. NextAddr defaults to change=false,
	// which is why a single aggregate count is not enough.
	for i := uint32(0); i < extCount; i++ {
		restored.RPC.NextAddr(&walletrpc.AddrRequest{
			Account: createAccountName,
			Type:    walletrpc.AddressType_TAPROOT_PUBKEY,
			Change:  false,
		})
	}
	for i := uint32(0); i < intCount; i++ {
		restored.RPC.NextAddr(&walletrpc.AddrRequest{
			Account: createAccountName,
			Type:    walletrpc.AddressType_TAPROOT_PUBKEY,
			Change:  true,
		})
	}

	// A rescan only searches for addresses already in the wallet DB.
	ht.RestartNodeWithExtraArgs(
		restored, []string{"--reset-wallet-transactions"},
	)

	ht.AssertWalletAccountBalance(restored, createAccountName, wantBal, 0)

	// Confirm both branches actually hold coins, not just that the
	// aggregate balance happens to match.
	var sawExt, sawInt bool
	for _, acct := range restored.RPC.ListAddresses(
		&walletrpc.ListAddressesRequest{
			AccountName: createAccountName,
		},
	).GetAccountWithAddresses() {
		for _, addr := range acct.GetAddresses() {
			if addr.GetBalance() == 0 {
				continue
			}
			if addr.GetIsInternal() {
				sawInt = true
			} else {
				sawExt = true
			}
		}
	}
	require.True(ht, sawExt, "external branch funds should be recovered")
	require.True(ht, sawInt, "internal branch funds should be recovered")

	// The reconstructed account must also be spendable.
	spendDest := restored.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type:    lnrpc.AddressType_TAPROOT_PUBKEY,
		Account: createAccountName,
	}).GetAddress()
	spendAmt := uint64(wantBal / 4)
	require.Greater(ht, spendAmt, uint64(0))

	fundedAgain := restored.RPC.FundPsbt(&walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_Raw{
			Raw: &walletrpc.TxTemplate{
				Outputs: map[string]uint64{
					spendDest: spendAmt,
				},
			},
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: 5,
		},
		Account: createAccountName,
	})
	finalAgain := restored.RPC.FinalizePsbt(&walletrpc.FinalizePsbtRequest{
		FundedPsbt: fundedAgain.GetFundedPsbt(),
		Account:    createAccountName,
	})
	require.NotEmpty(ht, finalAgain.GetRawFinalTx(),
		"restored account must be able to sign")

	restored.RPC.PublishTransaction(&walletrpc.Transaction{
		TxHex: finalAgain.GetRawFinalTx(),
	})
	ht.MineBlocksAndAssertNumTxes(1, 1)

	afterSpend := restored.RPC.WalletBalance().GetAccountBalance()
	got := afterSpend[createAccountName].GetConfirmedBalance()
	require.Less(ht, got, wantBal, "the spend should have paid a fee")
	require.Greater(ht, got, wantBal-int64(maxCreateAccountSpendFee),
		"the account should still hold its funds minus fees")
}
