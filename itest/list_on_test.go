//go:build integration

package itest

import (
	"fmt"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntest"
)

var allTestCases = []*lntest.TestCase{
	{
		Name:     "update channel status",
		TestFunc: testUpdateChanStatus,
	},
	{
		Name:     "external channel funding",
		TestFunc: testExternalFundingChanPoint,
	},
	{
		Name:     "channel backup restore unconfirmed",
		TestFunc: testChannelBackupRestoreUnconfirmed,
	},
	{
		Name:     "channel backup restore force close",
		TestFunc: testChannelBackupRestoreForceClose,
	},
	{
		Name:     "channel backup restore legacy",
		TestFunc: testChannelBackupRestoreLegacy,
	},
	{
		Name:     "data loss protection",
		TestFunc: testDataLossProtection,
	},
	{
		Name:     "send all coins",
		TestFunc: testSendAllCoins,
	},
	{
		Name:     "send selected coins",
		TestFunc: testSendSelectedCoins,
	},
	{
		Name:     "send selected coins channel reserve",
		TestFunc: testSendSelectedCoinsChannelReserve,
	},
	{
		Name:     "reorg notifications",
		TestFunc: testReorgNotifications,
	},
	{
		Name:     "disconnecting target peer",
		TestFunc: testDisconnectingTargetPeer,
	},
	{
		Name:     "sphinx replay persistence",
		TestFunc: testSphinxReplayPersistence,
	},
	{
		Name:     "funding expiry blocks on pending",
		TestFunc: testFundingExpiryBlocksOnPending,
	},
	{
		Name:     "pending channel confirmation until active",
		TestFunc: testPendingChannelConfirmationUntilActive,
	},
	{
		Name:     "pending channel reorg test",
		TestFunc: testPendingChannelAfterReorg,
	},
	{
		Name:     "list channels",
		TestFunc: testListChannels,
	},
	{
		Name:     "max pending channel",
		TestFunc: testMaxPendingChannels,
	},
	{
		Name:     "garbage collect link nodes",
		TestFunc: testGarbageCollectLinkNodes,
	},
	{
		Name:     "reject onward htlc",
		TestFunc: testRejectHTLC,
	},
	{
		Name:     "node sign verify",
		TestFunc: testNodeSignVerify,
	},
	{
		Name:     "list addresses",
		TestFunc: testListAddresses,
	},
	{
		Name:     "abandonchannel",
		TestFunc: testAbandonChannel,
	},
	{
		Name:     "recovery info",
		TestFunc: testGetRecoveryInfo,
	},
	{
		Name:     "onchain fund recovery",
		TestFunc: testOnchainFundRecovery,
	},
	{
		Name:     "wallet rescan address detection",
		TestFunc: testRescanAddressDetection,
	},
	{
		Name:     "basic funding flow with all input types",
		TestFunc: testChannelFundingInputTypes,
	},
	{
		Name:     "unconfirmed channel funding",
		TestFunc: testUnconfirmedChannelFunding,
	},
	{
		Name:     "funding flow persistence",
		TestFunc: testChannelFundingPersistence,
	},
	{
		Name:     "batch channel funding",
		TestFunc: testBatchChanFunding,
	},
	{
		Name:     "open channel with unstable utxos",
		TestFunc: testChannelFundingWithUnstableUtxos,
	},
	{
		Name:     "open psbt channel with unstable utxos",
		TestFunc: testPsbtChanFundingWithUnstableUtxos,
	},
	{
		Name:     "update channel policy",
		TestFunc: testUpdateChannelPolicy,
	},
	{
		Name:     "send update disable channel",
		TestFunc: testSendUpdateDisableChannel,
	},
	{
		Name:     "connection timeout",
		TestFunc: testNetworkConnectionTimeout,
	},
	{
		Name:     "reconnect after ip change",
		TestFunc: testReconnectAfterIPChange,
	},
	{
		Name:     "addpeer config",
		TestFunc: testAddPeerConfig,
	},
	{
		Name:     "private channel update policy",
		TestFunc: testUpdateChannelPolicyForPrivateChannel,
	},
	{
		Name:     "update channel policy fee rate accuracy",
		TestFunc: testUpdateChannelPolicyFeeRateAccuracy,
	},
	{
		Name:     "unannounced channels",
		TestFunc: testUnannouncedChannels,
	},
	{
		Name:     "graph topology notifications",
		TestFunc: testGraphTopologyNotifications,
	},
	{
		Name:     "node announcement",
		TestFunc: testNodeAnnouncement,
	},
	{
		Name:     "update node announcement rpc",
		TestFunc: testUpdateNodeAnnouncement,
	},
	{
		Name:     "self node announcement persistence",
		TestFunc: testSelfNodeAnnouncementPersistence,
	},
	{
		Name:     "list payments",
		TestFunc: testListPayments,
	},
	{
		Name:     "send direct payment anchor",
		TestFunc: testSendDirectPaymentAnchor,
	},
	{
		Name:     "send direct payment simple taproot",
		TestFunc: testSendDirectPaymentSimpleTaproot,
	},
	{
		Name:     "immediate payment after channel opened",
		TestFunc: testPaymentFollowingChannelOpen,
	},
	{
		Name:     "payment failure reason canceled",
		TestFunc: testPaymentFailureReasonCanceled,
	},
	{
		Name:     "invoice update subscription",
		TestFunc: testInvoiceSubscriptions,
	},
	{
		Name:     "streaming channel backup update",
		TestFunc: testChannelBackupUpdates,
	},
	{
		Name:     "export channel backup",
		TestFunc: testExportChannelBackup,
	},
	{
		Name:     "channel balance",
		TestFunc: testChannelBalance,
	},
	{
		Name:     "channel unsettled balance",
		TestFunc: testChannelUnsettledBalance,
	},
	{
		Name:     "chain kit",
		TestFunc: testChainKit,
	},
	{
		Name:     "neutrino kit",
		TestFunc: testNeutrino,
	},
	{
		Name:     "etcd failover",
		TestFunc: testEtcdFailover,
	},
	{
		Name:     "leader health check",
		TestFunc: testLeaderHealthCheck,
	},
	{
		Name:     "hold invoice force close",
		TestFunc: testHoldInvoiceForceClose,
	},
	{
		Name:     "hold invoice sender persistence",
		TestFunc: testHoldInvoicePersistence,
	},
	{
		Name:     "maximum channel size",
		TestFunc: testMaxChannelSize,
	},
	{
		Name:     "wumbo channels",
		TestFunc: testWumboChannels,
	},
	{
		Name:     "max htlc path payment",
		TestFunc: testMaxHtlcPathPayment,
	},
	{
		Name:     "max htlc pathfind",
		TestFunc: testMaxHtlcPathfind,
	},
	{
		Name:     "multi-hop htlc error propagation",
		TestFunc: testHtlcErrorPropagation,
	},
	{
		Name:     "multi-hop payments",
		TestFunc: testMultiHopPayments,
	},
	{
		Name:     "estimate route fee",
		TestFunc: testEstimateRouteFee,
	},
	{
		Name:     "anchors reserved value",
		TestFunc: testAnchorReservedValue,
	},
	{
		Name:     "3rd party anchor spend",
		TestFunc: testAnchorThirdPartySpend,
	},
	{
		Name:     "open channel reorg test",
		TestFunc: testOpenChannelAfterReorg,
	},
	{
		Name:     "open channel with shutdown address",
		TestFunc: testOpenChannelWithShutdownAddr,
	},
	{
		Name:     "sign psbt",
		TestFunc: testSignPsbt,
	},
	{
		Name:     "fund psbt",
		TestFunc: testFundPsbt,
	},
	{
		Name:     "fund psbt custom lock",
		TestFunc: testFundPsbtCustomLock,
	},
	{
		Name:     "resolution handoff",
		TestFunc: testResHandoff,
	},
	{
		Name:     "REST API",
		TestFunc: testRestAPI,
	},
	{
		Name:     "revoked uncooperative close retribution",
		TestFunc: testRevokedCloseRetribution,
	},
	{
		Name: "revoked uncooperative close retribution zero value " +
			"remote output",
		TestFunc: testRevokedCloseRetributionZeroValueRemoteOutput,
	},
	{
		Name:     "revoked uncooperative close retribution remote hodl",
		TestFunc: testRevokedCloseRetributionRemoteHodl,
	},
	{
		Name:     "multi-hop send to route",
		TestFunc: testMultiHopSendToRoute,
	},
	{
		Name:     "send to route error propagation",
		TestFunc: testSendToRouteErrorPropagation,
	},
	{
		Name:     "private channels",
		TestFunc: testPrivateChannels,
	},
	{
		Name:     "invoice routing hints",
		TestFunc: testInvoiceRoutingHints,
	},
	{
		Name:     "scid alias routing hints",
		TestFunc: testScidAliasRoutingHints,
	},
	{
		Name:     "multi-hop payments over private channels",
		TestFunc: testMultiHopOverPrivateChannels,
	},
	{
		Name:     "query routes",
		TestFunc: testQueryRoutes,
	},
	{
		Name:     "route fee cutoff",
		TestFunc: testRouteFeeCutoff,
	},
	{
		Name:     "route fee limit after queryroutes",
		TestFunc: testFeeLimitAfterQueryRoutes,
	},
	{
		Name:     "rpc middleware interceptor",
		TestFunc: testRPCMiddlewareInterceptor,
	},
	{
		Name:     "macaroon authentication",
		TestFunc: testMacaroonAuthentication,
	},
	{
		Name:     "bake macaroon",
		TestFunc: testBakeMacaroon,
	},
	{
		Name:     "delete macaroon id",
		TestFunc: testDeleteMacaroonID,
	},
	{
		Name:     "stateless init",
		TestFunc: testStatelessInit,
	},
	{
		Name:     "single hop invoice",
		TestFunc: testSingleHopInvoice,
	},
	{
		Name:     "wipe forwarding packages",
		TestFunc: testWipeForwardingPackages,
	},
	{
		Name:     "switch circuit persistence",
		TestFunc: testSwitchCircuitPersistence,
	},
	{
		Name:     "switch offline delivery",
		TestFunc: testSwitchOfflineDelivery,
	},
	{
		Name:     "switch offline delivery persistence",
		TestFunc: testSwitchOfflineDeliveryPersistence,
	},
	{
		Name:     "switch offline delivery outgoing offline",
		TestFunc: testSwitchOfflineDeliveryOutgoingOffline,
	},
	{
		Name:     "sendtoroute multi path payment",
		TestFunc: testSendToRouteMultiPath,
	},
	{
		Name:     "send to route fail payment notification",
		TestFunc: testSendToRouteFailPaymentNotification,
	},
	{
		Name:     "send multi path payment",
		TestFunc: testSendMultiPathPayment,
	},
	{
		Name:     "sendpayment amp invoice",
		TestFunc: testSendPaymentAMPInvoice,
	},
	{
		Name:     "sendpayment amp invoice repeat",
		TestFunc: testSendPaymentAMPInvoiceRepeat,
	},
	{
		Name:     "send payment amp",
		TestFunc: testSendPaymentAMP,
	},
	{
		Name:     "sendtoroute amp",
		TestFunc: testSendToRouteAMP,
	},
	{
		Name:     "send payment keysend mpp fail",
		TestFunc: testSendPaymentKeysendMPPFail,
	},
	{
		Name:     "forward interceptor dedup htlcs",
		TestFunc: testForwardInterceptorDedupHtlc,
	},
	{
		Name:     "forward interceptor",
		TestFunc: testForwardInterceptorBasic,
	},
	{
		Name:     "forward interceptor restart",
		TestFunc: testForwardInterceptorRestart,
	},
	{
		Name:     "invoice HTLC modifier basic",
		TestFunc: testInvoiceHtlcModifierBasic,
	},
	{
		Name:     "zero conf channel open",
		TestFunc: testZeroConfChannelOpen,
	},
	{
		Name:     "option scid alias",
		TestFunc: testOptionScidAlias,
	},
	{
		Name:     "scid alias upgrade",
		TestFunc: testOptionScidUpgrade,
	},
	{
		Name:     "nonstd sweep",
		TestFunc: testNonstdSweep,
	},
	{
		Name:     "multiple channel creation and update subscription",
		TestFunc: testBasicChannelCreationAndUpdates,
	},
	{
		Name:     "derive shared key",
		TestFunc: testDeriveSharedKey,
	},
	{
		Name:     "sign output raw",
		TestFunc: testSignOutputRaw,
	},
	{
		Name:     "sign verify message",
		TestFunc: testSignVerifyMessage,
	},
	{
		Name:     "bumpfee",
		TestFunc: testBumpFee,
	},
	{
		Name:     "bumpfee external input",
		TestFunc: testBumpFeeExternalInput,
	},
	{
		Name:     "bumpforceclosefee",
		TestFunc: testBumpForceCloseFee,
	},
	{
		Name:     "taproot spend",
		TestFunc: testTaprootSpend,
	},
	{
		Name:     "taproot musig2",
		TestFunc: testTaprootMuSig2,
	},
	{
		Name:     "taproot import scripts",
		TestFunc: testTaprootImportScripts,
	},
	{
		Name:     "simple taproot channel activation",
		TestFunc: testSimpleTaprootChannelActivation,
	},
	{
		Name:     "wallet import pubkey",
		TestFunc: testWalletImportPubKey,
	},
	{
		Name:     "async payments benchmark",
		TestFunc: testAsyncPayments,
	},
	{
		Name:     "taproot coop close",
		TestFunc: testTaprootCoopClose,
	},
	{
		Name:     "trackpayments",
		TestFunc: testTrackPayments,
	},
	{
		Name:     "trackpayments compatible",
		TestFunc: testTrackPaymentsCompatible,
	},
	{
		Name:     "custom message",
		TestFunc: testCustomMessage,
	},
	{
		Name:     "onion message",
		TestFunc: testOnionMessage,
	},
	{
		Name:     "sign verify message with addr",
		TestFunc: testSignVerifyMessageWithAddr,
	},
	{
		Name:     "zero conf reorg edge existence",
		TestFunc: testZeroConfReorg,
	},
	{
		Name:     "async bidirectional payments",
		TestFunc: testBidirectionalAsyncPayments,
	},
	{
		Name:     "lookup htlc resolution",
		TestFunc: testLookupHtlcResolution,
	},
	{
		Name:     "channel fundmax error",
		TestFunc: testChannelFundMaxError,
	},
	{
		Name:     "channel fundmax wallet amount",
		TestFunc: testChannelFundMaxWalletAmount,
	},
	{
		Name:     "channel fundmax anchor reserve",
		TestFunc: testChannelFundMaxAnchorReserve,
	},
	{
		Name:     "htlc timeout resolver extract preimage remote",
		TestFunc: testHtlcTimeoutResolverExtractPreimageRemote,
	},
	{
		Name:     "htlc timeout resolver extract preimage local",
		TestFunc: testHtlcTimeoutResolverExtractPreimageLocal,
	},
	{
		Name:     "custom features",
		TestFunc: testCustomFeatures,
	},
	{
		Name:     "update pending open channels on funder side",
		TestFunc: testUpdateOnFunderPendingOpenChannels,
	},
	{
		Name:     "update pending open channels on fundee side",
		TestFunc: testUpdateOnFundeePendingOpenChannels,
	},
	{
		Name:     "blinded payment htlc re-forward",
		TestFunc: testBlindedPaymentHTLCReForward,
	},
	{
		Name:     "query blinded route",
		TestFunc: testQueryBlindedRoutes,
	},
	{
		Name:     "route blinding invoices",
		TestFunc: testBlindedRouteInvoices,
	},
	{
		Name:     "receiver blinded error",
		TestFunc: testReceiverBlindedError,
	},
	{
		Name:     "relayer blinded error",
		TestFunc: testRelayingBlindedError,
	},
	{
		Name:     "introduction blinded error",
		TestFunc: testIntroductionNodeError,
	},
	{
		Name:     "disable introduction node",
		TestFunc: testDisableIntroductionNode,
	},
	{
		Name:     "on chain to blinded",
		TestFunc: testErrorHandlingOnChainFailure,
	},
	{
		Name:     "mpp to single blinded path",
		TestFunc: testMPPToSingleBlindedPath,
	},
	{
		Name:     "mpp to multiple blinded paths",
		TestFunc: testMPPToMultipleBlindedPaths,
	},
	{
		Name:     "route blinding dummy hops",
		TestFunc: testBlindedRouteDummyHops,
	},
	{
		Name:     "removetx",
		TestFunc: testRemoveTx,
	},
	{
		Name:     "listsweeps",
		TestFunc: testListSweeps,
	},
	{
		Name:     "fail funding flow psbt",
		TestFunc: testPsbtChanFundingFailFlow,
	},
	{
		Name:     "coop close with htlcs",
		TestFunc: testCoopCloseWithHtlcs,
	},
	{
		Name:     "coop close with htlcs restart",
		TestFunc: testCoopCloseWithHtlcsWithRestart,
	},
	{
		Name:     "coop close exceeds max fee",
		TestFunc: testCoopCloseExceedsMaxFee,
	},
	{
		Name:     "open channel locked balance",
		TestFunc: testOpenChannelLockedBalance,
	},
	{
		Name:     "sweep cpfp anchor outgoing timeout",
		TestFunc: testSweepCPFPAnchorOutgoingTimeout,
	},
	{
		Name:     "sweep cpfp anchor incoming timeout",
		TestFunc: testSweepCPFPAnchorIncomingTimeout,
	},
	{
		Name:     "sweep htlcs",
		TestFunc: testSweepHTLCs,
	},
	{
		Name:     "sweep commit output and anchor",
		TestFunc: testSweepCommitOutputAndAnchor,
	},
	{
		Name:     "payment failed htlc local swept",
		TestFunc: testPaymentFailedHTLCLocalSwept,
	},
	{
		Name:     "payment failed htlc local swept resumed",
		TestFunc: testPaymentFailedHTLCLocalSweptResumed,
	},
	{
		Name:     "payment succeeded htlc remote swept",
		TestFunc: testPaymentSucceededHTLCRemoteSwept,
	},
	{
		Name:     "send to route failed htlc timeout",
		TestFunc: testSendToRouteFailHTLCTimeout,
	},
	{
		Name:     "send to route failed htlc timeout resumed",
		TestFunc: testSendToRouteFailHTLCTimeoutResumed,
	},
	{
		Name:     "debuglevel show",
		TestFunc: testDebuglevelShow,
	},
	{
		Name:     "experimental endorsement",
		TestFunc: testExperimentalEndorsement,
	},
	{
		Name:     "quiescence",
		TestFunc: testQuiescence,
	},
	{
		Name:     "invoice migration",
		TestFunc: testInvoiceMigration,
	},
	{
		Name:     "graph migration",
		TestFunc: testGraphMigration,
	},
	{
		Name:     "payment address mismatch",
		TestFunc: testWrongPaymentAddr,
	},
	{
		Name:     "fee replacement",
		TestFunc: testFeeReplacement,
	},
	{
		Name:     "funding manager funding timeout",
		TestFunc: testFundingManagerFundingTimeout,
	},
	{
		Name:     "rbf coop close",
		TestFunc: testCoopCloseRbf,
	},
	{
		Name:     "rbf coop close disconnect",
		TestFunc: testRBFCoopCloseDisconnect,
	},
	{
		Name:     "bump fee low budget",
		TestFunc: testBumpFeeLowBudget,
	},
	{
		Name:     "partially specified route blinded invoice",
		TestFunc: testPartiallySpecifiedBlindedPath,
	},
	{
		Name:     "peer bootstrapping",
		TestFunc: testPeerBootstrapping,
	},
	{
		Name:     "grpc not found",
		TestFunc: testGRPCNotFound,
	},
	{
		Name:     "delete canceled invoice",
		TestFunc: testDeleteCanceledInvoice,
	},
	{
		Name:     "estimate fee",
		TestFunc: testEstimateFee,
	},
}

// appendPrefixed is used to add a prefix to each test name in the subtests
// before appending them to the main test cases.
func appendPrefixed(prefix string, testCases,
	subtestCases []*lntest.TestCase) []*lntest.TestCase {

	for _, tc := range subtestCases {
		name := fmt.Sprintf("%s-%s", prefix, tc.Name)
		testCases = append(testCases, &lntest.TestCase{
			Name:     name,
			TestFunc: tc.TestFunc,
		})
	}

	return testCases
}

// extractNames is used to extract tests' names from a group of prefixed tests.
func extractNames(prefix string, subtestCases []*lntest.TestCase) []string {
	return fn.Map(subtestCases, func(tc *lntest.TestCase) string {
		return fmt.Sprintf("%s-%s", prefix, tc.Name)
	})
}

func init() {
	// Register subtests.
	allTestCases = appendPrefixed(
		"multihop", allTestCases, multiHopForceCloseTestCases,
	)
	allTestCases = appendPrefixed(
		"watchtower", allTestCases, watchtowerTestCases,
	)
	allTestCases = appendPrefixed(
		"psbt", allTestCases, psbtFundingTestCases,
	)
	allTestCases = appendPrefixed(
		"remote signer", allTestCases, remoteSignerTestCases,
	)
	allTestCases = appendPrefixed(
		"channel backup", allTestCases, channelRestoreTestCases,
	)
	allTestCases = appendPrefixed(
		"utxo selection", allTestCases, fundUtxoSelectionTestCases,
	)
	allTestCases = appendPrefixed(
		"zero conf", allTestCases, zeroConfPolicyTestCases,
	)
	allTestCases = appendPrefixed(
		"channel fee policy", allTestCases, channelFeePolicyTestCases,
	)
	allTestCases = appendPrefixed(
		"wallet import account", allTestCases,
		walletImportAccountTestCases,
	)
	allTestCases = appendPrefixed(
		"funding", allTestCases, basicFundingTestCases,
	)
	allTestCases = appendPrefixed(
		"send to route", allTestCases, sendToRouteTestCases,
	)
	allTestCases = appendPrefixed(
		"channel force close", allTestCases, channelForceCloseTestCases,
	)
	allTestCases = appendPrefixed(
		"wallet", allTestCases, walletTestCases,
	)
	allTestCases = appendPrefixed(
		"coop close with external delivery", allTestCases,
		coopCloseWithExternalTestCases,
	)
	allTestCases = appendPrefixed(
		"peer conn", allTestCases, peerConnTestCases,
	)

	// Prepare the test cases for windows to exclude some of the flaky
	// ones.
	//
	// NOTE: We need to run this before the isWindowsOS check to make sure
	// the excluded tests are found in allTestCases. Otherwise, if a
	// non-existing test is included in excludedTestsWindows, we won't be
	// able to find it until it's pushed to the CI, which creates a much
	// longer feedback loop.
	windowsTestCases := filterWindowsFlakyTests()

	// If this is Windows, we'll skip running some of the flaky tests.
	if isWindowsOS() {
		allTestCases = windowsTestCases
	}
}
