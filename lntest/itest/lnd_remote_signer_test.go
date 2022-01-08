package itest

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/hdkeychain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

var (
	rootKey = "tprv8ZgxMBicQKsPe6jS4vDm2n7s42Q6MpvghUQqMmSKG7bTZvGKtjrcU3" +
		"PGzMNG37yzxywrcdvgkwrr8eYXJmbwdvUNVT4Ucv7ris4jvA7BUmg"

	nodePubKey = "033f55d436d4f7d24aeffb1b976647380f22ebf9e74390e8c76dcff" +
		"9fea0093b7a"

	accounts = []*lnrpc.WatchOnlyAccount{{
		Purpose: waddrmgr.KeyScopeBIP0049Plus.Purpose,
		// We always use the mainnet coin type for our BIP49/84
		// addresses!
		CoinType: 0,
		Account:  0,
		Xpub: "tpubDDXEYWvGCTytEF6hBog9p4qr2QBUvJhh4P2wM4qHHv9N489khk" +
			"QoGkBXDVoquuiyBf8SKBwrYseYdtq9j2v2nttPpE8qbuW3sE2MCk" +
			"FPhTq",
	}, {
		Purpose: waddrmgr.KeyScopeBIP0084.Purpose,
		// We always use the mainnet coin type for our BIP49/84
		// addresses!
		CoinType: 0,
		Account:  0,
		Xpub: "tpubDDWAWrSLRSFrG1KdqXMQQyTKYGSKLKaY7gxpvK7RdV3e3Dkhvu" +
			"W2GgsFvsPN4RGmuoYtUgZ1LHZE8oftz7T4mzc1BxGt5rt8zJcVQi" +
			"KTPPV",
	}}
)

// testRemoteSigner tests that a watch-only wallet can use a remote signing
// wallet to perform any signing or ECDH operations.
func testRemoteSigner(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	subTests := []struct {
		name       string
		randomSeed bool
		sendCoins  bool
		fn         func(tt *harnessTest, wo, carol *lntest.HarnessNode)
	}{{
		name:       "random seed",
		randomSeed: true,
		fn: func(tt *harnessTest, wo, carol *lntest.HarnessNode) {
			// Nothing more to test here.
		},
	}, {
		name: "account import",
		fn: func(tt *harnessTest, wo, carol *lntest.HarnessNode) {
			runWalletImportAccountScenario(
				net, tt,
				walletrpc.AddressType_WITNESS_PUBKEY_HASH,
				carol, wo,
			)
		},
	}, {
		name:      "basic channel open close",
		sendCoins: true,
		fn: func(tt *harnessTest, wo, carol *lntest.HarnessNode) {
			runBasicChannelCreationAndUpdates(
				net, tt, wo, carol,
			)
		},
	}, {
		name:      "async payments",
		sendCoins: true,
		fn: func(tt *harnessTest, wo, carol *lntest.HarnessNode) {
			runAsyncPayments(net, tt, wo, carol)
		},
	}, {
		name: "shared key",
		fn: func(tt *harnessTest, wo, carol *lntest.HarnessNode) {
			runDeriveSharedKey(tt, wo)
		},
	}, {
		name:      "cpfp",
		sendCoins: true,
		fn: func(tt *harnessTest, wo, carol *lntest.HarnessNode) {
			runCPFP(net, tt, wo, carol)
		},
	}, {
		name: "psbt",
		fn: func(tt *harnessTest, wo, carol *lntest.HarnessNode) {
			runPsbtChanFunding(net, tt, carol, wo)
			runSignPsbt(tt, net, wo)
		},
	}}

	for _, st := range subTests {
		subTest := st

		// Signer is our signing node and has the wallet with the full
		// master private key. We test that we can create the watch-only
		// wallet from the exported accounts but also from a static key
		// to make sure the derivation of the account public keys is
		// correct in both cases.
		password := []byte("itestpassword")
		var (
			signerNodePubKey  = nodePubKey
			watchOnlyAccounts = deriveCustomScopeAccounts(t.t)
			signer            *lntest.HarnessNode
			err               error
		)
		if !subTest.randomSeed {
			signer, err = net.RestoreNodeWithSeed(
				"Signer", nil, password, nil, rootKey, 0, nil,
			)
			require.NoError(t.t, err)
		} else {
			signer = net.NewNode(t.t, "Signer", nil)
			signerNodePubKey = signer.PubKeyStr

			rpcAccts, err := signer.WalletKitClient.ListAccounts(
				ctxb, &walletrpc.ListAccountsRequest{},
			)
			require.NoError(t.t, err)

			watchOnlyAccounts, err = walletrpc.AccountsToWatchOnly(
				rpcAccts.Accounts,
			)
			require.NoError(t.t, err)
		}

		// WatchOnly is the node that has a watch-only wallet and uses
		// the Signer node for any operation that requires access to
		// private keys.
		watchOnly, err := net.NewNodeRemoteSigner(
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
		require.NoError(t.t, err)

		resp, err := watchOnly.GetInfo(ctxb, &lnrpc.GetInfoRequest{})
		require.NoError(t.t, err)

		require.Equal(t.t, signerNodePubKey, resp.IdentityPubkey)

		if subTest.sendCoins {
			net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, watchOnly)
			assertAccountBalance(
				t.t, watchOnly, "default",
				btcutil.SatoshiPerBitcoin, 0,
			)
		}

		carol := net.NewNode(t.t, "carol", nil)
		net.EnsureConnected(t.t, watchOnly, carol)

		success := t.t.Run(subTest.name, func(tt *testing.T) {
			ht := newHarnessTest(tt, net)
			subTest.fn(ht, watchOnly, carol)
		})

		shutdownAndAssert(net, t, carol)
		shutdownAndAssert(net, t, watchOnly)
		shutdownAndAssert(net, t, signer)

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
