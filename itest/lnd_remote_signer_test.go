package itest

import (
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet/rpcwallet"
	"github.com/stretchr/testify/require"
)

// remoteSignerTestCases defines a set of test cases to run against the remote
// signer.
var remoteSignerTestCases = []*lntest.TestCase{
	{
		Name:     "random seed",
		TestFunc: testRemoteSignerRandomSeed,
	},
	{
		Name:     "random seed outbound",
		TestFunc: testRemoteSignerRandomSeedOutbound,
	},
	{
		Name:     "account import",
		TestFunc: testRemoteSignerAccountImport,
	},
	{
		Name:     "account import outbound",
		TestFunc: testRemoteSignerAccountImportOutbound,
	},
	{
		Name:     "channel open",
		TestFunc: testRemoteSignerChannelOpen,
	},
	{
		Name:     "channel open outbound",
		TestFunc: testRemoteSignerChannelOpenOutbound,
	},
	{
		Name:     "funding input types",
		TestFunc: testRemoteSignerChannelFundingInputTypes,
	},
	{
		Name:     "funding input types outbound",
		TestFunc: testRemoteSignerChannelFundingInputTypesOutbound,
	},
	{
		Name:     "funding async payments",
		TestFunc: testRemoteSignerAsyncPayments,
	},
	{
		Name:     "funding async payments outbound",
		TestFunc: testRemoteSignerAsyncPaymentsOutbound,
	},
	{
		Name:     "funding async payments taproot",
		TestFunc: testRemoteSignerAsyncPaymentsTaproot,
	},
	{
		Name:     "funding async payments taproot outbound",
		TestFunc: testRemoteSignerAsyncPaymentsTaprootOutbound,
	},
	{
		Name:     "shared key",
		TestFunc: testRemoteSignerSharedKey,
	},
	{
		Name:     "shared key outbound",
		TestFunc: testRemoteSignerSharedKeyOutbound,
	},
	{
		Name:     "bump fee",
		TestFunc: testRemoteSignerBumpFee,
	},
	{
		Name:     "bump fee outbound",
		TestFunc: testRemoteSignerBumpFeeOutbound,
	},
	{
		Name:     "psbt",
		TestFunc: testRemoteSignerPSBT,
	},
	{
		Name:     "psbt outbound",
		TestFunc: testRemoteSignerPSBTOutbound,
	},
	{
		Name:     "sign output raw",
		TestFunc: testRemoteSignerSignOutputRaw,
	},
	{
		Name:     "sign output raw outbound",
		TestFunc: testRemoteSignerSignOutputRawOutbound,
	},
	{
		Name:     "verify msg",
		TestFunc: testRemoteSignerSignVerifyMsg,
	},
	{
		Name:     "verify msg outbound",
		TestFunc: testRemoteSignerSignVerifyMsgOutbound,
	},
	{
		Name:     "taproot",
		TestFunc: testRemoteSignerTaproot,
	},
	{
		Name:     "taproot outbound",
		TestFunc: testRemoteSignerTaprootOutbound,
	},
}

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

// remoteSignerTestCase defines a test case for the remote signer test suite.
type remoteSignerTestCase struct {
	name       string
	randomSeed bool
	sendCoins  bool
	commitType lnrpc.CommitmentType
	fn         func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode)
}

// prepareInboundRemoteSignerTest prepares a test case using an inbound remote
// signer for the test suite by creating three nodes.
func prepareInboundRemoteSignerTest(ht *lntest.HarnessTest,
	tc remoteSignerTestCase) (*node.HarnessNode, *node.HarnessNode,
	*node.HarnessNode) {

	// Signer is our signing node and has the wallet with the full master
	// private key. We test that we can create the watch-only wallet from
	// the exported accounts but also from a static key to make sure the
	// derivation of the account public keys is correct in both cases.
	password := []byte("itestpassword")
	var (
		signerNodePubKey  = nodePubKey
		watchOnlyAccounts = deriveCustomScopeAccounts(ht.T)
		signer            *node.HarnessNode
		err               error
	)
	if !tc.randomSeed {
		signer = ht.RestoreNodeWithSeed(
			"Signer", nil, password, nil, rootKey, 0, nil,
		)
	} else {
		signer = ht.NewNode("Signer", nil)
		signerNodePubKey = signer.PubKeyStr

		rpcAccts := signer.RPC.ListAccounts(
			&walletrpc.ListAccountsRequest{},
		)

		watchOnlyAccounts, err = walletrpc.AccountsToWatchOnly(
			rpcAccts.Accounts,
		)
		require.NoError(ht, err)
	}

	var commitArgs []string
	if tc.commitType == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		commitArgs = lntest.NodeArgsForCommitType(
			tc.commitType,
		)
	}

	// WatchOnly is the node that has a watch-only wallet and uses the
	// Signer node for any operation that requires access to private keys.
	watchOnly := ht.NewNodeWatchOnly(
		"WatchOnly", append([]string{
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
		}, commitArgs...),
		password, &lnrpc.WatchOnly{
			MasterKeyBirthdayTimestamp: 0,
			MasterKeyFingerprint:       nil,
			Accounts:                   watchOnlyAccounts,
		},
	)

	resp := watchOnly.RPC.GetInfo()
	require.Equal(ht, signerNodePubKey, resp.IdentityPubkey)

	if tc.sendCoins {
		ht.FundCoins(btcutil.SatoshiPerBitcoin, watchOnly)
		ht.AssertWalletAccountBalance(
			watchOnly, "default",
			btcutil.SatoshiPerBitcoin, 0,
		)
	}

	carol := ht.NewNode("carol", commitArgs)
	ht.EnsureConnected(watchOnly, carol)

	return signer, watchOnly, carol
}

// prepareInboundRemoteSignerTest prepares a test case using an outbound remote
// signer for the test suite by creating three nodes.
func prepareOutboundRemoteSignerTest(ht *lntest.HarnessTest,
	tc remoteSignerTestCase) (*node.HarnessNode, *node.HarnessNode,
	*node.HarnessNode) {

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

	var commitArgs []string
	if tc.commitType == lnrpc.CommitmentType_SIMPLE_TAPROOT {
		commitArgs = lntest.NodeArgsForCommitType(
			tc.commitType,
		)
	}

	// WatchOnly is the node that has a watch-only wallet and uses
	// the Signer node for any operation that requires access to
	// private keys. We use the outbound signer type here, meaning
	// that the watch-only node expects the signer to make an
	// outbound connection to it.
	watchOnly := ht.CreateNewNode(
		"WatchOnly", append([]string{
			"--remotesigner.enable",
			"--remotesigner.allowinboundconnection",
			"--remotesigner.timeout=30s",
			"--remotesigner.requesttimeout=30s",
		}, commitArgs...),
		password, true,
	)

	// As the signer node will make an outbound connection to the
	// watch-only node, we must specify the watch-only node's RPC
	// connection details in the signer's configuration.
	signerArgs := []string{
		"--watchonlynode.enable",
		"--watchonlynode.timeout=30s",
		"--watchonlynode.requesttimeout=10s",
		fmt.Sprintf(
			"--watchonlynode.rpchost=localhost:%d",
			watchOnly.Cfg.RPCPort,
		),
		fmt.Sprintf(
			"--watchonlynode.tlscertpath=%s",
			watchOnly.Cfg.TLSCertPath,
		),
		fmt.Sprintf(
			"--watchonlynode.macaroonpath=%s",
			watchOnly.Cfg.AdminMacPath,
		),
	}

	if !tc.randomSeed {
		signer = ht.RestoreNodeWithSeed(
			"Signer", signerArgs, password, nil, rootKey, 0,
			nil,
		)
	} else {
		signer = ht.NewNode("Signer", signerArgs)
		signerNodePubKey = signer.PubKeyStr

		rpcAccts := signer.RPC.ListAccounts(
			&walletrpc.ListAccountsRequest{},
		)

		watchOnlyAccounts, err = walletrpc.AccountsToWatchOnly(
			rpcAccts.Accounts,
		)
		require.NoError(ht, err)
	}

	// As the watch-only node will not fully start until the signer
	// node connects to it, we need to start the watch-only node
	// after having started the signer node.
	ht.StartWatchOnly(watchOnly, "WatchOnly", password,
		&lnrpc.WatchOnly{
			MasterKeyBirthdayTimestamp: 0,
			MasterKeyFingerprint:       nil,
			Accounts:                   watchOnlyAccounts,
		},
	)

	resp := watchOnly.RPC.GetInfo()
	require.Equal(ht, signerNodePubKey, resp.IdentityPubkey)

	if tc.sendCoins {
		ht.FundCoins(btcutil.SatoshiPerBitcoin, watchOnly)
		ht.AssertWalletAccountBalance(
			watchOnly, "default",
			btcutil.SatoshiPerBitcoin, 0,
		)
	}

	carol := ht.NewNode("carol", commitArgs)
	ht.EnsureConnected(watchOnly, carol)

	return signer, watchOnly, carol
}

func executeRemoteSignerTestCase(ht *lntest.HarnessTest,
	tc remoteSignerTestCase, isOutbound bool) {

	var watchOnly, carol *node.HarnessNode

	if isOutbound {
		_, watchOnly, carol = prepareOutboundRemoteSignerTest(ht, tc)
	} else {
		_, watchOnly, carol = prepareInboundRemoteSignerTest(ht, tc)
	}

	tc.fn(ht, watchOnly, carol)
}

func randomSeedTestCase(isOutbound bool) remoteSignerTestCase {
	tName := "random seed"
	if isOutbound {
		tName += " outbound"
	}

	return remoteSignerTestCase{
		name:       tName,
		randomSeed: true,
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			// Nothing more to test here.
		},
	}
}

// testRemoteSignerRandomSeed tests that a watch-only wallet can use a remote
// signing wallet to perform any signing or ECDH operations.
func testRemoteSignerRandomSeed(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(ht, randomSeedTestCase(false), false)
}

// testRemoteSignerRandomSeed tests that a watch-only wallet can use an outbound
// remote signing wallet to perform any signing or ECDH operations.
func testRemoteSignerRandomSeedOutbound(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(ht, randomSeedTestCase(true), true)
}

func accountImportTestCase(isOutbound bool) remoteSignerTestCase {
	tName := "account import"
	if isOutbound {
		tName += " outbound"
	}

	return remoteSignerTestCase{
		name: tName,
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			runWalletImportAccountScenario(
				tt, walletrpc.AddressType_WITNESS_PUBKEY_HASH,
				carol, wo,
			)
		},
	}
}

func testRemoteSignerAccountImport(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(ht, accountImportTestCase(false), false)
}

func testRemoteSignerAccountImportOutbound(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(ht, accountImportTestCase(true), true)
}

func channelOpenTestCase(isOutbound bool) remoteSignerTestCase {
	tName := "basic channel open close"
	if isOutbound {
		tName += " outbound"
	}

	return remoteSignerTestCase{
		name:      tName,
		sendCoins: true,
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			runBasicChannelCreationAndUpdates(tt, wo, carol)
		},
	}
}

func testRemoteSignerChannelOpen(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(ht, channelOpenTestCase(false), false)
}

func testRemoteSignerChannelOpenOutbound(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(ht, channelOpenTestCase(true), true)
}

func channelFundingInputTypesTestCase(isOutbound bool) remoteSignerTestCase {
	tName := "channel funding input types"
	if isOutbound {
		tName += " outbound"
	}

	return remoteSignerTestCase{
		name:      tName,
		sendCoins: false,
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			runChannelFundingInputTypes(tt, carol, wo)
		},
	}
}

func testRemoteSignerChannelFundingInputTypes(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(
		ht, channelFundingInputTypesTestCase(false), false,
	)
}

func testRemoteSignerChannelFundingInputTypesOutbound(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(
		ht, channelFundingInputTypesTestCase(true), true,
	)
}

func asyncPaymentsTestCase(isOutbound bool) remoteSignerTestCase {
	tName := "async payments"
	if isOutbound {
		tName += " outbound"
	}

	return remoteSignerTestCase{
		name:      tName,
		sendCoins: true,
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			runAsyncPayments(tt, wo, carol, nil)
		},
	}
}

func testRemoteSignerAsyncPayments(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(ht, asyncPaymentsTestCase(false), false)
}

func testRemoteSignerAsyncPaymentsOutbound(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(ht, asyncPaymentsTestCase(true), true)
}

func asyncPaymentsTaprootTestCase(isOutbound bool) remoteSignerTestCase {
	tName := "async payments taproot"
	if isOutbound {
		tName += " outbound"
	}

	return remoteSignerTestCase{
		name:      tName,
		sendCoins: true,
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			commitType := lnrpc.CommitmentType_SIMPLE_TAPROOT

			runAsyncPayments(
				tt, wo, carol, &commitType,
			)
		},
		commitType: lnrpc.CommitmentType_SIMPLE_TAPROOT,
	}
}

func testRemoteSignerAsyncPaymentsTaproot(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(
		ht, asyncPaymentsTaprootTestCase(false), false,
	)
}

func testRemoteSignerAsyncPaymentsTaprootOutbound(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(
		ht, asyncPaymentsTaprootTestCase(true), true,
	)
}

func sharedKeyTestCase(isOutbound bool) remoteSignerTestCase {
	tName := "shared key"
	if isOutbound {
		tName += " outbound"
	}

	return remoteSignerTestCase{
		name: tName,
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			runDeriveSharedKey(tt, wo)
		},
	}
}

func testRemoteSignerSharedKey(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(ht, sharedKeyTestCase(false), false)
}

func testRemoteSignerSharedKeyOutbound(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(ht, sharedKeyTestCase(true), true)
}

func bumpFeeTestCase(isOutbound bool) remoteSignerTestCase {
	tName := "bumpfee"
	if isOutbound {
		tName += " outbound"
	}

	return remoteSignerTestCase{
		name:      tName,
		sendCoins: true,
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			runBumpFee(tt, wo)
		},
	}
}

func testRemoteSignerBumpFee(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(ht, bumpFeeTestCase(false), false)
}

func testRemoteSignerBumpFeeOutbound(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(ht, bumpFeeTestCase(true), true)
}

func psbtTestCase(ht *lntest.HarnessTest,
	isOutbound bool) remoteSignerTestCase {

	tName := "psbt"
	if isOutbound {
		tName += " outbound"
	}

	return remoteSignerTestCase{
		name:       tName,
		randomSeed: true,
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			runPsbtChanFundingWithNodes(
				tt, carol, wo, false,
				lnrpc.CommitmentType_LEGACY,
			)
			runSignPsbtSegWitV0P2WKH(tt, wo)
			runSignPsbtSegWitV1KeySpendBip86(tt, wo)
			runSignPsbtSegWitV1KeySpendRootHash(tt, wo)
			runSignPsbtSegWitV1ScriptSpend(tt, wo)

			// The above tests all make sure we can sign for keys
			// that aren't in the wallet. But we also want to make
			// sure we can fund and then sign PSBTs from our wallet.
			runFundAndSignPsbt(ht, wo)
		},
	}
}

func testRemoteSignerPSBT(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(ht, psbtTestCase(ht, false), false)
}

func testRemoteSignerPSBTOutbound(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(ht, psbtTestCase(ht, true), true)
}

func signOutputRawTestCase(isOutbound bool) remoteSignerTestCase {
	tName := "sign output raw"
	if isOutbound {
		tName += " outbound"
	}

	return remoteSignerTestCase{
		name:      tName,
		sendCoins: true,
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			runSignOutputRaw(tt, wo)
		},
	}
}

func testRemoteSignerSignOutputRaw(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(ht, signOutputRawTestCase(false), false)
}

func testRemoteSignerSignOutputRawOutbound(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(ht, signOutputRawTestCase(true), true)
}

func signVerifyMsgTestCase(isOutbound bool) remoteSignerTestCase {
	tName := "sign verify msg"
	if isOutbound {
		tName += " outbound"
	}
	return remoteSignerTestCase{
		name:      tName,
		sendCoins: true,
		fn: func(tt *lntest.HarnessTest, wo, carol *node.HarnessNode) {
			runSignVerifyMessage(tt, wo)
		},
	}
}

func testRemoteSignerSignVerifyMsg(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(ht, signVerifyMsgTestCase(false), false)
}

func testRemoteSignerSignVerifyMsgOutbound(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(ht, signVerifyMsgTestCase(true), true)
}

func taprootTestCase(isOutbound bool) remoteSignerTestCase {
	tName := "taproot"
	if isOutbound {
		tName += " outbound"
	}
	return remoteSignerTestCase{
		name:       tName,
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
	}
}

func testRemoteSignerTaproot(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(ht, taprootTestCase(false), false)
}

func testRemoteSignerTaprootOutbound(ht *lntest.HarnessTest) {
	executeRemoteSignerTestCase(ht, taprootTestCase(true), true)
}

// testOutboundRSMacaroonEnforcement tests that a valid macaroon including
// the `remotesigner` entity is required to connect to a watch-only node that
// uses an outbound remote signer, while the watch-only node is in the state
// where it waits for the signer to connect.
func testOutboundRSMacaroonEnforcement(ht *lntest.HarnessTest) {
	// Ensure that the watch-only node uses a configuration that requires an
	// outbound remote signer during startup.
	watchOnlyArgs := []string{
		"--remotesigner.enable",
		"--remotesigner.allowinboundconnection",
		"--remotesigner.timeout=15s",
		"--remotesigner.requesttimeout=15s",
	}

	// Create the watch-only node. Note that we require authentication for
	// the watch-only node, as we want to test that the macaroon enforcement
	// works as expected.
	watchOnly := ht.CreateNewNode("WatchOnly", watchOnlyArgs, nil, false)

	startChan := make(chan error)

	// Start the watch-only node in a goroutine as it requires a remote
	// signer to connect before it can fully start.
	go func() {
		startChan <- watchOnly.Start(ht.Context())
	}()

	// Wait and ensure that the watch-only node reaches the state where
	// it waits for the remote signer to connect, as this is the state where
	// we want to test the macaroon enforcement.
	err := wait.Predicate(func() bool {
		if watchOnly.RPC == nil {
			return false
		}

		state, err := watchOnly.RPC.State.GetState(
			ht.Context(), &lnrpc.GetStateRequest{},
		)
		if err != nil {
			return false
		}

		return state.State == lnrpc.WalletState_ALLOW_REMOTE_SIGNER
	}, 5*time.Second)
	require.NoError(ht, err)

	// Set up a connection to the watch-only node. However, instead of using
	// the watch-only node's admin macaroon, we'll use the invoice macaroon.
	// The connection should not be allowed using this macaroon because it
	// lacks the `remotesigner` entity required when the signer node
	// connects to the watch-only node.
	connectionCfg := lncfg.ConnectionCfg{
		RPCHost:        watchOnly.Cfg.RPCAddr(),
		MacaroonPath:   watchOnly.Cfg.InvoiceMacPath,
		TLSCertPath:    watchOnly.Cfg.TLSCertPath,
		Timeout:        10 * time.Second,
		RequestTimeout: 10 * time.Second,
	}

	streamFeeder := rpcwallet.NewStreamFeeder(connectionCfg)

	stream, err := streamFeeder.GetStream(ht.Context())
	require.NoError(ht, err)

	defer func() {
		require.NoError(ht, stream.Close())
	}()

	// Since we're using an unauthorized macaroon, we should expect to be
	// denied access to the watch-only node.
	_, err = stream.Recv()
	require.ErrorContains(ht, err, "permission denied")

	// Finally, connect a real signer to the watch-only node so that
	// it can start up properly.
	signerArgs := []string{
		"--watchonlynode.enable",
		"--watchonlynode.timeout=30s",
		"--watchonlynode.requesttimeout=10s",
		fmt.Sprintf(
			"--watchonlynode.rpchost=localhost:%d",
			watchOnly.Cfg.RPCPort,
		),
		fmt.Sprintf(
			"--watchonlynode.tlscertpath=%s",
			watchOnly.Cfg.TLSCertPath,
		),
		fmt.Sprintf(
			"--watchonlynode.macaroonpath=%s",
			watchOnly.Cfg.AdminMacPath, // An authorized macaroon.
		),
	}

	_ = ht.NewNode("Signer", signerArgs)

	// Finally, wait and ensure that the watch-only node is able to start
	// up properly.
	err = <-startChan
	require.NoError(ht, err, "Shouldn't error on watch-only node startup")
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
