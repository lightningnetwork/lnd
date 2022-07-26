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
		Name:     "external channel funding",
		TestFunc: testExternalFundingChanPoint,
	},
}
