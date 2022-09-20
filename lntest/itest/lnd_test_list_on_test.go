//go:build rpctest
// +build rpctest

package itest

var allTestCases = []*testCase{
	{
		name: "async bidirectional payments",
		test: testBidirectionalAsyncPayments,
	},
	{
		name: "open channel fee policy",
		test: testOpenChannelUpdateFeePolicy,
	},
	{
		name: "custom messaging",
		test: testCustomMessage,
	},
}
