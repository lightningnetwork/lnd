package itest

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcutil"
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
	}, {
		Purpose:  keychain.BIP0043Purpose,
		CoinType: harnessNetParams.HDCoinType,
		Account:  uint32(keychain.KeyFamilyMultiSig),
		Xpub: "tpubDDXFHr67Ro2tHKVWG2gNjjijKUH1Lyv5NKFYdJnuaLGVNBVwyV" +
			"5AbykhR43iy8wYozEMbw2QfmAqZhb8gnuL5mm9sZh8YsR6FjGAbe" +
			"w1xoT",
	}, {
		Purpose:  keychain.BIP0043Purpose,
		CoinType: harnessNetParams.HDCoinType,
		Account:  uint32(keychain.KeyFamilyRevocationBase),
		Xpub: "tpubDDXFHr67Ro2tKkccDqNfDqZpd5wCs2n6XRV2Uh185DzCTbkDaE" +
			"d9v7P837zZTYBNVfaRriuxgGVgxbGjDui4CKxyzBzwz4aAZxjn2P" +
			"hNcQy",
	}, {
		Purpose:  keychain.BIP0043Purpose,
		CoinType: harnessNetParams.HDCoinType,
		Account:  uint32(keychain.KeyFamilyHtlcBase),
		Xpub: "tpubDDXFHr67Ro2tNH4KH41i4oTsWfRjFigoH1Ee7urvHow51opH9x" +
			"J7mu1qSPMPVtkVqQZ5tE4NTuFJPrbDqno7TQietyUDmPTwyVviJb" +
			"GCwXk",
	}, {
		Purpose:  keychain.BIP0043Purpose,
		CoinType: harnessNetParams.HDCoinType,
		Account:  uint32(keychain.KeyFamilyPaymentBase),
		Xpub: "tpubDDXFHr67Ro2tQj5Zvav2ALhkU6dRQAhEtNPnYJVBC8hs2U1A9e" +
			"cqxRY3XTiJKBDD7e8tudhmTRs8aGWJAiAXJN5kXy3Hi6cmiwGWjX" +
			"K5Cv5",
	}, {
		Purpose:  keychain.BIP0043Purpose,
		CoinType: harnessNetParams.HDCoinType,
		Account:  uint32(keychain.KeyFamilyDelayBase),
		Xpub: "tpubDDXFHr67Ro2tSSR2LLBJtotxx2U45cuESLWKA72YT9td3SzVKH" +
			"AptzDEx5chsUNZ4WRMY5h6HJxRSebjRatxQKX1uUsux1LvKS1wsf" +
			"NJ2PH",
	}, {
		Purpose:  keychain.BIP0043Purpose,
		CoinType: harnessNetParams.HDCoinType,
		Account:  uint32(keychain.KeyFamilyRevocationRoot),
		Xpub: "tpubDDXFHr67Ro2tTwzfWvNoMoPpZbxdMEfe1WhbXJxvXikGixPa4g" +
			"gSRZeGx6T5yxVHTVT3rjVh35Veqsowj7emX8SZfXKDKDKcLduXCe" +
			"WPUU3",
	}, {
		Purpose:  keychain.BIP0043Purpose,
		CoinType: harnessNetParams.HDCoinType,
		Account:  uint32(keychain.KeyFamilyNodeKey),
		Xpub: "tpubDDXFHr67Ro2tYEDS2EByRedfsUoEwBtrzVbS1qdPrX6sAkUYGL" +
			"rZWvMmQv8KZDZ4zd9r8WzM9bJ2nGp7XuNVC4w2EBtWg7i76gbrmu" +
			"EWjQh",
	}, {
		Purpose:  keychain.BIP0043Purpose,
		CoinType: harnessNetParams.HDCoinType,
		Account:  uint32(keychain.KeyFamilyStaticBackup),
		Xpub: "tpubDDXFHr67Ro2tYpwnFJEQaM8eAPM2UV5uY6gFgXeSzS5aC5T9Tf" +
			"zXuawYKBbQMZJn8qHXLafY4tAutoda1aKP5h6Nbgy3swPbnhWbFj" +
			"S5wnX",
	}, {
		Purpose:  keychain.BIP0043Purpose,
		CoinType: harnessNetParams.HDCoinType,
		Account:  uint32(keychain.KeyFamilyTowerSession),
		Xpub: "tpubDDXFHr67Ro2tddKpAjUegXqt7EGxRXnHkeLbUkfuFMGbLJYgRp" +
			"G4ew5pMmGg2nmcGmHFQ29w3juNhd8N5ZZ8HwJdymC4f5ukQLJ4yg" +
			"9rEr3",
	}, {
		Purpose:  keychain.BIP0043Purpose,
		CoinType: harnessNetParams.HDCoinType,
		Account:  uint32(keychain.KeyFamilyTowerID),
		Xpub: "tpubDDXFHr67Ro2tgE89V8ZdgMytC2Jq1iT9ttGhdzR1X7haQJNBmX" +
			"t8kau6taC6DGASYzbrjmo9z9w6JQFcaLNqbhS2h2PVSzKf79j265" +
			"Zi8hF",
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
			watchOnlyAccounts = accounts
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
				t.t, signer, "default",
				btcutil.SatoshiPerBitcoin, 0,
			)
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
