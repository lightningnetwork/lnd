//go:build rpctest
// +build rpctest

package itest

var allTestCases = []*testCase{
	{
		name: "async bidirectional payments",
		test: testBidirectionalAsyncPayments,
	},
}
