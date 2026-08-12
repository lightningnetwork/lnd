package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

const (
	// createAccountName is the account these tests create and spend from.
	createAccountName = "custom"

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
