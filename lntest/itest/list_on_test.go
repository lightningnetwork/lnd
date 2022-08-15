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
		Name:     "channel backup restore",
		TestFunc: testChannelBackupRestore,
	},
	{
		Name:     "data loss protection",
		TestFunc: testDataLossProtection,
	},
}
