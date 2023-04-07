//go:build integration

package itest

import "github.com/lightningnetwork/lnd/lntest"

var allTestCases = []*lntest.TestCase{
	{
		Name:     "update channel status",
		TestFunc: testUpdateChanStatus,
	},
	{
		Name:     "basic funding flow",
		TestFunc: testBasicChannelFunding,
	},
	{
		Name:     "multi hop htlc local timeout",
		TestFunc: testMultiHopHtlcLocalTimeout,
	},
	{
		Name:     "multi hop receiver chain claim",
		TestFunc: testMultiHopReceiverChainClaim,
	},
	{
		Name:     "multi hop local force close on-chain htlc timeout",
		TestFunc: testMultiHopLocalForceCloseOnChainHtlcTimeout,
	},
	{
		Name:     "multi hop remote force close on-chain htlc timeout",
		TestFunc: testMultiHopRemoteForceCloseOnChainHtlcTimeout,
	},
	{
		Name:     "multi hop htlc local chain claim",
		TestFunc: testMultiHopHtlcLocalChainClaim,
	},
	{
		Name:     "multi hop htlc remote chain claim",
		TestFunc: testMultiHopHtlcRemoteChainClaim,
	},
	{
		Name:     "multi hop htlc aggregation",
		TestFunc: testMultiHopHtlcAggregation,
	},
	{
		Name:     "external channel funding",
		TestFunc: testExternalFundingChanPoint,
	},
	{
		Name:     "channel backup restore basic",
		TestFunc: testChannelBackupRestoreBasic,
	},
	{
		Name:     "channel backup restore unconfirmed",
		TestFunc: testChannelBackupRestoreUnconfirmed,
	},
	{
		Name:     "channel backup restore commit types",
		TestFunc: testChannelBackupRestoreCommitTypes,
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
		Name:     "sweep coins",
		TestFunc: testSweepAllCoins,
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
		Name:     "update channel policy",
		TestFunc: testUpdateChannelPolicy,
	},
	{
		Name:     "send update disable channel",
		TestFunc: testSendUpdateDisableChannel,
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
		Name:     "list outgoing payments",
		TestFunc: testListPayments,
	},
	{
		Name:     "immediate payment after channel opened",
		TestFunc: testPaymentFollowingChannelOpen,
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
		Name:     "commitment deadline",
		TestFunc: testCommitmentTransactionDeadline,
	},
	{
		Name:     "channel force closure",
		TestFunc: testChannelForceClosure,
	},
	{
		Name:     "failing link",
		TestFunc: testFailingChannel,
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
		Name:     "psbt channel funding",
		TestFunc: testPsbtChanFunding,
	},
	{
		Name:     "psbt channel funding external",
		TestFunc: testPsbtChanFundingExternal,
	},
	{
		Name:     "psbt channel funding single step",
		TestFunc: testPsbtChanFundingSingleStep,
	},
	{
		Name:     "sign psbt",
		TestFunc: testSignPsbt,
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
		Name: "revoked uncooperative close retribution altruist " +
			"watchtower",
		TestFunc: testRevokedCloseRetributionAltruistWatchtower,
	},
	{
		Name:     "single-hop send to route",
		TestFunc: testSingleHopSendToRoute,
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
		Name:     "forward interceptor dedup htlcs",
		TestFunc: testForwardInterceptorDedupHtlc,
	},
	{
		Name:     "forward interceptor",
		TestFunc: testForwardInterceptorBasic,
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
		Name:     "scid alias channel update",
		TestFunc: testUpdateChannelPolicyScidAlias,
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
		Name:     "cpfp",
		TestFunc: testCPFP,
	},
	{
		Name:     "taproot",
		TestFunc: testTaproot,
	},
	{
		Name:     "wallet import account",
		TestFunc: testWalletImportAccount,
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
		Name:     "remote signer",
		TestFunc: testRemoteSigner,
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
		Name:     "open channel fee policy",
		TestFunc: testOpenChannelUpdateFeePolicy,
	},
	{
		Name:     "custom message",
		TestFunc: testCustomMessage,
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
		Name:     "watchtower session management",
		TestFunc: testWatchtowerSessionManagement,
	},
	{
		Name:     "channel fundmax",
		TestFunc: testChannelFundMax,
	},
	{
		Name:     "htlc timeout resolver extract preimage remote",
		TestFunc: testHtlcTimeoutResolverExtractPreimageRemote,
	},
	{
		Name:     "htlc timeout resolver extract preimage local",
		TestFunc: testHtlcTimeoutResolverExtractPreimageLocal,
	},
}
