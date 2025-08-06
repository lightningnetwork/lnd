package btcwallet

import (
	"encoding/hex"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntest/unittest"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

var (
	// seedBytes is the raw entropy of the aezeed:
	//   able promote dizzy mixture sword myth share public find tattoo
	//   catalog cousin bulb unfair machine alarm cool large promote kick
	//   shop rug mean year
	// Which corresponds to the master root key:
	//   xprv9s21ZrQH143K2KADjED57FvNbptdKLp4sqKzssegwEGKQMGoDkbyhUeCKe5m3A
	//   MU44z4vqkmGswwQVKrv599nFG16PPZDEkNrogwoDGeCmZ
	seedBytes, _ = hex.DecodeString("4a7611b6979ba7c4bc5c5cd2239b2973")

	// firstAddress is the first address that we should get from the wallet,
	// corresponding to the derivation path m/84'/0'/0'/0/0 (even on regtest
	// which is a special case for the BIP49/84 addresses in btcwallet).
	firstAddress = "bcrt1qgdlgjc5ede7fjv350wcjqat80m0zsmfaswsj9p"

	// firstAddressPubKey is the public key of the first address that we
	// should get from the wallet.
	firstAddressPubKey = "02b844aecf8250c29e46894147a7dae02de55a034a533b6" +
		"0c6a6469294ee356ce4"

	// firstAddressTaproot is the first address that we should get from the
	// wallet when deriving a taproot address.
	firstAddressTaproot = "bcrt1ps8c222fgysvnsj2m8hxk8khy6wthcrhv9va9z3t4" +
		"h3qeyz65sh4qqwvdgc"

	// firstAddressTaprootPubKey is the public key of the first address that
	// we should get from the wallet when deriving a taproot address.
	firstAddressTaprootPubKey = "03004113d6185c955d6e8f5922b50cc0ac3b64fa" +
		"0979402604c5b887f07e3b5388"

	testPubKeyBytes, _ = hex.DecodeString(
		"037a67771635344641d4b56aac33cd5f7a265b59678dce3aec31b89125e3" +
			"b8b9b2",
	)
	testPubKey, _          = btcec.ParsePubKey(testPubKeyBytes)
	testTaprootKeyBytes, _ = hex.DecodeString(
		"03f068684c9141027318eed958dccbf4f7f748700e1da53315630d82a362" +
			"d6a887",
	)
	testTaprootKey, _ = btcec.ParsePubKey(testTaprootKeyBytes)

	testTapscriptAddr = "bcrt1p7p5xsny3gyp8xx8wm9vdejl57lm5suqwrkjnx9trpk" +
		"p2xckk4zrs4xehl8"
	testTapscriptPkScript = append(
		[]byte{txscript.OP_1, txscript.OP_DATA_32},
		schnorr.SerializePubKey(testTaprootKey)...,
	)

	testCases = []struct {
		name string
		path []uint32
		err  string
		wif  string
	}{{
		name: "m/84'/0'/0'/0/0",
		path: []uint32{
			hardenedKey(84), hardenedKey(0), hardenedKey(0), 0, 0,
		},
		wif: "cPp3XUewCBQVg3pgwVWbtpzDwhWTQpHhu8saN3SdGRTkiLpu1R6h",
	}, {
		name: "m/84'/0'/0'/1/0",
		path: []uint32{
			hardenedKey(84), hardenedKey(0), hardenedKey(0), 1, 0,
		},
		wif: "cPUR1nFAeYAtSWSkKoWB6WbzRTbDSGdrGRmv1kVLRPyo7QXph2gt",
	}, {
		name: "m/84'/0'/0'/0/12345",
		path: []uint32{
			hardenedKey(84), hardenedKey(0), hardenedKey(0), 0,
			12345,
		},
		wif: "cQCdGxqKeGZKiC2uRYMAGenJHkDvajiPieT4Yg7k1BKawjKkywvz",
	}, {
		name: "m/49'/0'/0'/0/0",
		path: []uint32{
			hardenedKey(49), hardenedKey(0), hardenedKey(0), 0, 0,
		},
		wif: "cMwVK2bcTzivPZfcCH585rBGghqsJAP9MdVy8inRti1wZvLn5DvY",
	}, {
		name: "m/49'/0'/0'/1/0",
		path: []uint32{
			hardenedKey(49), hardenedKey(0), hardenedKey(0), 1, 0,
		},
		wif: "cNPW9bMtdc2YGBzWzSCXFN4excjrT34nZzGYtfkzkazUrt3dXuv7",
	}, {
		name: "m/49'/0'/0'/1/12345",
		path: []uint32{
			hardenedKey(49), hardenedKey(0), hardenedKey(0), 1,
			12345,
		},
		wif: "cNdJt2fSNUJYVSb8JFjhosPcQgNvJ92SjNeNpsf1gUwDVDv2KVRa",
	}, {
		name: "m/1017'/1'/0'/0/0",
		path: []uint32{
			hardenedKey(1017), hardenedKey(1), hardenedKey(0), 0, 0,
		},
		wif: "cPsCmbWQENgptj3eTiyd85QSAD1xqYKPM9jUkfvm7vgN3SoVPWSP",
	}, {
		name: "m/1017'/1'/6'/0/0",
		path: []uint32{
			hardenedKey(1017), hardenedKey(1), hardenedKey(6), 0, 0,
		},
		wif: "cPeQdpcGJmLqpdmvokh3DK9ZtjYAXxiw4p4ELNUWkWt6bMRqArEV",
	}, {
		name: "m/1017'/1'/7'/0/123",
		path: []uint32{
			hardenedKey(1017), hardenedKey(1), hardenedKey(7), 0,
			123,
		},
		wif: "cPcWZMqY4YErkcwjtFJaYoXkzd7bKxrfxAVzhDgy3n5BGH8CU8sn",
	}, {
		name: "m/84'/1'/0'/0/0",
		path: []uint32{
			hardenedKey(84), hardenedKey(1), hardenedKey(0), 0, 0,
		},
		err: "coin type must be 0 for BIP49/84 btcwallet keys",
	}, {
		name: "m/1017'/0'/0'/0/0",
		path: []uint32{
			hardenedKey(1017), hardenedKey(0), hardenedKey(0), 0, 0,
		},
		err: "expected coin type 1, instead was 0",
	}, {
		name: "m/84'/0'/1'/0/0",
		path: []uint32{
			hardenedKey(84), hardenedKey(0), hardenedKey(1), 0, 0,
		},
		err: "account 1 not found",
	}, {
		name: "m/49'/0'/1'/0/0",
		path: []uint32{
			hardenedKey(49), hardenedKey(0), hardenedKey(1), 0, 0,
		},
		err: "account 1 not found",
	}, {
		name: "non-hardened purpose m/84/0/0/0/0",
		path: []uint32{84, 0, 0, 0, 0},
		err:  "element at index 0 is not hardened",
	}, {
		name: "non-hardened account m/84'/0'/0/0/0",
		path: []uint32{hardenedKey(84), hardenedKey(0), 0, 0, 0},
		err:  "element at index 2 is not hardened",
	}}
)

// TestBip32KeyDerivation makes sure that private keys can be derived from a
// BIP32 key path correctly.
func TestBip32KeyDerivation(t *testing.T) {
	netParams := &chaincfg.RegressionNetParams
	w, _ := newTestWallet(t, netParams, seedBytes)

	// This is just a sanity check that the wallet was initialized
	// correctly. We make sure the first derived address is the expected
	// one.
	firstDerivedAddr, err := w.NewAddress(
		lnwallet.WitnessPubKey, false, lnwallet.DefaultAccountName,
	)
	require.NoError(t, err)
	require.Equal(t, firstAddress, firstDerivedAddr.String())

	// Let's go through the test cases now that we know our wallet is ready.
	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			privKey, err := w.deriveKeyByBIP32Path(tc.path)

			if tc.err == "" {
				require.NoError(t, err)
				wif, err := btcutil.NewWIF(
					privKey, netParams, true,
				)
				require.NoError(t, err)
				require.Equal(t, tc.wif, wif.String())
			} else {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.err)
			}
		})
	}
}

// TestScriptImport tests the btcwallet's tapscript import capabilities by
// importing both a full taproot script tree and a partially revealed branch
// with a proof to make sure the resulting addresses match up.
func TestScriptImport(t *testing.T) {
	netParams := &chaincfg.RegressionNetParams
	w, miner := newTestWallet(t, netParams, seedBytes)

	firstDerivedAddr, err := w.NewAddress(
		lnwallet.TaprootPubkey, false, lnwallet.DefaultAccountName,
	)
	require.NoError(t, err)
	require.Equal(t, firstAddressTaproot, firstDerivedAddr.String())

	scope := waddrmgr.KeyScopeBIP0086
	_, err = w.InternalWallet().AddrManager().FetchScopedKeyManager(scope)
	require.NoError(t, err)

	// Let's create a taproot script output now. This is a hash lock with a
	// simple preimage of "foobar".
	builder := txscript.NewScriptBuilder()
	builder.AddOp(txscript.OP_DUP)
	builder.AddOp(txscript.OP_HASH160)
	builder.AddData(btcutil.Hash160([]byte("foobar")))
	builder.AddOp(txscript.OP_EQUALVERIFY)
	script1, err := builder.Script()
	require.NoError(t, err)
	leaf1 := txscript.NewBaseTapLeaf(script1)

	// Let's add a second script output as well to test the partial reveal.
	builder = txscript.NewScriptBuilder()
	builder.AddData(schnorr.SerializePubKey(testPubKey))
	builder.AddOp(txscript.OP_CHECKSIG)
	script2, err := builder.Script()
	require.NoError(t, err)
	leaf2 := txscript.NewBaseTapLeaf(script2)

	// Our first test case is storing the script with all its leaves.
	tapscript1 := input.TapscriptFullTree(testPubKey, leaf1, leaf2)

	taprootKey1, err := tapscript1.TaprootKey()
	require.NoError(t, err)
	require.Equal(
		t, testTaprootKey.SerializeCompressed(),
		taprootKey1.SerializeCompressed(),
	)

	addr1, err := w.ImportTaprootScript(scope, tapscript1)
	require.NoError(t, err)

	require.Equal(t, testTapscriptAddr, addr1.Address().String())
	pkScript, err := txscript.PayToAddrScript(addr1.Address())
	require.NoError(t, err)
	require.Equal(t, testTapscriptPkScript, pkScript)

	// Send some coins to the taproot address now and wait until they are
	// seen as unconfirmed.
	_, err = miner.SendOutputs([]*wire.TxOut{{
		Value:    btcutil.SatoshiPerBitcoin,
		PkScript: pkScript,
	}}, 1)
	require.NoError(t, err)

	var utxos []*lnwallet.Utxo
	require.Eventually(t, func() bool {
		utxos, err = w.ListUnspentWitness(0, math.MaxInt32, "")
		require.NoError(t, err)

		return len(utxos) == 1
	}, time.Minute, 50*time.Millisecond)
	require.Equal(t, testTapscriptPkScript, utxos[0].PkScript)

	// Now, as a last test, make sure that when we try adding an address
	// with partial script reveal, we get an error that the address already
	// exists.
	inclusionProof := leaf2.TapHash()
	tapscript2 := input.TapscriptPartialReveal(
		testPubKey, leaf1, inclusionProof[:],
	)
	_, err = w.ImportTaprootScript(scope, tapscript2)
	require.Error(t, err)
	require.Contains(t, err.Error(), fmt.Sprintf(
		"address for script hash/key %x already exists",
		schnorr.SerializePubKey(testTaprootKey),
	))
}

func newTestWallet(t *testing.T, netParams *chaincfg.Params,
	seedBytes []byte) (*BtcWallet, *rpctest.Harness) {

	chainBackend, miner := getChainBackend(t, netParams)

	loaderOpt := LoaderWithLocalWalletDB(t.TempDir(), false, time.Minute)
	config := Config{
		PrivatePass: []byte("some-pass"),
		HdSeed:      seedBytes,
		NetParams:   netParams,
		CoinType:    netParams.HDCoinType,
		ChainSource: chainBackend,
		// wallet starts in recovery mode
		RecoveryWindow: 2,
		LoaderOptions:  []LoaderOption{loaderOpt},
	}
	blockCache := blockcache.NewBlockCache(10000)
	w, err := New(config, blockCache)
	if err != nil {
		t.Fatalf("creating wallet failed: %v", err)
	}

	err = w.Start()
	if err != nil {
		t.Fatalf("starting wallet failed: %v", err)
	}

	return w, miner
}

// getChainBackend returns a simple btcd based chain backend to back the wallet.
func getChainBackend(t *testing.T, netParams *chaincfg.Params) (chain.Interface,
	*rpctest.Harness) {

	miningNode := unittest.NewMiner(
		t, netParams, []string{"--txindex"}, true, 25,
	)

	// Next, mine enough blocks in order for SegWit and the CSV package
	// soft-fork to activate on RegNet.
	numBlocks := netParams.MinerConfirmationWindow * 2
	_, err := miningNode.Client.Generate(numBlocks)
	require.NoError(t, err)

	rpcConfig := miningNode.RPCConfig()
	chainClient, err := chain.NewRPCClient(
		netParams, rpcConfig.Host, rpcConfig.User, rpcConfig.Pass,
		rpcConfig.Certificates, false, 20,
	)
	require.NoError(t, err)

	return chainClient, miningNode
}

// hardenedKey returns a key of a hardened derivation key path.
func hardenedKey(part uint32) uint32 {
	return part + hdkeychain.HardenedKeyStart
}
