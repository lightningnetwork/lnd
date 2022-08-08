//go:build rpctest
// +build rpctest

package itest

var allTestCases = []*testCase{
	{
		name: "single hop invoice",
		test: testSingleHopInvoice,
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
		name: "query routes",
		test: testQueryRoutes,
	},
	{
		name: "route fee cutoff",
		test: testRouteFeeCutoff,
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
		name: "bake macaroon",
		test: testBakeMacaroon,
	},
	{
		name: "delete macaroon id",
		test: testDeleteMacaroonID,
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
		name: "sendtoroute multi path payment",
		test: testSendToRouteMultiPath,
	},
	{
		name: "sendtoroute amp",
		test: testSendToRouteAMP,
	},
	{
		name: "sendpayment amp",
		test: testSendPaymentAMP,
	},
	{
		name: "sendpayment amp invoice",
		test: testSendPaymentAMPInvoice,
	},
	{
		name: "sendpayment amp invoice repeat",
		test: testSendPaymentAMPInvoiceRepeat,
	},
	{
		name: "send multi path payment",
		test: testSendMultiPathPayment,
	},
	{
		name: "forward interceptor",
		test: testForwardInterceptorBasic,
	},
	{
		name: "forward interceptor dedup htlcs",
		test: testForwardInterceptorDedupHtlc,
	},
	{
		name: "stateless init",
		test: testStatelessInit,
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
		name: "rpc middleware interceptor",
		test: testRPCMiddlewareInterceptor,
	},
	{
		name: "wipe forwarding packages",
		test: testWipeForwardingPackages,
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
		name: "zero conf channel open",
		test: testZeroConfChannelOpen,
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
