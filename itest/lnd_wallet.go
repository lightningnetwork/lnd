package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// walletTestCases defines a set of tests aiming at asserting functionalities
// provided by the wallerpc.
var walletTestCases = []*lntest.TestCase{
	{
		Name: "listunspent P2WPKH",
		TestFunc: func(ht *lntest.HarnessTest) {
			runTestListUnspent(ht, ht.FundCoins)
		},
	},
	{
		Name: "listunspent NP2WPKH",
		TestFunc: func(ht *lntest.HarnessTest) {
			runTestListUnspent(ht, ht.FundCoinsNP2WKH)
		},
	},
	{
		Name: "listunspent P2TR",
		TestFunc: func(ht *lntest.HarnessTest) {
			runTestListUnspent(ht, ht.FundCoinsP2TR)
		},
	},
	{
		Name: "listunspent P2WPKH restart",
		TestFunc: func(ht *lntest.HarnessTest) {
			runTestListUnspentRestart(ht, ht.FundCoins)
		},
	},
	{
		Name: "listunspent NP2WPKH restart",
		TestFunc: func(ht *lntest.HarnessTest) {
			runTestListUnspentRestart(ht, ht.FundCoinsNP2WKH)
		},
	},
	{
		Name: "listunspent P2TR restart",
		TestFunc: func(ht *lntest.HarnessTest) {
			runTestListUnspentRestart(ht, ht.FundCoinsP2TR)
		},
	},
}

type fundMethod func(amt btcutil.Amount, hn *node.HarnessNode) *wire.MsgTx

// testListUnspent checks that once Alice sends all coins to Bob, her wallet
// balances are updated and the ListUnspent returns no UTXO.
func runTestListUnspent(ht *lntest.HarnessTest, fundCoins fundMethod) {
	// Create two test nodes.
	alice := ht.NewNode("Alice", nil)
	bob := ht.NewNode("Bob", nil)

	// Fund Alice one UTXO.
	coin := btcutil.Amount(100_000)
	fundCoins(coin, alice)

	// Log Alice's wallet balance for debug.
	balance := alice.RPC.WalletBalance()
	ht.Logf("Alice has balance: %v", balance)

	// Send all Alice's balance to Bob.
	ht.SendAllCoins(alice, bob)

	// Mine Alice's send coin tx.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Alice wallet should be empty now, assert that all her balance fields
	// are zero.
	ht.AssertWalletAccountBalance(alice, lnwallet.DefaultAccountName, 0, 0)
	ht.AssertWalletLockedBalance(alice, 0)

	// Alice should have no UTXO.
	ht.AssertNumUTXOs(alice, 0)
}

// testListUnspentRestart checks that once Alice sends all coins to Bob, then
// restarts, her wallet balances are updated and the ListUnspent returns no
// UTXO.
func runTestListUnspentRestart(ht *lntest.HarnessTest, fundCoins fundMethod) {
	// Create two test nodes.
	alice := ht.NewNode("Alice", nil)
	bob := ht.NewNode("Bob", nil)

	// Fund Alice one UTXO.
	coin := btcutil.Amount(100_000)
	fundCoins(coin, alice)

	// Log Alice's wallet balance for debug.
	balance := alice.RPC.WalletBalance()
	ht.Logf("Alice has balance: %v", balance)

	// Send all Alice's balance to Bob.
	ht.SendAllCoins(alice, bob)

	// Shutdown Alice.
	restart := ht.SuspendNode(alice)

	// Mine Alice's send coin tx.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Restart Alice.
	require.NoError(ht, restart())

	// Alice wallet should be empty now, assert that all her balance fields
	// are zero.
	ht.AssertWalletAccountBalance(alice, lnwallet.DefaultAccountName, 0, 0)
	ht.AssertWalletLockedBalance(alice, 0)

	// Alice should have no UTXO.
	ht.AssertNumUTXOs(alice, 0)
}
