//go:build rpctest
// +build rpctest

package itest

var allTestCases = []*testCase{
	{
		name: "multiple channel creation and update subscription",
		test: testBasicChannelCreationAndUpdates,
	},
	{
		name: "derive shared key",
		test: testDeriveSharedKey,
	},
	{
		name: "sign output raw",
		test: testSignOutputRaw,
	},
	{
		name: "sign verify message",
		test: testSignVerifyMessage,
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
		name: "cpfp",
		test: testCPFP,
	},
	{
		name: "psbt channel funding",
		test: testPsbtChanFunding,
	},
	{
		name: "sign psbt",
		test: testSignPsbt,
	},
	{
		name: "wallet import account",
		test: testWalletImportAccount,
	},
	{
		name: "wallet import pubkey",
		test: testWalletImportPubKey,
	},
	{
		name: "remote signer",
		test: testRemoteSigner,
	},
	{
		name: "taproot",
		test: testTaproot,
	},
	{
		name: "option scid alias",
		test: testOptionScidAlias,
	},
	{
		name: "scid alias channel update",
		test: testUpdateChannelPolicyScidAlias,
	},
	{
		name: "scid alias upgrade",
		test: testOptionScidUpgrade,
	},
	{
		name: "nonstd sweep",
		test: testNonstdSweep,
	},
	{
		name: "taproot coop close",
		test: testTaprootCoopClose,
	},
	{
		name: "trackpayments",
		test: testTrackPayments,
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
