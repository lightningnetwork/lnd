package itest

import (
	"bytes"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
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
	{
		Name: "submit package basic",
		TestFunc: func(ht *lntest.HarnessTest) {
			runTestSubmitPackageBasicCPFP(ht)
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

// Helper to convert walletrpc.KeyDescriptor to a P2WPKH address.
func keyDescToAddrP2WPKH(ht *lntest.HarnessTest,
	keyDesc *signrpc.KeyDescriptor) btcutil.Address {

	harnessNetParams := &chaincfg.RegressionNetParams

	pubKeyBytes := keyDesc.RawKeyBytes
	require.NotEmpty(ht, pubKeyBytes, "KeyDescriptor has empty RawKeyBytes")

	addr, err := btcutil.NewAddressWitnessPubKeyHash(
		btcutil.Hash160(pubKeyBytes),
		harnessNetParams,
	)
	require.NoError(ht, err)

	return addr
}

// runTestSubmitPackageBasicCPFP tests the SubmitPackage RPC for a simple
// Child-Pays-For-Parent (CPFP) scenario.
func runTestSubmitPackageBasicCPFP(ht *lntest.HarnessTest) {
	const (
		fundingAmount = btcutil.Amount(10_000_000)
		childFeeRate  = 100
		testKeyFamily = 111
	)

	// Create a new node (Alice).
	alice := ht.NewNode("Alice", nil)
	defer ht.Shutdown(alice)

	// Fund Alice with a UTXO.
	ht.FundCoins(fundingAmount*2, alice)

	// Derive a key and address for the initial funding.
	fundingKey := alice.RPC.DeriveNextKey(
		&walletrpc.KeyReq{
			KeyFamily: testKeyFamily,
		},
	)

	fundingAddr := keyDescToAddrP2WPKH(ht, fundingKey)
	ht.Logf("Funding address: %s", fundingAddr)

	// Send funds to this specific address.
	ht.SendCoinsToAddr(alice, fundingAddr, fundingAmount)
	block := ht.MineBlocksAndAssertNumTxes(1, 1)

	// Now find the funding tx in the mined block so we can use the output
	// as an input to the parent tx.
	fundingPkScript := ht.PayToAddrScript(fundingAddr)
	var (
		fundingOutIndex uint32
		fundingOut      *wire.TxOut
		fundingTx       *wire.MsgTx
	)

	for _, tx := range block[0].Transactions {
		for i, txOut := range tx.TxOut {
			if bytes.Equal(txOut.PkScript, fundingPkScript) {
				fundingTx = tx
				fundingOut = txOut
				fundingOutIndex = uint32(i)

				break
			}
		}
	}
	require.NotNil(ht, fundingTx, "Funding output not found")

	fundOutPoint := wire.OutPoint{
		Hash:  fundingTx.TxHash(),
		Index: fundingOutIndex,
	}

	parentOutValue := int64(fundingAmount)

	// Create a V3 "parent" transaction spending the funding output.
	parentTx := wire.NewMsgTx(3)
	parentTx.AddTxIn(wire.NewTxIn(&fundOutPoint, nil, nil))

	// Derive a key and address for the parent's output.
	parentOutKey := alice.RPC.DeriveNextKey(
		&walletrpc.KeyReq{
			KeyFamily: testKeyFamily,
		},
	)

	parentOutAddr := keyDescToAddrP2WPKH(ht, parentOutKey)
	parentOutPkScript := ht.PayToAddrScript(parentOutAddr)
	parentTx.AddTxOut(wire.NewTxOut(parentOutValue, parentOutPkScript))

	// Serialize the parent tx.
	var parentBuf bytes.Buffer
	require.NoError(ht, parentTx.Serialize(&parentBuf))

	// Create a sign descriptor for the parent tx.
	parentSignDesc := &signrpc.SignDescriptor{
		KeyDesc: fundingKey,
		Output: &signrpc.TxOut{
			Value:    fundingOut.Value,
			PkScript: fundingOut.PkScript,
		},
		Sighash:       uint32(txscript.SigHashAll),
		WitnessScript: fundingOut.PkScript,
		SignMethod:    signrpc.SignMethod_SIGN_METHOD_WITNESS_V0,
	}

	// Sign the parent tx.
	parentSignResp, err := alice.RPC.Signer.SignOutputRaw(
		ht.Context(),
		&signrpc.SignReq{
			RawTxBytes: parentBuf.Bytes(),
			SignDescs: []*signrpc.SignDescriptor{
				parentSignDesc,
			},
		},
	)
	require.NoError(ht, err)

	// Set the parent tx's witness to the signature.
	parentTx.TxIn[0].Witness = makeP2WPKHWitness(
		parentSignResp.RawSigs[0], fundingKey,
	)

	// Reserialize the parent after the witness has been set.
	parentBuf.Reset()
	require.NoError(ht, parentTx.Serialize(&parentBuf))

	parentOut := &wire.OutPoint{
		Hash:  parentTx.TxHash(),
		Index: 0,
	}

	// Create a new P2WPKH address for the child's output.
	childOutAddrResp := alice.RPC.NewAddress(
		&lnrpc.NewAddressRequest{
			Account: lnwallet.DefaultAccountName,
			Type:    lnrpc.AddressType_WITNESS_PUBKEY_HASH,
		},
	)
	require.NoError(ht, err)

	// Estimate child's weight and fee.
	childEstimator := input.TxWeightEstimator{}
	childEstimator.AddP2WKHInput()
	childEstimator.AddP2WKHOutput()
	childWeight := childEstimator.Weight()
	childFee := chainfee.SatPerVByte(
		childFeeRate,
	).FeePerKWeight().FeeForWeight(
		childWeight,
	)

	childOutValue := btcutil.Amount(parentOutValue) - childFee

	// Create the unsigned child transaction spending the parent's output.
	childTx := wire.NewMsgTx(3)
	childTx.AddTxIn(wire.NewTxIn(parentOut, nil, nil))

	childOutAddr := ht.DecodeAddress(childOutAddrResp.Address)
	childOutPkScript := ht.PayToAddrScript(childOutAddr)

	childTx.AddTxOut(
		wire.NewTxOut(int64(childOutValue), childOutPkScript),
	)

	// Serialize the child tx.
	var childBuf bytes.Buffer
	require.NoError(ht, childTx.Serialize(&childBuf))

	// Create a sign descriptor for the child tx.
	childSignDesc := &signrpc.SignDescriptor{
		KeyDesc: parentOutKey,
		Output: &signrpc.TxOut{
			Value:    parentOutValue,
			PkScript: parentOutPkScript,
		},
		Sighash:       uint32(txscript.SigHashAll),
		WitnessScript: parentOutPkScript,
		SignMethod:    signrpc.SignMethod_SIGN_METHOD_WITNESS_V0,
	}

	// Sign the child tx.
	childSignResp, err := alice.RPC.Signer.SignOutputRaw(
		ht.Context(),
		&signrpc.SignReq{
			RawTxBytes: childBuf.Bytes(),
			SignDescs: []*signrpc.SignDescriptor{
				childSignDesc,
			},
		},
	)
	require.NoError(ht, err)

	// Set the child tx's witness to the signature.
	childTx.TxIn[0].Witness = makeP2WPKHWitness(
		childSignResp.RawSigs[0], parentOutKey,
	)

	// Reserialize the child after the witness has been set.
	childBuf.Reset()
	require.NoError(ht, childTx.Serialize(&childBuf))

	submitPackageResp, err := alice.RPC.WalletKit.SubmitPackage(
		ht.Context(), &walletrpc.SubmitPackageRequest{
			ParentTxs: [][]byte{parentBuf.Bytes()},
			ChildTx:   childBuf.Bytes(),
		},
	)
	require.NoError(ht, err)
	ht.Logf("SubmitPackage response: %v", spew.Sdump(submitPackageResp))

	// TODO(bhandras): once we have package support for btcd or we switch
	// to bitcoind miner in the tests we can uncomment the following lines
	// and test the package transactions in the mempool and mined blocks.
	/*
		ht.AssertTxInMempool(parentTx.TxHash())
		ht.AssertTxInMempool(childTx.TxHash())

		ht.Log("Mining block with package transactions...")
		block = ht.MineBlocksAndAssertNumTxes(1, 2)
		ht.AssertTxInBlock(block[0], parentTx.TxHash())
		ht.AssertTxInBlock(block[0], childTx.TxHash())
	*/
}

// makeP2WPKHWitness creates a P2WPKH witness stack for the given signature and
// key descriptor.
func makeP2WPKHWitness(sig []byte,
	keyDesc *signrpc.KeyDescriptor) wire.TxWitness {

	sigWithHash := append(sig, byte(txscript.SigHashAll))

	return wire.TxWitness{
		sigWithHash,         // element 0: 70-73-byte signature.
		keyDesc.RawKeyBytes, // element 1: 33-byte compressed pubkey.
	}
}
