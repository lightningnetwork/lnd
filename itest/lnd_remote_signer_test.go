package itest

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/stretchr/testify/require"
)

var (
	rootKey = "tprv8ZgxMBicQKsPe6jS4vDm2n7s42Q6MpvghUQqMmSKG7bTZvGKtjrcU3" +
		"PGzMNG37yzxywrcdvgkwrr8eYXJmbwdvUNVT4Ucv7ris4jvA7BUmg"

	nodePubKey = "033f55d436d4f7d24aeffb1b976647380f22ebf9e74390e8c76dcff" +
		"9fea0093b7a"

	accounts = []*lnrpc.WatchOnlyAccount{{
		Purpose: waddrmgr.KeyScopeBIP0049Plus.Purpose,
		// We always use the mainnet coin type for our BIP49/84/86
		// addresses!
		CoinType: 0,
		Account:  0,
		Xpub: "tpubDDXEYWvGCTytEF6hBog9p4qr2QBUvJhh4P2wM4qHHv9N489khk" +
			"QoGkBXDVoquuiyBf8SKBwrYseYdtq9j2v2nttPpE8qbuW3sE2MCk" +
			"FPhTq",
	}, {
		Purpose: waddrmgr.KeyScopeBIP0084.Purpose,
		// We always use the mainnet coin type for our BIP49/84/86
		// addresses!
		CoinType: 0,
		Account:  0,
		Xpub: "tpubDDWAWrSLRSFrG1KdqXMQQyTKYGSKLKaY7gxpvK7RdV3e3Dkhvu" +
			"W2GgsFvsPN4RGmuoYtUgZ1LHZE8oftz7T4mzc1BxGt5rt8zJcVQi" +
			"KTPPV",
	}, {
		Purpose: waddrmgr.KeyScopeBIP0086.Purpose,
		// We always use the mainnet coin type for our BIP49/84/86
		// addresses!
		CoinType: 0,
		Account:  0,
		Xpub: "tpubDDtdXpdJFU2zFKWHJwe5M2WtYtcV7qSWtKohT9VP9zarNSwKnm" +
			"kwDQawsu1vUf9xwXhUDYXbdUqpcrRTn9bLyW4BAVRimZ4K7r5o1J" +
			"S924u",
	}}
)

// testRemoteSigner tests that a watch-only wallet can use a remote signing
// wallet to perform any signing or ECDH operations.
func testRemoteSigner(ht *lntest.HarnessTest) {
	type testCase struct {
		name       string
		randomSeed bool
		sendCoins  bool
		fn         func(tt *lntest.HarnessTest,
			wo, carol *node.HarnessNode)
	}

	subTests := []testCase{{
		name:       "random seed",
		randomSeed: true,
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			// Nothing more to test here.
		},
	}, {
		name: "account import",
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			runWalletImportAccountScenario(
				tt, walletrpc.AddressType_WITNESS_PUBKEY_HASH,
				carol, wo,
			)
		},
	}, {
		name:      "basic channel open close",
		sendCoins: true,
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			runBasicChannelCreationAndUpdates(tt, wo, carol)
		},
	}, {
		name:      "channel funding input types",
		sendCoins: false,
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			runChannelFundingInputTypes(tt, carol, wo)
		},
	}, {
		name:      "async payments",
		sendCoins: true,
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			runAsyncPayments(tt, wo, carol)
		},
	}, {
		name: "shared key",
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			runDeriveSharedKey(tt, wo)
		},
	}, {
		name:      "cpfp",
		sendCoins: true,
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			runCPFP(tt, wo, carol)
		},
	}, {
		name:       "psbt",
		randomSeed: true,
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			runPsbtChanFunding(tt, carol, wo)
			runSignPsbtSegWitV0P2WKH(tt, wo)
			runSignPsbtSegWitV1KeySpendBip86(tt, wo)
			runSignPsbtSegWitV1KeySpendRootHash(tt, wo)
			runSignPsbtSegWitV1ScriptSpend(tt, wo)

			// The above tests all make sure we can sign for keys
			// that aren't in the wallet. But we also want to make
			// sure we can fund and then sign PSBTs from our wallet.
			runFundAndSignPsbt(ht, wo)
		},
	}, {
		name:      "sign output raw",
		sendCoins: true,
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			runSignOutputRaw(tt, wo)
		},
	}, {
		name:      "sign verify msg",
		sendCoins: true,
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			runSignVerifyMessage(tt, wo)
		},
	}, {
		name:       "taproot",
		sendCoins:  true,
		randomSeed: true,
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			testTaprootSendCoinsKeySpendBip86(tt, wo)
			testTaprootComputeInputScriptKeySpendBip86(tt, wo)
			testTaprootSignOutputRawScriptSpend(tt, wo)
			testTaprootSignOutputRawKeySpendBip86(tt, wo)
			testTaprootSignOutputRawKeySpendRootHash(tt, wo)

			muSig2Versions := []signrpc.MuSig2Version{
				signrpc.MuSig2Version_MUSIG2_VERSION_V040,
				signrpc.MuSig2Version_MUSIG2_VERSION_V100RC2,
			}
			for _, version := range muSig2Versions {
				testTaprootMuSig2KeySpendRootHash(
					tt, wo, version,
				)
				testTaprootMuSig2ScriptSpend(tt, wo, version)
				testTaprootMuSig2KeySpendBip86(tt, wo, version)
				testTaprootMuSig2CombinedLeafKeySpend(
					tt, wo, version,
				)
			}
		},
	}}

	prepareTest := func(st *lntest.HarnessTest,
		subTest testCase) (*node.HarnessNode,
		*node.HarnessNode, *node.HarnessNode) {

		// Signer is our signing node and has the wallet with the full
		// master private key. We test that we can create the watch-only
		// wallet from the exported accounts but also from a static key
		// to make sure the derivation of the account public keys is
		// correct in both cases.
		password := []byte("itestpassword")
		var (
			signerNodePubKey  = nodePubKey
			watchOnlyAccounts = deriveCustomScopeAccounts(ht.T)
			signer            *node.HarnessNode
			err               error
		)
		if !subTest.randomSeed {
			signer = st.RestoreNodeWithSeed(
				"Signer", nil, password, nil, rootKey, 0, nil,
			)
		} else {
			signer = st.NewNode("Signer", nil)
			signerNodePubKey = signer.PubKeyStr

			rpcAccts := signer.RPC.ListAccounts(
				&walletrpc.ListAccountsRequest{},
			)

			watchOnlyAccounts, err = walletrpc.AccountsToWatchOnly(
				rpcAccts.Accounts,
			)
			require.NoError(st, err)
		}

		// WatchOnly is the node that has a watch-only wallet and uses
		// the Signer node for any operation that requires access to
		// private keys.
		watchOnly := st.NewNodeRemoteSigner(
			"WatchOnly", []string{
				"--remotesigner.enable",
				fmt.Sprintf(
					"--remotesigner.rpchost=localhost:%d",
					signer.Cfg.RPCPort,
				),
				fmt.Sprintf(
					"--remotesigner.tlscertpath=%s",
					signer.Cfg.TLSCertPath,
				),
				fmt.Sprintf(
					"--remotesigner.macaroonpath=%s",
					signer.Cfg.AdminMacPath,
				),
			}, password, &lnrpc.WatchOnly{
				MasterKeyBirthdayTimestamp: 0,
				MasterKeyFingerprint:       nil,
				Accounts:                   watchOnlyAccounts,
			},
		)

		resp := watchOnly.RPC.GetInfo()
		require.Equal(st, signerNodePubKey, resp.IdentityPubkey)

		if subTest.sendCoins {
			st.FundCoins(btcutil.SatoshiPerBitcoin, watchOnly)
			ht.AssertWalletAccountBalance(
				watchOnly, "default",
				btcutil.SatoshiPerBitcoin, 0,
			)
		}

		carol := st.NewNode("carol", nil)
		st.EnsureConnected(watchOnly, carol)

		return signer, watchOnly, carol
	}

	for _, testCase := range subTests {
		subTest := testCase

		success := ht.Run(subTest.name, func(tt *testing.T) {
			// Skip the cleanup here as no standby node is used.
			st := ht.Subtest(tt)

			_, watchOnly, carol := prepareTest(st, subTest)
			subTest.fn(st, watchOnly, carol)
		})

		if !success {
			return
		}
	}
}

// deriveCustomScopeAccounts derives the first 255 default accounts of the custom lnd
// internal key scope.
func deriveCustomScopeAccounts(t *testing.T) []*lnrpc.WatchOnlyAccount {
	allAccounts := make([]*lnrpc.WatchOnlyAccount, 0, 255+len(accounts))
	allAccounts = append(allAccounts, accounts...)

	extendedRootKey, err := hdkeychain.NewKeyFromString(rootKey)
	require.NoError(t, err)

	path := []uint32{
		keychain.BIP0043Purpose + hdkeychain.HardenedKeyStart,
		harnessNetParams.HDCoinType + hdkeychain.HardenedKeyStart,
	}
	coinTypeKey, err := derivePath(extendedRootKey, path)
	require.NoError(t, err)
	for idx := uint32(0); idx <= 255; idx++ {
		accountPath := []uint32{idx + hdkeychain.HardenedKeyStart}
		accountKey, err := derivePath(coinTypeKey, accountPath)
		require.NoError(t, err)

		accountXPub, err := accountKey.Neuter()
		require.NoError(t, err)

		allAccounts = append(allAccounts, &lnrpc.WatchOnlyAccount{
			Purpose:  keychain.BIP0043Purpose,
			CoinType: harnessNetParams.HDCoinType,
			Account:  idx,
			Xpub:     accountXPub.String(),
		})
	}

	return allAccounts
}

// derivePath derives the given path from an extended key.
func derivePath(key *hdkeychain.ExtendedKey, path []uint32) (
	*hdkeychain.ExtendedKey, error) {

	var (
		currentKey = key
		err        error
	)
	for _, pathPart := range path {
		currentKey, err = currentKey.Derive(pathPart)
		if err != nil {
			return nil, err
		}
	}

	return currentKey, nil
}
