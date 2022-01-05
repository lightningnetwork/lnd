package btcwallet

import (
	"encoding/hex"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/integration/rpctest"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/hdkeychain"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightningnetwork/lnd/blockcache"
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
	w, cleanup := newTestWallet(t, netParams, seedBytes)
	defer cleanup()

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

func newTestWallet(t *testing.T, netParams *chaincfg.Params,
	seedBytes []byte) (*BtcWallet, func()) {

	tempDir, err := ioutil.TempDir("", "lnwallet")
	if err != nil {
		_ = os.RemoveAll(tempDir)
		t.Fatalf("creating temp dir failed: %v", err)
	}

	chainBackend, backendCleanup := getChainBackend(t, netParams)
	cleanup := func() {
		_ = os.RemoveAll(tempDir)
		backendCleanup()
	}

	loaderOpt := LoaderWithLocalWalletDB(tempDir, false, time.Minute)
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
		cleanup()
		t.Fatalf("creating wallet failed: %v", err)
	}

	err = w.Start()
	if err != nil {
		cleanup()
		t.Fatalf("starting wallet failed: %v", err)
	}

	return w, cleanup
}

// getChainBackend returns a simple btcd based chain backend to back the wallet.
func getChainBackend(t *testing.T, netParams *chaincfg.Params) (chain.Interface,
	func()) {

	miningNode, err := rpctest.New(netParams, nil, nil, "")
	require.NoError(t, err)
	require.NoError(t, miningNode.SetUp(true, 25))

	// Next, mine enough blocks in order for SegWit and the CSV package
	// soft-fork to activate on RegNet.
	numBlocks := netParams.MinerConfirmationWindow * 2
	_, err = miningNode.Client.Generate(numBlocks)
	require.NoError(t, err)

	rpcConfig := miningNode.RPCConfig()
	chainClient, err := chain.NewRPCClient(
		netParams, rpcConfig.Host, rpcConfig.User, rpcConfig.Pass,
		rpcConfig.Certificates, false, 20,
	)
	require.NoError(t, err)

	return chainClient, func() {
		_ = miningNode.TearDown()
	}
}

// hardenedKey returns a key of a hardened derivation key path.
func hardenedKey(part uint32) uint32 {
	return part + hdkeychain.HardenedKeyStart
}
