// +build rpctest

package itest

var testsCases = []*testCase{
	{
		name: "sweep coins",
		test: testSweepAllCoins,
	},
	{
		name: "onchain fund recovery",
		test: testOnchainFundRecovery,
	},
	{
		name: "basic funding flow",
		test: testBasicChannelFunding,
	},
	{
		name: "unconfirmed channel funding",
		test: testUnconfirmedChannelFunding,
	},
	{
		name: "update channel policy",
		test: testUpdateChannelPolicy,
	},
	{
		name: "open channel reorg test",
		test: testOpenChannelAfterReorg,
	},
	{
		name: "disconnecting target peer",
		test: testDisconnectingTargetPeer,
	},
	{
		name: "graph topology notifications",
		test: testGraphTopologyNotifications,
	},
	{
		name: "funding flow persistence",
		test: testChannelFundingPersistence,
	},
	{
		name: "channel force closure",
		test: testChannelForceClosure,
	},
	{
		name: "channel balance",
		test: testChannelBalance,
	},
	{
		name: "channel unsettled balance",
		test: testChannelUnsettledBalance,
	},
	{
		name: "single hop invoice",
		test: testSingleHopInvoice,
	},
	{
		name: "sphinx replay persistence",
		test: testSphinxReplayPersistence,
	},
	{
		name: "list outgoing payments",
		test: testListPayments,
	},
	{
		name: "max pending channel",
		test: testMaxPendingChannels,
	},
	{
		name: "multi-hop payments",
		test: testMultiHopPayments,
	},
	{
		name: "single-hop send to route",
		test: testSingleHopSendToRoute,
	},
	{
		name: "multi-hop send to route",
		test: testMultiHopSendToRoute,
	},
	{
		name: "send to route error propagation",
		test: testSendToRouteErrorPropagation,
	},
	{
		name: "unannounced channels",
		test: testUnannouncedChannels,
	},
	{
		name: "private channels",
		test: testPrivateChannels,
	},
	{
		name: "invoice routing hints",
		test: testInvoiceRoutingHints,
	},
	{
		name: "multi-hop payments over private channels",
		test: testMultiHopOverPrivateChannels,
	},
	{
		name: "multiple channel creation and update subscription",
		test: testBasicChannelCreationAndUpdates,
	},
	{
		name: "invoice update subscription",
		test: testInvoiceSubscriptions,
	},
	{
		name: "multi-hop htlc error propagation",
		test: testHtlcErrorPropagation,
	},
	{
		name: "reject onward htlc",
		test: testRejectHTLC,
	},
	// TODO(roasbeef): multi-path integration test
	{
		name: "node announcement",
		test: testNodeAnnouncement,
	},
	{
		name: "node sign verify",
		test: testNodeSignVerify,
	},
	{
		name: "async payments benchmark",
		test: testAsyncPayments,
	},
	{
		name: "async bidirectional payments",
		test: testBidirectionalAsyncPayments,
	},
	{
		// bob: outgoing our commit timeout
		// carol: incoming their commit watch and see timeout
		name: "test multi-hop htlc local force close immediate expiry",
		test: testMultiHopHtlcLocalTimeout,
	},
	{
		// bob: outgoing watch and see, they sweep on chain
		// carol: incoming our commit, know preimage
		name: "test multi-hop htlc receiver chain claim",
		test: testMultiHopReceiverChainClaim,
	},
	{
		// bob: outgoing our commit watch and see timeout
		// carol: incoming their commit watch and see timeout
		name: "test multi-hop local force close on-chain htlc timeout",
		test: testMultiHopLocalForceCloseOnChainHtlcTimeout,
	},
	{
		// bob: outgoing their commit watch and see timeout
		// carol: incoming our commit watch and see timeout
		name: "test multi-hop remote force close on-chain htlc timeout",
		test: testMultiHopRemoteForceCloseOnChainHtlcTimeout,
	},
	{
		// bob: outgoing our commit watch and see, they sweep on chain
		// bob: incoming our commit watch and learn preimage
		// carol: incoming their commit know preimage
		name: "test multi-hop htlc local chain claim",
		test: testMultiHopHtlcLocalChainClaim,
	},
	{
		// bob: outgoing their commit watch and see, they sweep on chain
		// bob: incoming their commit watch and learn preimage
		// carol: incoming our commit know preimage
		name: "test multi-hop htlc remote chain claim",
		test: testMultiHopHtlcRemoteChainClaim,
	},
	{
		name: "switch circuit persistence",
		test: testSwitchCircuitPersistence,
	},
	{
		name: "switch offline delivery",
		test: testSwitchOfflineDelivery,
	},
	{
		name: "switch offline delivery persistence",
		test: testSwitchOfflineDeliveryPersistence,
	},
	{
		name: "switch offline delivery outgoing offline",
		test: testSwitchOfflineDeliveryOutgoingOffline,
	},
	{
		// TODO(roasbeef): test always needs to be last as Bob's state
		// is borked since we trick him into attempting to cheat Alice?
		name: "revoked uncooperative close retribution",
		test: testRevokedCloseRetribution,
	},
	{
		name: "failing link",
		test: testFailingChannel,
	},
	{
		name: "garbage collect link nodes",
		test: testGarbageCollectLinkNodes,
	},
	{
		name: "abandonchannel",
		test: testAbandonChannel,
	},
	{
		name: "revoked uncooperative close retribution zero value remote output",
		test: testRevokedCloseRetributionZeroValueRemoteOutput,
	},
	{
		name: "revoked uncooperative close retribution remote hodl",
		test: testRevokedCloseRetributionRemoteHodl,
	},
	{
		name: "revoked uncooperative close retribution altruist watchtower",
		test: testRevokedCloseRetributionAltruistWatchtower,
	},
	{
		name: "data loss protection",
		test: testDataLossProtection,
	},
	{
		name: "query routes",
		test: testQueryRoutes,
	},
	{
		name: "route fee cutoff",
		test: testRouteFeeCutoff,
	},
	{
		name: "send update disable channel",
		test: testSendUpdateDisableChannel,
	},
	{
		name: "streaming channel backup update",
		test: testChannelBackupUpdates,
	},
	{
		name: "export channel backup",
		test: testExportChannelBackup,
	},
	{
		name: "channel backup restore",
		test: testChannelBackupRestore,
	},
	{
		name: "hold invoice sender persistence",
		test: testHoldInvoicePersistence,
	},
	{
		name: "cpfp",
		test: testCPFP,
	},
	{
		name: "macaroon authentication",
		test: testMacaroonAuthentication,
	},
	{
		name: "immediate payment after channel opened",
		test: testPaymentFollowingChannelOpen,
	},
	{
		name: "external channel funding",
		test: testExternalFundingChanPoint,
	},
}
