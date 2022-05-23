package itest

import (
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// testGetRecoveryInfo checks whether lnd gives the right information about
// the wallet recovery process.
func testGetRecoveryInfo(ht *lntest.HarnessTest) {
	// First, create a new node with strong passphrase and grab the mnemonic
	// used for key derivation. This will bring up Carol with an empty
	// wallet, and such that she is synced up.
	password := []byte("The Magic Words are Squeamish Ossifrage")
	carol, mnemonic, _ := ht.NewNodeWithSeed("Carol", nil, password, false)

	checkInfo := func(expectedRecoveryMode, expectedRecoveryFinished bool,
		expectedProgress float64, recoveryWindow int32) {

		// Restore Carol, passing in the password, mnemonic, and
		// desired recovery window.
		node := ht.RestoreNodeWithSeed(
			carol.Name(), nil, password, mnemonic, "",
			recoveryWindow, nil,
		)

		// Query carol for her current wallet recovery progress.
		err := wait.NoError(func() error {
			// Verify that recovery info gives the right response.
			resp := ht.GetRecoveryInfo(node)

			mode := resp.RecoveryMode
			finished := resp.RecoveryFinished
			progress := resp.Progress

			if mode != expectedRecoveryMode {
				return fmt.Errorf("expected recovery mode %v "+
					"got %v", expectedRecoveryMode, mode)
			}
			if finished != expectedRecoveryFinished {
				return fmt.Errorf("expected finished %v "+
					"got %v", expectedRecoveryFinished,
					finished)
			}
			if progress != expectedProgress {
				return fmt.Errorf("expected progress %v"+
					"got %v", expectedProgress, progress)
			}

			return nil
		}, defaultTimeout)
		require.NoError(ht, err)

		// Lastly, shutdown this Carol so we can move on to the next
		// restoration.
		ht.Shutdown(node)
	}

	// Restore Carol with a recovery window of 0. Since it's not in recovery
	// mode, the recovery info will give a response with recoveryMode=false,
	// recoveryFinished=false, and progress=0
	checkInfo(false, false, 0, 0)

	// Change the recovery windown to be 1 to turn on recovery mode. Since
	// the current chain height is the same as the birthday height, it
	// should indicate the recovery process is finished.
	checkInfo(true, true, 1, 1)

	// We now go ahead 5 blocks. Because the wallet's syncing process is
	// controlled by a goroutine in the background, it will catch up
	// quickly. This makes the recovery progress back to 1.
	ht.MineBlocks(5)
	checkInfo(true, true, 1, 1)
}

// testOnchainFundRecovery checks lnd's ability to rescan for onchain outputs
// when providing a valid aezeed that owns outputs on the chain. This test
// performs multiple restorations using the same seed and various recovery
// windows to ensure we detect funds properly.
func testOnchainFundRecovery(ht *lntest.HarnessTest) {
	// First, create a new node with strong passphrase and grab the mnemonic
	// used for key derivation. This will bring up Carol with an empty
	// wallet, and such that she is synced up.
	password := []byte("The Magic Words are Squeamish Ossifrage")
	carol, mnemonic, _ := ht.NewNodeWithSeed("Carol", nil, password, false)

	// As long as the mnemonic is non-nil and the extended key is empty, the
	// closure below will always restore the node from the seed. The tests
	// need to manually overwrite this value to change that behavior.
	rootKey := ""

	// Create a closure for testing the recovery of Carol's wallet. This
	// method takes the expected value of Carol's balance when using the
	// given recovery window. Additionally, the caller can specify an action
	// to perform on the restored node before the node is shutdown.
	restoreCheckBalance := func(expAmount int64, expectedNumUTXOs uint32,
		recoveryWindow int32, fn func(*lntest.HarnessNode)) {

		ht.Helper()

		// Restore Carol, passing in the password, mnemonic, and
		// desired recovery window.
		node := ht.RestoreNodeWithSeed(
			carol.Name(), nil, password, mnemonic, rootKey,
			recoveryWindow, nil,
		)

		// Query carol for her current wallet balance, and also that we
		// gain the expected number of UTXOs.
		var (
			currBalance  int64
			currNumUTXOs uint32
		)
		err := wait.NoError(func() error {
			resp := ht.GetWalletBalance(node)
			currBalance = resp.ConfirmedBalance

			utxoResp := ht.ListUnspent(node, "", math.MaxInt32, 0)
			currNumUTXOs = uint32(len(utxoResp.Utxos))

			// Verify that Carol's balance and number of UTXOs
			// matches what's expected.
			if expAmount != currBalance {
				return fmt.Errorf("balance not matched, want "+
					"%d, got %d", expAmount, currBalance)
			}
			if currNumUTXOs != expectedNumUTXOs {
				return fmt.Errorf("num of UTXOs not matched, "+
					"want %d, got %d", expectedNumUTXOs,
					currNumUTXOs)
			}

			return nil
		}, defaultTimeout)
		require.NoError(ht, err, "timeout checking Carol")

		// If the user provided a callback, execute the commands against
		// the restored Carol.
		if fn != nil {
			fn(node)
		}

		// Lastly, shutdown this Carol so we can move on to the next
		// restoration.
		ht.Shutdown(node)
	}

	// Create a closure-factory for building closures that can generate and
	// skip a configurable number of addresses, before finally sending coins
	// to a next generated address. The returned closure will apply the same
	// behavior to both default P2WKH and NP2WKH scopes.
	skipAndSend := func(nskip int) func(*lntest.HarnessNode) {
		return func(node *lntest.HarnessNode) {
			ht.Helper()

			// Generate and skip the number of addresses requested.
			for i := 0; i < nskip; i++ {
				ht.NewAddress(node, AddrTypeWitnessPubkeyHash)
				ht.NewAddress(node, AddrTypeNestedPubkeyHash)
				ht.NewAddress(node, AddrTypeTaprootPubkey)
			}

			// Send one BTC to the next P2WKH address.
			ht.SendCoins(btcutil.SatoshiPerBitcoin, node)

			// And another to the next NP2WKH address.
			ht.SendCoinsNP2WKH(btcutil.SatoshiPerBitcoin, node)

			// Add another whole coin to the P2TR address.
			ht.SendCoinsP2TR(btcutil.SatoshiPerBitcoin, node)
		}
	}

	// Restore Carol with a recovery window of 0. Since no coins have been
	// sent, her balance should be zero.
	//
	// After, one BTC is sent to both her first external P2WKH and NP2WKH
	// addresses.
	restoreCheckBalance(0, 0, 0, skipAndSend(0))

	// Check that restoring without a look-ahead results in having no funds
	// in the wallet, even though they exist on-chain.
	restoreCheckBalance(0, 0, 0, nil)

	// Now, check that using a look-ahead of 1 recovers the balance from
	// the two transactions above. We should also now have 2 UTXOs in the
	// wallet at the end of the recovery attempt.
	//
	// After, we will generate and skip 9 P2WKH, NP2WKH and P2TR addresses,
	// and send another BTC to the subsequent 10th address in each
	// derivation path.
	restoreCheckBalance(3*btcutil.SatoshiPerBitcoin, 3, 1, skipAndSend(9))

	// Check that using a recovery window of 9 does not find the two most
	// recent txns.
	restoreCheckBalance(3*btcutil.SatoshiPerBitcoin, 3, 9, nil)

	// Extending our recovery window to 10 should find the most recent
	// transactions, leaving the wallet with 6 BTC total. We should also
	// learn of the two additional UTXOs created above.
	//
	// After, we will skip 19 more addrs, sending to the 20th address past
	// our last found address, and repeat the same checks.
	restoreCheckBalance(6*btcutil.SatoshiPerBitcoin, 6, 10, skipAndSend(19))

	// Check that recovering with a recovery window of 19 fails to find the
	// most recent transactions.
	restoreCheckBalance(6*btcutil.SatoshiPerBitcoin, 6, 19, nil)

	// Ensure that using a recovery window of 20 succeeds with all UTXOs
	// found and the final balance reflected.

	// After these checks are done, we'll want to make sure we can also
	// recover change address outputs.  This is mainly motivated by a now
	// fixed bug in the wallet in which change addresses could at times be
	// created outside of the default key scopes. Recovery only used to be
	// performed on the default key scopes, so ideally this test case
	// would've caught the bug earlier. Carol has received 9 BTC so far from
	// the miner, we'll send 8 back to ensure all of her UTXOs get spent to
	// avoid fee discrepancies and a change output is formed.
	const minerAmt = 8 * btcutil.SatoshiPerBitcoin
	const finalBalance = 9 * btcutil.SatoshiPerBitcoin
	promptChangeAddr := func(node *lntest.HarnessNode) {
		ht.Helper()

		minerAddr := ht.NewMinerAddress()
		resp := ht.SendCoinFromNode(
			node, &lnrpc.SendCoinsRequest{
				Addr:   minerAddr.String(),
				Amount: minerAmt,
			},
		)

		txid := ht.AssertNumTxsInMempool(1)[0]
		require.Equal(ht, txid.String(), resp.Txid)

		block := ht.MineBlocks(1)[0]
		ht.AssertTxInBlock(block, txid)
	}
	restoreCheckBalance(finalBalance, 9, 20, promptChangeAddr)

	// We should expect a static fee of 50100 satoshis for spending 9 inputs
	// (3 P2WPKH, 3 NP2WPKH, 3 P2TR) to two P2WPKH outputs. Carol should
	// therefore only have one UTXO present (the change output) of
	// 9 - 8 - fee BTC.
	const fee = 50100
	restoreCheckBalance(finalBalance-minerAmt-fee, 1, 21, nil)

	// Last of all, make sure we can also restore a node from the extended
	// master root key directly instead of the seed.
	var seedMnemonic aezeed.Mnemonic
	copy(seedMnemonic[:], mnemonic)
	cipherSeed, err := seedMnemonic.ToCipherSeed(password)
	require.NoError(ht, err)
	extendedRootKey, err := hdkeychain.NewMaster(
		cipherSeed.Entropy[:], harnessNetParams,
	)
	require.NoError(ht, err)
	rootKey = extendedRootKey.String()
	mnemonic = nil

	restoreCheckBalance(finalBalance-minerAmt-fee, 1, 21, nil)
}
