//go:build rpctest
// +build rpctest

package itest

import "github.com/lightningnetwork/lnd/lntest"

var allTestCases = []*lntest.TestCase{
	{
		Name:     "test multi-hop htlc",
		TestFunc: testMultiHopHtlcClaims,
	},

	// Recovery related tests.
	{
		Name:     "recovery info",
		TestFunc: testGetRecoveryInfo,
	},
	{
		Name:     "onchain fund recovery",
		TestFunc: testOnchainFundRecovery,
	},

	// Funding related tests.
	{
		Name:     "basic funding flow",
		TestFunc: testBasicChannelFunding,
	},
	{
		Name:     "unconfirmed channel funding",
		TestFunc: testUnconfirmedChannelFunding,
	},
	{
		Name:     "external channel funding",
		TestFunc: testExternalFundingChanPoint,
	},
	{
		Name:     "batch channel funding",
		TestFunc: testBatchChanFunding,
	},
	{
		Name:     "psbt channel funding",
		TestFunc: testPsbtChanFunding,
	},
	{
		Name:     "psbt channel funding external",
		TestFunc: testPsbtChanFundingExternal,
	},
	// {
	// 	name: "psbt channel funding single step",
	// 	test: testPsbtChanFundingSingleStep,
	// },

	// Channel Policy related tests.
	{
		Name:     "update channel policy",
		TestFunc: testUpdateChannelPolicy,
	},
	{
		Name:     "private channel update policy",
		TestFunc: testUpdateChannelPolicyForPrivateChannel,
	},
	{
		Name:     "send update disable channel",
		TestFunc: testSendUpdateDisableChannel,
	},
	{
		Name:     "funding flow persistence",
		TestFunc: testChannelFundingPersistence,
	},

	// Open channel related tests.
	{
		Name:     "open channel reorg test",
		TestFunc: testOpenChannelAfterReorg,
	},
	{
		Name:     "multiple channel creation and update subscription",
		TestFunc: testBasicChannelCreationAndUpdates,
	},

	// Connection related tests.
	{
		Name:     "disconnecting target peer",
		TestFunc: testDisconnectingTargetPeer,
	},

	// Onion related tests.
	{
		Name:     "sphinx replay persistence",
		TestFunc: testSphinxReplayPersistence,
	},

	// RPC endpoint tests.
	// This category focuses on testing the endpoints return the expected
	// response given different requests. Testing logic should be simple
	// and straight forward, that we only validate the responses by
	// altering the requests.
	{
		Name:     "list channels",
		TestFunc: testListChannels,
	},
	{
		Name:     "node sign verify",
		TestFunc: testNodeSignVerify,
	},
	{
		Name:     "sweep coins",
		TestFunc: testSweepAllCoins,
	},
	{
		Name:     "list outgoing payments",
		TestFunc: testListPayments,
	},
	{
		Name:     "derive shared key",
		TestFunc: testDeriveSharedKey,
	},

	// Node config related tests.
	{
		Name:     "max pending channel",
		TestFunc: testMaxPendingChannels,
	},
	{
		Name:     "reject onward htlc",
		TestFunc: testRejectHTLC,
	},
	{
		Name:     "connection timeout",
		TestFunc: testNetworkConnectionTimeout,
	},

	// Link related tests.
	{
		Name:     "garbage collect link nodes",
		TestFunc: testGarbageCollectLinkNodes,
	},

	// Channel backup relatd tests.
	{
		Name:     "data loss protection",
		TestFunc: testDataLossProtection,
	},
	{
		Name:     "channel backup restore",
		TestFunc: testChannelBackupRestore,
	},
	{
		Name:     "streaming channel backup update",
		TestFunc: testChannelBackupUpdates,
	},
	{
		Name:     "export channel backup",
		TestFunc: testExportChannelBackup,
	},

	// Misc - uncategorized tests.
	{
		Name:     "abandon channel",
		TestFunc: testAbandonChannel,
	},

	// Channel graph related tests.
	{
		Name:     "update channel status",
		TestFunc: testUpdateChanStatus,
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

	// Close channel related tests.
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

	// Channel balance related tests.
	{
		Name:     "channel balance",
		TestFunc: testChannelBalance,
	},
	{
		Name:     "channel unsettled balance",
		TestFunc: testChannelUnsettledBalance,
	},

	// Routing related tests.
	{
		Name:     "single hop invoice",
		TestFunc: testSingleHopInvoice,
	},
	{
		Name:     "multi-hop payments",
		TestFunc: testMultiHopPayments,
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

	// Payment related tests.
	{
		Name:     "immediate payment after channel open",
		TestFunc: testPaymentFollowingChannelOpen,
	},
	{
		Name:     "async payments benchmark",
		TestFunc: testAsyncPayments,
	},
	{
		Name:     "async bidirectional payments",
		TestFunc: testBidirectionalAsyncPayments,
	},
	{
		Name:     "invoice update subscription",
		TestFunc: testInvoiceSubscriptions,
	},
	{
		Name:     "hold invoice sender persistence",
		TestFunc: testHoldInvoicePersistence,
	},

	// HTLC related tests.
	{
		Name:     "multi-hop htlc error propagation",
		TestFunc: testHtlcErrorPropagation,
	},

	// Switch related tests.
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

	// Revocation related tests.
	{
		Name:     "revoked uncooperative close retribution",
		TestFunc: testRevokedCloseRetribution,
	},
	{
		Name:     "revoked uncooperative close retribution zero value remote output",
		TestFunc: testRevokedCloseRetributionZeroValueRemoteOutput,
	},
	{
		Name:     "revoked uncooperative close retribution remote hodl",
		TestFunc: testRevokedCloseRetributionRemoteHodl,
	},
	{
		Name:     "revoked uncooperative close retribution altruist watchtower",
		TestFunc: testRevokedCloseRetributionAltruistWatchtower,
	},
	{
		Name:     "hold invoice force close",
		TestFunc: testHoldInvoiceForceClose,
	},

	// Anchor related tests.
	{
		Name:     "cpfp",
		TestFunc: testCPFP,
	},
	{
		Name:     "anchors reserved value",
		TestFunc: testAnchorReservedValue,
	},

	// Macaroon related tests.
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

	// MPP and AMP related tests.
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

	// {
	// 	name: "sendtoroute amp",
	// 	test: testSendToRouteAMP,
	// },
	// {
	// 	name: "sendpayment amp",
	// 	test: testSendPaymentAMP,
	// },
	// {
	// {
	// 	name: "sendpayment amp invoice repeat",
	// 	test: testSendPaymentAMPInvoiceRepeat,
	// },
	// {
	// 	name: "reconnect after ip change",
	// 	test: testReconnectAfterIPChange,
	// },
	// {
	// 	name: "REST API",
	// 	test: testRestAPI,
	// },
	// {
	// 	name: "forward interceptor",
	// 	test: testForwardInterceptorBasic,
	// },
	// {
	// 	name: "forward interceptor dedup htlcs",
	// 	test: testForwardInterceptorDedupHtlc,
	// },
	// {
	// 	name: "wumbo channels",
	// 	test: testWumboChannels,
	// },
	// {
	// 	name: "maximum channel size",
	// 	test: testMaxChannelSize,
	// },
	// {
	// 	name: "wallet import account",
	// 	test: testWalletImportAccount,
	// },
	// {
	// 	name: "wallet import pubkey",
	// 	test: testWalletImportPubKey,
	// },
	// {
	// 	name: "etcd_failover",
	// 	test: testEtcdFailover,
	// },
	// {
	// 	name: "max htlc pathfind",
	// 	test: testMaxHtlcPathfind,
	// },
	// {
	// 	name: "rpc middleware interceptor",
	// 	test: testRPCMiddlewareInterceptor,
	// },
	// {
	// 	name: "wipe forwarding packages",
	// 	test: testWipeForwardingPackages,
	// },
	// {
	// 	name: "remote signer",
	// 	test: testRemoteSigner,
	// },
	// {
	// 	name: "sign psbt",
	// 	test: testSignPsbt,
	// },
	// {
	// 	name: "update channel policy fee rate accuracy",
	// 	test: testUpdateChannelPolicyFeeRateAccuracy,
	// },
	// {
	// 	name: "3rd party anchor spend",
	// 	test: testAnchorThirdPartySpend,
	// },
	// {
	// 	name: "sign output raw",
	// 	test: testSignOutputRaw,
	// },
	// {
	// 	name: "taproot",
	// 	test: testTaproot,
	// },
	// {
	// 	name: "resolution handoff",
	// 	test: testResHandoff,
	// },
	// {
	// 	name: "addpeer config",
	// 	test: testAddPeerConfig,
	// },
	// {
	// 	name: "basic funding flow with all input types",
	// 	test: testChannelFundingInputTypes,
	// },
}
