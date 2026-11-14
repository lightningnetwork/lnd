package itest

import (
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// walletTestCases defines a set of tests aiming at asserting functionalities
// provided by the wallerpc.
var walletTestCases = []*lntest.TestCase{
	{
		Name:     "xcreate account",
		TestFunc: testXCreateAccount,
	},
	{
		Name:     "xcreate account rejections",
		TestFunc: testXCreateAccountRejections,
	},
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
	{
		Name:     "internal address type p2wkh",
		TestFunc: testInternalAddressTypeP2WKH,
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

// testInternalAddressTypeP2WKH checks that a node started with
// --internal-address-type=p2wkh never pays itself to a P2TR output: not in the
// change of SendCoins, not in the change of a channel funding transaction, not
// in a cooperative close and not in the sweep after a force close.
func testInternalAddressTypeP2WKH(ht *lntest.HarnessTest) {
	alice := ht.NewNodeWithCoins(
		"Alice", []string{"--internal-address-type=p2wkh"},
	)
	bob := ht.NewNode("Bob", nil)
	ht.EnsureConnected(alice, bob)

	// Send coins to a P2WKH address of Bob, so every output of the
	// transaction is P2WKH unless the change is P2TR.
	addr := bob.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	})
	alice.RPC.SendCoins(&lnrpc.SendCoinsRequest{
		Addr:       addr.Address,
		Amount:     100_000,
		TargetConf: 6,
	})
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	assertNoP2TROutputs(ht, block.Transactions[1])

	// Open a channel funded by Alice's wallet and check its change.
	chanAmt := btcutil.Amount(500_000)
	cp := ht.OpenChannel(alice, bob, lntest.OpenChannelParams{
		Amt: chanAmt,
	})
	fundingTxid := ht.GetChanPointFundingTxid(cp)
	assertNoP2TROutputs(
		ht, ht.Miner().GetRawTransaction(fundingTxid).MsgTx(),
	)

	// Both nodes signal option_shutdown_anysegwit, so without the option
	// Alice would use a P2TR delivery address. Bob has no balance in the
	// channel, so all outputs of the closing transaction are Alice's.
	closingTxid := ht.CloseChannel(alice, cp)
	assertNoP2TROutputs(
		ht, ht.Miner().GetRawTransaction(closingTxid).MsgTx(),
	)

	// Finally, force close a channel and check the sweep of Alice's
	// to_local output.
	cp = ht.OpenChannel(alice, bob, lntest.OpenChannelParams{
		Amt: chanAmt,
	})
	stream, _ := ht.CloseChannelAssertPending(alice, cp, true)
	ht.AssertStreamChannelForceClosed(alice, cp, false, stream)
	ht.AssertNumPendingForceClose(alice, 1)

	// The sweeper broadcasts the sweep at DefaultCSV-1.
	ht.MineEmptyBlocks(node.DefaultCSV - 1)
	ht.AssertNumPendingSweeps(alice, 1)

	sweepTx := ht.GetNumTxsFromMempool(1)[0]
	assertNoP2TROutputs(ht, sweepTx)
	ht.MineBlocksAndAssertNumTxes(1, 1)
}

// assertNoP2TROutputs asserts that the given transaction has no P2TR outputs
// and at least one P2WKH output.
func assertNoP2TROutputs(ht *lntest.HarnessTest, tx *wire.MsgTx) {
	var numP2WKH int
	for _, txOut := range tx.TxOut {
		require.Falsef(ht, txscript.IsPayToTaproot(txOut.PkScript),
			"tx %v has a P2TR output", tx.TxHash())

		if txscript.IsPayToWitnessPubKeyHash(txOut.PkScript) {
			numP2WKH++
		}
	}

	require.Positivef(ht, numP2WKH, "tx %v has no P2WKH output",
		tx.TxHash())
}
