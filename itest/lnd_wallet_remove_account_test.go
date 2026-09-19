package itest

import (
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/stretchr/testify/require"
)

// walletRemoveAccountTestCases tests that a watch-only account imported via
// ImportAccount can be removed again, and that the accounts the wallet relies
// on are protected from removal.
var walletRemoveAccountTestCases = []*lntest.TestCase{
	{
		Name:     "happy path",
		TestFunc: testRemoveAccount,
	},
	{
		Name:     "rejections",
		TestFunc: testRemoveAccountRejections,
	},
}

// testRemoveAccount covers the full life cycle of an imported account: import
// Carol's account xpub into Dave's node, fund it, remove it while it still
// holds a balance, and assert the wallet stops tracking it — across a restart
// — until the same xpub is imported again.
func testRemoveAccount(ht *lntest.HarnessTest) {
	const utxoAmt int64 = btcutil.SatoshiPerBitcoin
	addrType := walletrpc.AddressType_WITNESS_PUBKEY_HASH

	// We'll start our test by having two nodes, Carol and Dave. Carol's
	// default wallet account will be imported into Dave's node, funded,
	// and then removed again.
	carol := ht.NewNode("carol", nil)
	dave := ht.NewNode("dave", nil)

	listResp := carol.RPC.ListAccounts(&walletrpc.ListAccountsRequest{
		Name:        "default",
		AddressType: addrType,
	})
	require.Len(ht, listResp.Accounts, 1)
	carolAccount := listResp.Accounts[0]

	const importedAccount = "carol"
	dave.RPC.ImportAccount(&walletrpc.ImportAccountRequest{
		Name:              importedAccount,
		ExtendedPublicKey: carolAccount.ExtendedPublicKey,
		AddressType:       addrType,
	})

	// Fund the imported account and confirm the coins, so that the removal
	// below provably happens on an account with a non-zero balance.
	externalAddr := newExternalAddr(
		ht, dave, carol, importedAccount, addrType,
	)

	alice := ht.NewNodeWithCoins("Alice", nil)
	alice.RPC.SendCoins(&lnrpc.SendCoinsRequest{
		Addr:       externalAddr,
		Amount:     utxoAmt,
		SatPerByte: 1,
	})
	ht.MineBlocksAndAssertNumTxes(1, 1)
	ht.AssertWalletAccountBalance(dave, importedAccount, utxoAmt, 0)

	// Remove the account. The response echoes what was removed.
	removeResp := dave.RPC.RemoveAccount(&walletrpc.RemoveAccountRequest{
		Name: importedAccount,
	})
	require.Equal(ht, importedAccount, removeResp.Account.Name)
	require.True(ht, removeResp.Account.WatchOnly)

	// The wallet must have dropped the account entirely: it no longer
	// lists, its addresses are gone and its balance no longer counts
	// towards the wallet's.
	assertAccountGone(ht, dave, importedAccount)

	// The removal must also survive a restart.
	ht.RestartNode(dave)
	assertAccountGone(ht, dave, importedAccount)

	// Re-importing the same xpub under the same name must succeed again. A
	// dry run first proves derivation restarts identically: the first
	// external address is the one we funded above.
	dryResp := dave.RPC.ImportAccount(&walletrpc.ImportAccountRequest{
		Name:              importedAccount,
		ExtendedPublicKey: carolAccount.ExtendedPublicKey,
		AddressType:       addrType,
		DryRun:            true,
	})
	require.NotEmpty(ht, dryResp.DryRunExternalAddrs)
	require.Equal(ht, externalAddr, dryResp.DryRunExternalAddrs[0])

	dave.RPC.ImportAccount(&walletrpc.ImportAccountRequest{
		Name:              importedAccount,
		ExtendedPublicKey: carolAccount.ExtendedPublicKey,
		AddressType:       addrType,
	})

	// The deposit predates the re-import, and events are only detected
	// from the import onwards, so the balance starts out at zero. This
	// pins ImportAccount's documented no-rescan limitation for the
	// remove-then-reimport cycle.
	ht.AssertWalletAccountBalance(dave, importedAccount, 0, 0)
}

// assertAccountGone asserts that no trace of the given account is left in the
// node's wallet: not in the account list, not in the address list and not in
// the wallet's balance.
func assertAccountGone(ht *lntest.HarnessTest, hn *node.HarnessNode,
	account string) {

	listResp := hn.RPC.ListAccounts(&walletrpc.ListAccountsRequest{})
	for _, acct := range listResp.Accounts {
		require.NotEqual(ht, account, acct.Name)
	}

	addrResp := hn.RPC.ListAddresses(&walletrpc.ListAddressesRequest{})
	for _, acct := range addrResp.AccountWithAddresses {
		require.NotEqual(ht, account, acct.Name)
	}

	balanceResp := hn.RPC.WalletBalance()
	require.NotContains(ht, balanceResp.AccountBalance, account)
	require.EqualValues(ht, 0, balanceResp.TotalBalance)
}

// testRemoveAccountRejections asserts the guard rails around RemoveAccount:
// the wallet's reserved accounts and accounts it owns itself cannot be
// removed, and a missing or unnamed account is reported as such.
func testRemoveAccountRejections(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", nil)

	// An empty name is refused before anything is looked up.
	err := alice.RPC.RemoveAccountAssertErr(
		&walletrpc.RemoveAccountRequest{},
	)
	require.ErrorContains(ht, err, "account name is required")

	// The wallet's own reserved accounts are refused by name.
	for _, reserved := range []string{"default", "imported"} {
		err := alice.RPC.RemoveAccountAssertErr(
			&walletrpc.RemoveAccountRequest{Name: reserved},
		)
		require.ErrorContains(ht, err, "reserved by the wallet")
	}

	// A name that does not resolve to any account is reported missing.
	err = alice.RPC.RemoveAccountAssertErr(
		&walletrpc.RemoveAccountRequest{Name: "does-not-exist"},
	)
	require.ErrorContains(ht, err, "not found")

	// An account the wallet derived from its own master key is not an
	// imported one and must be refused, and must still be intact
	// afterwards.
	const ownedAccount = "owned"
	alice.RPC.XCreateAccount(&walletrpc.XCreateAccountRequest{
		Name:              ownedAccount,
		AddressType:       walletrpc.AddressType_TAPROOT_PUBKEY,
		IKnowWhatIAmDoing: true,
	})

	err = alice.RPC.RemoveAccountAssertErr(
		&walletrpc.RemoveAccountRequest{Name: ownedAccount},
	)
	require.ErrorContains(ht, err, "not a watch-only imported account")

	listResp := alice.RPC.ListAccounts(&walletrpc.ListAccountsRequest{
		Name: ownedAccount,
	})
	require.Len(ht, listResp.Accounts, 1)
}
