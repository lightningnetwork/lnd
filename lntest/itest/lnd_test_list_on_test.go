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

	// Channel config related tests.
	{
		Name:     "max pending channel",
		TestFunc: testMaxPendingChannels,
	},

	// Link related tests.
	{
		Name:     "garbage collect link nodes",
		TestFunc: testGarbageCollectLinkNodes,
	},

	// DLP(data loss protection) relatd tests.
	{
		Name:     "data loss protection",
		TestFunc: testDataLossProtection,
	},
	{
		Name:     "channel backup restore",
		TestFunc: testChannelBackupRestore,
	},

	// {
	// 	name: "reconnect after ip change",
	// 	test: testReconnectAfterIPChange,
	// },
	// {
	// 	name: "graph topology notifications",
	// 	test: testGraphTopologyNotifications,
	// },
	// {
	// 	name: "sweep coins",
	// 	test: testSweepAllCoins,
	// },
	// {
	// 	name: "channel force closure",
	// 	test: testChannelForceClosure,
	// },
	// {
	// 	name: "channel balance",
	// 	test: testChannelBalance,
	// },
	// {
	// 	name: "channel unsettled balance",
	// 	test: testChannelUnsettledBalance,
	// },
	// {
	// 	name: "single hop invoice",
	// 	test: testSingleHopInvoice,
	// },
	// {
	// 	name: "update channel status",
	// 	test: testUpdateChanStatus,
	// },
	// {
	// 	name: "list outgoing payments",
	// 	test: testListPayments,
	// },
	// {
	// 	name: "multi-hop payments",
	// 	test: testMultiHopPayments,
	// },
	// {
	// 	name: "single-hop send to route",
	// 	test: testSingleHopSendToRoute,
	// },
	// {
	// 	name: "multi-hop send to route",
	// 	test: testMultiHopSendToRoute,
	// },
	// {
	// 	name: "send to route error propagation",
	// 	test: testSendToRouteErrorPropagation,
	// },
	// {
	// 	name: "unannounced channels",
	// 	test: testUnannouncedChannels,
	// },
	// {
	// 	name: "private channels",
	// 	test: testPrivateChannels,
	// },
	// {
	// 	name: "invoice routing hints",
	// 	test: testInvoiceRoutingHints,
	// },
	// {
	// 	name: "multi-hop payments over private channels",
	// 	test: testMultiHopOverPrivateChannels,
	// },
	// {
	// 	name: "invoice update subscription",
	// 	test: testInvoiceSubscriptions,
	// },
	// {
	// 	name: "multi-hop htlc error propagation",
	// 	test: testHtlcErrorPropagation,
	// },
	// {
	// 	name: "reject onward htlc",
	// 	test: testRejectHTLC,
	// },
	// // TODO(roasbeef): multi-path integration test
	// {
	// 	name: "node announcement",
	// 	test: testNodeAnnouncement,
	// },
	// {
	// 	name: "node sign verify",
	// 	test: testNodeSignVerify,
	// },
	// {
	// 	name: "derive shared key",
	// 	test: testDeriveSharedKey,
	// },
	// {
	// 	name: "async payments benchmark",
	// 	test: testAsyncPayments,
	// },
	// {
	// 	name: "async bidirectional payments",
	// 	test: testBidirectionalAsyncPayments,
	// },
	// {
	// 	name: "switch circuit persistence",
	// 	test: testSwitchCircuitPersistence,
	// },
	// {
	// 	name: "switch offline delivery",
	// 	test: testSwitchOfflineDelivery,
	// },
	// {
	// 	name: "switch offline delivery persistence",
	// 	test: testSwitchOfflineDeliveryPersistence,
	// },
	// {
	// 	name: "switch offline delivery outgoing offline",
	// 	test: testSwitchOfflineDeliveryOutgoingOffline,
	// },
	// {
	// 	// TODO(roasbeef): test always needs to be last as Bob's state
	// 	// is borked since we trick him into attempting to cheat Alice?
	// 	name: "revoked uncooperative close retribution",
	// 	test: testRevokedCloseRetribution,
	// },
	// {
	// 	name: "failing link",
	// 	test: testFailingChannel,
	// },
	// {
	// 	name: "abandonchannel",
	// 	test: testAbandonChannel,
	// },
	// {
	// 	name: "revoked uncooperative close retribution zero value remote output",
	// 	test: testRevokedCloseRetributionZeroValueRemoteOutput,
	// },
	// {
	// 	name: "revoked uncooperative close retribution remote hodl",
	// 	test: testRevokedCloseRetributionRemoteHodl,
	// },
	// {
	// 	name: "revoked uncooperative close retribution altruist watchtower",
	// 	test: testRevokedCloseRetributionAltruistWatchtower,
	// },
	// {
	// 	name: "query routes",
	// 	test: testQueryRoutes,
	// },
	// {
	// 	name: "route fee cutoff",
	// 	test: testRouteFeeCutoff,
	// },
	// {
	// 	name: "streaming channel backup update",
	// 	test: testChannelBackupUpdates,
	// },
	// {
	// 	name: "export channel backup",
	// 	test: testExportChannelBackup,
	// },
	// {
	// 	name: "hold invoice sender persistence",
	// 	test: testHoldInvoicePersistence,
	// },
	// {
	// 	name: "hold invoice force close",
	// 	test: testHoldInvoiceForceClose,
	// },
	// {
	// 	name: "commitment deadline",
	// 	test: testCommitmentTransactionDeadline,
	// },
	// {
	// 	name: "cpfp",
	// 	test: testCPFP,
	// },
	// {
	// 	name: "anchors reserved value",
	// 	test: testAnchorReservedValue,
	// },
	// {
	// 	name: "macaroon authentication",
	// 	test: testMacaroonAuthentication,
	// },
	// {
	// 	name: "bake macaroon",
	// 	test: testBakeMacaroon,
	// },
	// {
	// 	name: "delete macaroon id",
	// 	test: testDeleteMacaroonID,
	// },
	// {
	// 	name: "immediate payment after channel opened",
	// 	test: testPaymentFollowingChannelOpen,
	// },
	// {
	// 	name: "psbt channel funding",
	// 	test: testPsbtChanFunding,
	// },
	// {
	// 	name: "psbt channel funding external",
	// 	test: testPsbtChanFundingExternal,
	// },
	// {
	// 	name: "psbt channel funding single step",
	// 	test: testPsbtChanFundingSingleStep,
	// },
	// {
	// 	name: "sendtoroute multi path payment",
	// 	test: testSendToRouteMultiPath,
	// },
	// {
	// 	name: "sendtoroute amp",
	// 	test: testSendToRouteAMP,
	// },
	// {
	// 	name: "sendpayment amp",
	// 	test: testSendPaymentAMP,
	// },
	// {
	// 	name: "sendpayment amp invoice",
	// 	test: testSendPaymentAMPInvoice,
	// },
	// {
	// 	name: "sendpayment amp invoice repeat",
	// 	test: testSendPaymentAMPInvoiceRepeat,
	// },
	// {
	// 	name: "send multi path payment",
	// 	test: testSendMultiPathPayment,
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
	// 	name: "connection timeout",
	// 	test: testNetworkConnectionTimeout,
	// },
	// {
	// 	name: "stateless init",
	// 	test: testStatelessInit,
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
	// 	name: "test update node announcement rpc",
	// 	test: testUpdateNodeAnnouncement,
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
