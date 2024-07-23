package itest

import (
	"bytes"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
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
			resp := node.RPC.GetRecoveryInfo(nil)

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
		recoveryWindow int32, fn func(*node.HarnessNode)) {

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
			resp := node.RPC.WalletBalance()
			currBalance = resp.ConfirmedBalance

			req := &walletrpc.ListUnspentRequest{
				Account:  "",
				MaxConfs: math.MaxInt32,
				MinConfs: 0,
			}
			utxoResp := node.RPC.ListUnspent(req)
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
	skipAndSend := func(nskip int) func(*node.HarnessNode) {
		return func(node *node.HarnessNode) {
			ht.Helper()

			// Generate and skip the number of addresses requested.
			for i := 0; i < nskip; i++ {
				req := &lnrpc.NewAddressRequest{}

				req.Type = AddrTypeWitnessPubkeyHash
				node.RPC.NewAddress(req)

				req.Type = AddrTypeNestedPubkeyHash
				node.RPC.NewAddress(req)

				req.Type = AddrTypeTaprootPubkey
				node.RPC.NewAddress(req)
			}

			// Send one BTC to the next P2WKH address.
			ht.FundCoins(btcutil.SatoshiPerBitcoin, node)

			// And another to the next NP2WKH address.
			ht.FundCoinsNP2WKH(btcutil.SatoshiPerBitcoin, node)

			// Add another whole coin to the P2TR address.
			ht.FundCoinsP2TR(btcutil.SatoshiPerBitcoin, node)
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
	promptChangeAddr := func(node *node.HarnessNode) {
		ht.Helper()

		minerAddr := ht.NewMinerAddress()
		req := &lnrpc.SendCoinsRequest{
			Addr:       minerAddr.String(),
			Amount:     minerAmt,
			TargetConf: 6,
		}
		resp := node.RPC.SendCoins(req)

		txid := ht.AssertNumTxsInMempool(1)[0]
		require.Equal(ht, txid.String(), resp.Txid)

		block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
		ht.AssertTxInBlock(block, txid)
	}
	restoreCheckBalance(finalBalance, 9, 20, promptChangeAddr)

	// We should expect a static fee of 36400 satoshis for spending 9
	// inputs (3 P2WPKH, 3 NP2WPKH, 3 P2TR) to two P2TR outputs. Carol
	// should therefore only have one UTXO present (the change output) of
	// 9 - 8 - fee BTC.
	const fee = 37000
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

// testRescanAddressDetection makes sure that addresses created from internal
// (m/1017' scope) keys aren't detected as UTXOs when re-scanning the wallet
// with --reset-wallet-transactions to avoid showing them as un-spent ghost
// UTXOs even if they are being spent. This is to test a fix in the wallet that
// addresses the following scenario:
//  1. A key is derived from the internal 1017' scope with a custom key family
//     and a p2wkh address is derived from that key.
//  2. Funds are sent to the address created above in a way that also creates a
//     change output. The change output is recognized as belonging to the
//     wallet, which is correct.
//  3. The funds on the address created in step 1 are fully spent (without
//     creating a change output) into an output that doesn't belong to the
//     wallet (e.g. a channel funding output).
//  4. At some point the user re-scans their wallet by using the
//     --reset-wallet-transactions flag.
//  5. The wallet re-scan detects the change output created in step 2 and flags
//     the transaction as relevant.
//  6. While adding the relevant TX to the wallet DB, the wallet also detects
//     the address from step 1 as belonging to the wallet (because the internal
//     key scope is defined as having the address type p2wkh) and adds that
//     output as an UTXO as well (<- this is the bug). The wallet now has two
//     UTXOs in its database.
//  7. The transaction that spends the UTXO of the address from step 1 is not
//     detected by the wallet as belonging to it (because the output is a
//     channel output and the input (correctly) isn't recognized as belonging to
//     the wallet in that part of the code, it is never marked as spent and
//     stays in the wallet as a ghost UTXO forever.
//
// The fix in the wallet is simple: In step 6, don't detect addresses from
// internal scopes while re-scanning to be in line with the logic in other areas
// of the wallet code.
func testRescanAddressDetection(ht *lntest.HarnessTest) {
	// We start off by creating a new node with the wallet re-scan flag
	// enabled. This won't have any effect on the first startup but will
	// come into effect after we re-start the node.
	walletPassword := []byte("some-password")
	carol, _, _ := ht.NewNodeWithSeed(
		"carol", []string{"--reset-wallet-transactions"},
		walletPassword, false,
	)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// Create an address generated from internal keys.
	keyDesc := carol.RPC.DeriveNextKey(&walletrpc.KeyReq{KeyFamily: 123})
	pubKeyHash := btcutil.Hash160(keyDesc.RawKeyBytes)
	ghostUtxoAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		pubKeyHash, harnessNetParams,
	)
	require.NoError(ht, err)

	// Send funds to the (p2wkh!) address generated from the internal
	// (m/1017') key scope. Because the internal key scope is defined as
	// p2wkh address type, this might be incorrectly detected by the wallet
	// in some situations (which this test makes sure is fixed).
	const ghostUtxoAmount = 456_000
	carol.RPC.SendCoins(&lnrpc.SendCoinsRequest{
		Addr:        ghostUtxoAddr.String(),
		Amount:      ghostUtxoAmount,
		SatPerVbyte: 1,
	})
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Make sure we see the change output in our list of unspent outputs.
	// We _don't_ expect to see the ghost UTXO here as in this step it's
	// ignored as an internal address correctly.
	ht.AssertNumUTXOsConfirmed(carol, 1)
	unspent := carol.RPC.ListUnspent(&walletrpc.ListUnspentRequest{
		MinConfs: 1,
	})

	// Which one was the change output and which one the ghost UTXO output?
	var ghostUtxoIndex uint32
	if unspent.Utxos[0].Outpoint.OutputIndex == 0 {
		ghostUtxoIndex = 1
	}

	ghostUtxoHash, err := chainhash.NewHash(
		unspent.Utxos[0].Outpoint.TxidBytes,
	)
	require.NoError(ht, err)

	burnScript, _ := ht.CreateBurnAddr(AddrTypeWitnessPubkeyHash)

	// Create fee estimation for a p2wkh input and p2wkh output.
	feeRate := chainfee.SatPerKWeight(12500)
	estimator := input.TxWeightEstimator{}
	estimator.AddP2WKHInput()
	estimator.AddP2WKHOutput()
	estimatedWeight := estimator.Weight()
	requiredFee := feeRate.FeeForWeight(estimatedWeight)

	tx := wire.NewMsgTx(2)
	tx.TxIn = []*wire.TxIn{{
		PreviousOutPoint: wire.OutPoint{
			Hash:  *ghostUtxoHash,
			Index: ghostUtxoIndex,
		},
	}}
	value := int64(ghostUtxoAmount - requiredFee)
	tx.TxOut = []*wire.TxOut{{
		PkScript: burnScript,
		Value:    value,
	}}

	var buf bytes.Buffer
	require.NoError(ht, tx.Serialize(&buf))

	ghostUtxoScript := ht.PayToAddrScript(ghostUtxoAddr)
	utxoInfo := []*signrpc.TxOut{{
		PkScript: ghostUtxoScript,
		Value:    ghostUtxoAmount,
	}}

	// Let's sign the input now.
	signResp := carol.RPC.SignOutputRaw(&signrpc.SignReq{
		RawTxBytes: buf.Bytes(),
		SignDescs: []*signrpc.SignDescriptor{{
			Output:        utxoInfo[0],
			InputIndex:    0,
			KeyDesc:       keyDesc,
			Sighash:       uint32(txscript.SigHashAll),
			WitnessScript: utxoInfo[0].PkScript,
		}},
	})

	// Add the witness to the input and publish the tx.
	tx.TxIn[0].Witness = wire.TxWitness{
		append(signResp.RawSigs[0], byte(txscript.SigHashAll)),
		keyDesc.RawKeyBytes,
	}
	buf.Reset()
	require.NoError(ht, tx.Serialize(&buf))
	carol.RPC.PublishTransaction(&walletrpc.Transaction{
		TxHex: buf.Bytes(),
	})

	// Wait until the spending tx is found and mine a block to confirm it.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// The wallet should still just see a single UTXO of the change output
	// created earlier.
	ht.AssertNumUTXOsConfirmed(carol, 1)

	// Let's now re-start the node, causing it to do the wallet re-scan.
	ht.RestartNode(carol)

	// There should now still only be a single UTXO from the change output
	// instead of two (the ghost UTXO should be missing if the fix works).
	ht.AssertNumUTXOsConfirmed(carol, 1)
}
