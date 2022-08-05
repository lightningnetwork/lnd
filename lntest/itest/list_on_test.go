//go:build rpctest
// +build rpctest

package itest

import "github.com/lightningnetwork/lnd/lntemp"

// TODO(yy): remove the temp.
var allTestCasesTemp = []*lntemp.TestCase{
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
}
