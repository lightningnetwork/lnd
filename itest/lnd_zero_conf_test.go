package itest

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/aliasmgr"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/rpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// zeroConfPolicyTestCases checks that option-scid-alias, zero-conf
// channel-types, and option-scid-alias feature-bit-only channels have the
// expected graph and that payments work when updating the channel policy.
var zeroConfPolicyTestCases = []*lntest.TestCase{
	{
		Name: "channel policy update private",
		TestFunc: func(ht *lntest.HarnessTest) {
			// zeroConf: false
			// scidAlias: false
			// private: true
			testPrivateUpdateAlias(
				ht, false, false, true,
			)
		},
	},
	{
		Name: "channel policy update private scid alias",
		TestFunc: func(ht *lntest.HarnessTest) {
			// zeroConf: false
			// scidAlias: true
			// private: true
			testPrivateUpdateAlias(
				ht, false, true, true,
			)
		},
	},
	{
		Name: "channel policy update private zero conf",
		TestFunc: func(ht *lntest.HarnessTest) {
			// zeroConf: true
			// scidAlias: false
			// private: true
			testPrivateUpdateAlias(
				ht, true, false, true,
			)
		},
	},
	{
		Name: "channel policy update public zero conf",
		TestFunc: func(ht *lntest.HarnessTest) {
			// zeroConf: true
			// scidAlias: false
			// private: false
			testPrivateUpdateAlias(
				ht, true, false, false,
			)
		},
	},
	{
		Name: "channel policy update public",
		TestFunc: func(ht *lntest.HarnessTest) {
			// zeroConf: false
			// scidAlias: false
			// private: false
			testPrivateUpdateAlias(
				ht, false, false, false,
			)
		},
	},
}

// testZeroConfChannelOpen tests that opening a zero-conf channel works and
// sending payments also works.
func testZeroConfChannelOpen(ht *lntest.HarnessTest) {
	// Since option-scid-alias is opt-in, the provided harness nodes will
	// not have the feature bit set. Also need to set anchors as those are
	// default-off in itests.
	scidAliasArgs := []string{
		"--protocol.option-scid-alias",
		"--protocol.zero-conf",
		"--protocol.anchors",
	}

	bob := ht.NewNode("Bob", nil)

	// We'll give Bob some coins in order to fund the channel.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, bob)

	carol := ht.NewNode("Carol", scidAliasArgs)
	ht.EnsureConnected(bob, carol)

	// We'll open a regular public channel between Bob and Carol here.
	chanAmt := btcutil.Amount(1_000_000)
	p := lntest.OpenChannelParams{
		Amt: chanAmt,
	}
	ht.OpenChannel(bob, carol, p)

	// Spin-up Dave so Carol can open a zero-conf channel to him.
	dave := ht.NewNode("Dave", scidAliasArgs)

	// We'll give Carol some coins in order to fund the channel.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// Ensure that both Carol and Dave are connected.
	ht.EnsureConnected(carol, dave)

	// Setup a ChannelAcceptor for Dave.
	acceptStream, cancel := dave.RPC.ChannelAcceptor()
	go acceptChannel(ht.T, true, acceptStream)

	// Open a private zero-conf anchors channel of 1M satoshis.
	params := lntest.OpenChannelParams{
		Amt:            chanAmt,
		Private:        true,
		CommitmentType: lnrpc.CommitmentType_ANCHORS,
		ZeroConf:       true,
	}
	stream := ht.OpenChannelAssertStream(carol, dave, params)

	// Remove the ChannelAcceptor.
	cancel()

	// We should receive the OpenStatusUpdate_ChanOpen update without
	// having to mine any blocks.
	fundingPoint2 := ht.WaitForChannelOpenEvent(stream)

	ht.AssertChannelInGraph(carol, fundingPoint2)
	ht.AssertChannelInGraph(dave, fundingPoint2)

	// Attempt to send a 10K satoshi payment from Carol to Dave.
	daveInvoiceParams := &lnrpc.Invoice{
		Value:   int64(10_000),
		Private: true,
	}
	daveInvoiceResp := dave.RPC.AddInvoice(daveInvoiceParams)
	ht.CompletePaymentRequests(
		carol, []string{daveInvoiceResp.PaymentRequest},
	)

	// Now attempt to send a multi-hop payment from Bob to Dave. This tests
	// that Dave issues an invoice with an alias SCID that Carol knows and
	// uses to forward to Dave.
	daveInvoiceResp2 := dave.RPC.AddInvoice(daveInvoiceParams)
	ht.CompletePaymentRequests(
		bob, []string{daveInvoiceResp2.PaymentRequest},
	)

	// Check that Dave has a zero-conf alias SCID in the graph.
	descReq := &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: true,
	}

	err := waitForZeroConfGraphChange(dave, descReq, true)
	require.NoError(ht, err)

	// We'll now confirm the zero-conf channel between Carol and Dave and
	// assert that sending is still possible.
	block := ht.MineBlocksAndAssertNumTxes(6, 1)[0]

	// Dave should still have the alias edge in his db.
	err = waitForZeroConfGraphChange(dave, descReq, true)
	require.NoError(ht, err)

	fundingTxID := ht.GetChanPointFundingTxid(fundingPoint2)

	ht.AssertTxInBlock(block, fundingTxID)

	daveInvoiceResp3 := dave.RPC.AddInvoice(daveInvoiceParams)
	ht.CompletePaymentRequests(
		bob, []string{daveInvoiceResp3.PaymentRequest},
	)

	// Eve will now initiate a zero-conf channel with Carol. This tests
	// that the ChannelUpdates sent are correct since they will be
	// referring to different alias SCIDs.
	eve := ht.NewNode("Eve", scidAliasArgs)
	ht.EnsureConnected(eve, carol)

	// Give Eve some coins to fund the channel.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, eve)

	// Setup a ChannelAcceptor.
	acceptStream, cancel = carol.RPC.ChannelAcceptor()
	go acceptChannel(ht.T, true, acceptStream)

	// We'll open a public zero-conf anchors channel of 1M satoshis.
	params.Private = false
	stream = ht.OpenChannelAssertStream(eve, carol, params)

	// Remove the ChannelAcceptor.
	cancel()

	// Wait to receive the OpenStatusUpdate_ChanOpen update.
	fundingPoint3 := ht.WaitForChannelOpenEvent(stream)

	ht.AssertChannelInGraph(eve, fundingPoint3)
	ht.AssertChannelInGraph(carol, fundingPoint3)

	// Attempt to send a 20K satoshi payment from Eve to Dave.
	daveInvoiceParams.Value = int64(20_000)
	daveInvoiceResp4 := dave.RPC.AddInvoice(daveInvoiceParams)
	ht.CompletePaymentRequests(
		eve, []string{daveInvoiceResp4.PaymentRequest},
	)

	// Assert that Eve has stored the zero-conf alias in her graph.
	err = waitForZeroConfGraphChange(eve, descReq, true)
	require.NoError(ht, err, "expected to not receive error")

	// We'll confirm the zero-conf channel between Eve and Carol and assert
	// that sending is still possible.
	block = ht.MineBlocksAndAssertNumTxes(6, 1)[0]

	fundingTxID = ht.GetChanPointFundingTxid(fundingPoint3)
	ht.AssertTxInBlock(block, fundingTxID)

	// Wait until Eve's ZeroConf channel is replaced by the confirmed SCID
	// in her graph.
	err = waitForZeroConfGraphChange(eve, descReq, false)
	require.NoError(ht, err, "expected to not receive error")

	// Attempt to send a 6K satoshi payment from Dave to Eve.
	eveInvoiceParams := &lnrpc.Invoice{
		Value:   int64(6_000),
		Private: true,
	}
	eveInvoiceResp := eve.RPC.AddInvoice(eveInvoiceParams)

	// Assert that route hints is empty since the channel is public.
	payReq := eve.RPC.DecodePayReq(eveInvoiceResp.PaymentRequest)
	require.Len(ht, payReq.RouteHints, 0)

	// Make sure Dave is aware of this channel and send the payment.
	ht.AssertChannelInGraph(dave, fundingPoint3)
	ht.CompletePaymentRequests(
		dave, []string{eveInvoiceResp.PaymentRequest},
	)
}

// testOptionScidAlias checks that opening an option_scid_alias channel-type
// channel or w/o the channel-type works properly.
func testOptionScidAlias(ht *lntest.HarnessTest) {
	type scidTestCase struct {
		name string

		// If this is false, then the channel will be a regular non
		// channel-type option-scid-alias-feature-bit channel.
		chantype bool

		private bool
	}

	var testCases = []scidTestCase{
		{
			name:     "private chan-type",
			chantype: true,
			private:  true,
		},
		{
			name:     "public no chan-type",
			chantype: false,
			private:  false,
		},
		{
			name:     "private no chan-type",
			chantype: false,
			private:  true,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		success := ht.Run(testCase.name, func(t *testing.T) {
			st := ht.Subtest(t)
			optionScidAliasScenario(
				st, testCase.chantype, testCase.private,
			)
		})
		if !success {
			break
		}
	}
}

func optionScidAliasScenario(ht *lntest.HarnessTest, chantype, private bool) {
	// Option-scid-alias is opt-in, as is anchors.
	scidAliasArgs := []string{
		"--protocol.option-scid-alias",
		"--protocol.anchors",
	}

	bob := ht.NewNode("Bob", nil)

	// We'll give Bob some coins in order to fund the channel.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, bob)

	carol := ht.NewNode("Carol", scidAliasArgs)
	dave := ht.NewNode("Dave", scidAliasArgs)

	// Ensure Bob, Carol are connected.
	ht.EnsureConnected(bob, carol)

	// Ensure Carol, Dave are connected.
	ht.EnsureConnected(carol, dave)

	// Give Carol some coins so she can open the channel.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	chanAmt := btcutil.Amount(1_000_000)

	params := lntest.OpenChannelParams{
		Amt:            chanAmt,
		Private:        private,
		CommitmentType: lnrpc.CommitmentType_ANCHORS,
		ScidAlias:      chantype,
	}
	fundingPoint := ht.OpenChannel(carol, dave, params)

	// Make sure Bob knows this channel if it's public.
	if !private {
		ht.AssertChannelInGraph(bob, fundingPoint)
	}

	// Assert that a payment from Carol to Dave works as expected.
	daveInvoiceParams := &lnrpc.Invoice{
		Value:   int64(10_000),
		Private: true,
	}
	daveInvoiceResp := dave.RPC.AddInvoice(daveInvoiceParams)
	ht.CompletePaymentRequests(
		carol, []string{daveInvoiceResp.PaymentRequest},
	)

	// We'll now open a regular public channel between Bob and Carol and
	// assert that Bob can pay Dave. We'll also assert that the invoice
	// Dave issues has the startingAlias as a hop hint.
	p := lntest.OpenChannelParams{
		Amt: chanAmt,
	}
	fundingPoint2 := ht.OpenChannel(bob, carol, p)

	// Wait until Dave receives the Bob<->Carol channel.
	ht.AssertChannelInGraph(dave, fundingPoint2)

	daveInvoiceResp2 := dave.RPC.AddInvoice(daveInvoiceParams)
	decodedReq := dave.RPC.DecodePayReq(daveInvoiceResp2.PaymentRequest)

	if !private {
		require.Len(ht, decodedReq.RouteHints, 0)
		payReq := daveInvoiceResp2.PaymentRequest
		ht.CompletePaymentRequests(bob, []string{payReq})

		return
	}

	require.Len(ht, decodedReq.RouteHints, 1)
	require.Len(ht, decodedReq.RouteHints[0].HopHints, 1)

	startingAlias := lnwire.ShortChannelID{
		BlockHeight: 16_000_000,
		TxIndex:     0,
		TxPosition:  0,
	}

	daveHopHint := decodedReq.RouteHints[0].HopHints[0].ChanId
	require.Equal(ht, startingAlias.ToUint64(), daveHopHint)

	ht.CompletePaymentRequests(
		bob, []string{daveInvoiceResp2.PaymentRequest},
	)
}

// waitForZeroConfGraphChange waits for the zero-conf channel to be visible in
// the graph after confirmation or not. The expect argument denotes whether the
// zero-conf is expected in the graph or not. There should always be at least
// one channel of the passed HarnessNode, zero-conf or not.
func waitForZeroConfGraphChange(hn *node.HarnessNode,
	req *lnrpc.ChannelGraphRequest, expect bool) error {

	return wait.NoError(func() error {
		graph := hn.RPC.DescribeGraph(req)

		if expect {
			// If we expect a zero-conf channel, we'll assert that
			// one exists, both policies exist, and we are party to
			// the channel.
			for _, e := range graph.Edges {
				// The BlockHeight will be less than 16_000_000
				// if this is not a zero-conf channel.
				scid := lnwire.NewShortChanIDFromInt(
					e.ChannelId,
				)
				if scid.BlockHeight < 16_000_000 {
					continue
				}

				// Both edge policies must exist in the zero
				// conf case.
				if e.Node1Policy == nil ||
					e.Node2Policy == nil {

					continue
				}

				// Check if we are party to the zero-conf
				// channel.
				if e.Node1Pub == hn.PubKeyStr ||
					e.Node2Pub == hn.PubKeyStr {

					return nil
				}
			}

			return errors.New("failed to find zero-conf channel " +
				"in graph")
		}

		// If we don't expect a zero-conf channel, we'll assert that
		// none exist, that we have a non-zero-conf channel with at
		// both policies, and one of the policies in the database is
		// ours.
		for _, e := range graph.Edges {
			scid := lnwire.NewShortChanIDFromInt(e.ChannelId)
			if scid.BlockHeight == 16_000_000 {
				return errors.New("found zero-conf channel")
			}

			// One of the edge policies must exist.
			if e.Node1Policy == nil || e.Node2Policy == nil {
				continue
			}

			// If we are part of this channel, exit gracefully.
			if e.Node1Pub == hn.PubKeyStr ||
				e.Node2Pub == hn.PubKeyStr {

				return nil
			}
		}

		return errors.New(
			"failed to find non-zero-conf channel in graph",
		)
	}, defaultTimeout)
}

func testPrivateUpdateAlias(ht *lntest.HarnessTest,
	zeroConf, scidAliasType, private bool) {

	// We'll create a new node Eve that will not have option-scid-alias
	// channels.
	eve := ht.NewNode("Eve", nil)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, eve)

	// Since option-scid-alias is opt-in we'll need to specify the protocol
	// arguments when creating a new node.
	scidAliasArgs := []string{
		"--protocol.option-scid-alias",
		"--protocol.zero-conf",
		"--protocol.anchors",
	}
	carol := ht.NewNode("Carol", scidAliasArgs)

	// Spin-up Dave who will have an option-scid-alias feature-bit-only or
	// channel-type channel with Carol.
	dave := ht.NewNode("Dave", scidAliasArgs)

	// We'll give Carol some coins in order to fund the channel.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// Ensure that Carol and Dave are connected.
	ht.EnsureConnected(carol, dave)

	// We'll open a regular public channel between Eve and Carol here. Eve
	// will be the one receiving the onion-encrypted ChannelUpdate.
	ht.EnsureConnected(eve, carol)

	chanAmt := btcutil.Amount(1_000_000)

	p := lntest.OpenChannelParams{
		Amt:     chanAmt,
		PushAmt: chanAmt / 2,
	}
	fundingPoint := ht.OpenChannel(eve, carol, p)

	// Make sure Dave has seen this public channel.
	ht.AssertChannelInGraph(dave, fundingPoint)

	// Setup a ChannelAcceptor for Dave.
	acceptStream, cancel := dave.RPC.ChannelAcceptor()
	go acceptChannel(ht.T, zeroConf, acceptStream)

	// Open a private channel, optionally specifying a channel-type.
	params := lntest.OpenChannelParams{
		Amt:            chanAmt,
		Private:        private,
		CommitmentType: lnrpc.CommitmentType_ANCHORS,
		ZeroConf:       zeroConf,
		ScidAlias:      scidAliasType,
		PushAmt:        chanAmt / 2,
	}
	fundingPoint2 := ht.OpenChannelNoAnnounce(carol, dave, params)

	// Remove the ChannelAcceptor.
	cancel()

	// Carol will now update the channel edge policy for her channel with
	// Dave.
	baseFeeMSat := 33000
	feeRate := int64(5)
	timeLockDelta := uint32(chainreg.DefaultBitcoinTimeLockDelta)
	updateFeeReq := &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   int64(baseFeeMSat),
		FeeRate:       float64(feeRate),
		TimeLockDelta: timeLockDelta,
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: fundingPoint2,
		},
	}
	carol.RPC.UpdateChannelPolicy(updateFeeReq)

	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMsat:      int64(baseFeeMSat),
		FeeRateMilliMsat: testFeeBase * feeRate,
		TimeLockDelta:    timeLockDelta,
		MinHtlc:          1000, // default value
		MaxHtlcMsat:      lntest.CalculateMaxHtlc(chanAmt),
	}

	// Assert that Dave receives Carol's policy update.
	ht.AssertChannelPolicyUpdate(
		dave, carol, expectedPolicy, fundingPoint2, true,
	)

	// Have Dave also update his policy.
	baseFeeMSat = 15000
	feeRate = int64(4)
	updateFeeReq = &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   int64(baseFeeMSat),
		FeeRate:       float64(feeRate),
		TimeLockDelta: timeLockDelta,
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: fundingPoint2,
		},
	}
	dave.RPC.UpdateChannelPolicy(updateFeeReq)

	expectedPolicy = &lnrpc.RoutingPolicy{
		FeeBaseMsat:      int64(baseFeeMSat),
		FeeRateMilliMsat: testFeeBase * feeRate,
		TimeLockDelta:    timeLockDelta,
		MinHtlc:          1000,
		MaxHtlcMsat:      lntest.CalculateMaxHtlc(chanAmt),
	}

	// Assert that Carol receives Dave's policy update.
	ht.AssertChannelPolicyUpdate(
		carol, dave, expectedPolicy, fundingPoint2, true,
	)

	// Assert that if Dave disables the channel, Carol sees it.
	disableReq := &routerrpc.UpdateChanStatusRequest{
		ChanPoint: fundingPoint2,
		Action:    routerrpc.ChanStatusAction_DISABLE,
	}
	dave.RPC.UpdateChanStatus(disableReq)

	expectedPolicy.Disabled = true
	ht.AssertChannelPolicyUpdate(
		carol, dave, expectedPolicy, fundingPoint2, true,
	)

	// Assert that if Dave enables the channel, Carol sees it.
	enableReq := &routerrpc.UpdateChanStatusRequest{
		ChanPoint: fundingPoint2,
		Action:    routerrpc.ChanStatusAction_ENABLE,
	}
	dave.RPC.UpdateChanStatus(enableReq)

	expectedPolicy.Disabled = false
	ht.AssertChannelPolicyUpdate(
		carol, dave, expectedPolicy, fundingPoint2, true,
	)

	// Create an invoice for Carol to pay.
	invoiceParams := &lnrpc.Invoice{
		Value:   int64(10_000),
		Private: true,
	}
	daveInvoiceResp := dave.RPC.AddInvoice(invoiceParams)

	// Carol will attempt to send Dave an HTLC.
	payReqs := []string{daveInvoiceResp.PaymentRequest}
	ht.CompletePaymentRequests(carol, payReqs)

	// Now Eve will create an invoice that Dave will pay.
	eveInvoiceResp := eve.RPC.AddInvoice(invoiceParams)
	payReqs = []string{eveInvoiceResp.PaymentRequest}
	ht.CompletePaymentRequests(dave, payReqs)

	// If this is a public channel, it won't be included in the hop hints,
	// so we'll mine enough for 6 confs here. We only expect a tx in the
	// mempool for the zero-conf case.
	if !private {
		var expectTx int
		if zeroConf {
			expectTx = 1
		}
		ht.MineBlocksAndAssertNumTxes(6, expectTx)

		// Sleep here so that the edge can be deleted and re-inserted.
		// This is necessary since the edge may have a policy for the
		// peer that is "correct" but has an invalid signature from the
		// PoV of BOLT#7.
		//
		// TODO(yy): further investigate this sleep.
		time.Sleep(time.Second * 5)

		// Make sure Eve has heard about this public channel.
		ht.AssertChannelInGraph(eve, fundingPoint2)
	}

	// Dave creates an invoice that Eve will pay.
	daveInvoiceResp2 := dave.RPC.AddInvoice(invoiceParams)

	// Carol then updates the channel policy again.
	feeRate = int64(2)
	updateFeeReq = &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   int64(baseFeeMSat),
		FeeRate:       float64(feeRate),
		TimeLockDelta: timeLockDelta,
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: fundingPoint2,
		},
	}
	carol.RPC.UpdateChannelPolicy(updateFeeReq)

	expectedPolicy = &lnrpc.RoutingPolicy{
		FeeBaseMsat:      int64(baseFeeMSat),
		FeeRateMilliMsat: testFeeBase * feeRate,
		TimeLockDelta:    timeLockDelta,
		MinHtlc:          1000,
		MaxHtlcMsat:      lntest.CalculateMaxHtlc(chanAmt),
	}

	// Assert Dave receives Carol's policy update.
	ht.AssertChannelPolicyUpdate(
		dave, carol, expectedPolicy, fundingPoint2, true,
	)

	// If the channel is public, check that Eve receives Carol's policy
	// update.
	if !private {
		ht.AssertChannelPolicyUpdate(
			eve, carol, expectedPolicy, fundingPoint2, true,
		)
	}

	// Eve will pay Dave's invoice and should use the updated base fee.
	payReqs = []string{daveInvoiceResp2.PaymentRequest}
	ht.CompletePaymentRequests(eve, payReqs)

	// Eve will issue an invoice that Dave will pay.
	eveInvoiceResp2 := eve.RPC.AddInvoice(invoiceParams)
	payReqs = []string{eveInvoiceResp2.PaymentRequest}
	ht.CompletePaymentRequests(dave, payReqs)

	// If this is a private channel, we'll mine 6 blocks here to test the
	// funding manager logic that deals with ChannelUpdates. If this is not
	// a zero-conf channel, we don't expect a tx in the mempool.
	if private {
		var expectTx int
		if zeroConf {
			expectTx = 1
		}
		ht.MineBlocksAndAssertNumTxes(6, expectTx)
	}

	// Dave will issue an invoice and Eve will pay it.
	daveInvoiceResp3 := dave.RPC.AddInvoice(invoiceParams)
	payReqs = []string{daveInvoiceResp3.PaymentRequest}
	ht.CompletePaymentRequests(eve, payReqs)

	// Carol will disable the channel, assert that Dave sees it and Eve as
	// well if the channel is public.
	carol.RPC.UpdateChanStatus(disableReq)

	expectedPolicy.Disabled = true
	ht.AssertChannelPolicyUpdate(
		dave, carol, expectedPolicy, fundingPoint2, true,
	)

	if !private {
		ht.AssertChannelPolicyUpdate(
			eve, carol, expectedPolicy, fundingPoint2, true,
		)
	}

	// Carol will enable the channel, assert the same as above.
	carol.RPC.UpdateChanStatus(enableReq)
	expectedPolicy.Disabled = false
	ht.AssertChannelPolicyUpdate(
		dave, carol, expectedPolicy, fundingPoint2, true,
	)

	if !private {
		ht.AssertChannelPolicyUpdate(
			eve, carol, expectedPolicy, fundingPoint2, true,
		)
	}

	// Dave will issue an invoice and Eve should pay it after Carol updates
	// her channel policy.
	daveInvoiceResp4 := dave.RPC.AddInvoice(invoiceParams)

	feeRate = int64(3)
	updateFeeReq = &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   int64(baseFeeMSat),
		FeeRate:       float64(feeRate),
		TimeLockDelta: timeLockDelta,
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: fundingPoint2,
		},
	}
	carol.RPC.UpdateChannelPolicy(updateFeeReq)

	expectedPolicy = &lnrpc.RoutingPolicy{
		FeeBaseMsat:      int64(baseFeeMSat),
		FeeRateMilliMsat: testFeeBase * feeRate,
		TimeLockDelta:    timeLockDelta,
		MinHtlc:          1000,
		MaxHtlcMsat:      lntest.CalculateMaxHtlc(chanAmt),
	}

	// Assert Dave and optionally Eve receives Carol's update.
	ht.AssertChannelPolicyUpdate(
		dave, carol, expectedPolicy, fundingPoint2, true,
	)

	if !private {
		ht.AssertChannelPolicyUpdate(
			eve, carol, expectedPolicy, fundingPoint2, true,
		)
	}

	payReqs = []string{daveInvoiceResp4.PaymentRequest}
	ht.CompletePaymentRequests(eve, payReqs)
}

// testOptionScidUpgrade tests that toggling the option-scid-alias feature bit
// correctly upgrades existing channels.
func testOptionScidUpgrade(ht *lntest.HarnessTest) {
	bob := ht.NewNodeWithCoins("Bob", nil)

	// Start carol with anchors only.
	carolArgs := []string{
		"--protocol.anchors",
	}
	carol := ht.NewNode("carol", carolArgs)

	// Start dave with anchors + scid-alias.
	daveArgs := []string{
		"--protocol.anchors",
		"--protocol.option-scid-alias",
	}
	dave := ht.NewNode("dave", daveArgs)

	// Give carol some coins.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// Ensure carol and are connected.
	ht.EnsureConnected(carol, dave)

	chanAmt := btcutil.Amount(1_000_000)

	p := lntest.OpenChannelParams{
		Amt:     chanAmt,
		PushAmt: chanAmt / 2,
		Private: true,
	}
	ht.OpenChannel(carol, dave, p)

	// Bob will open a channel to Carol now.
	ht.EnsureConnected(bob, carol)

	p = lntest.OpenChannelParams{
		Amt: chanAmt,
	}
	fundingPoint2 := ht.OpenChannel(bob, carol, p)

	// Make sure Dave knows this channel.
	ht.AssertChannelInGraph(dave, fundingPoint2)

	// Carol will now set the option-scid-alias feature bit and restart.
	carolArgs = append(carolArgs, "--protocol.option-scid-alias")
	ht.RestartNodeWithExtraArgs(carol, carolArgs)

	// Dave will create an invoice for Carol to pay, it should contain an
	// alias in the hop hints.
	daveParams := &lnrpc.Invoice{
		Value:   int64(10_000),
		Private: true,
	}

	var daveInvoice *lnrpc.AddInvoiceResponse

	var startingAlias lnwire.ShortChannelID
	startingAlias.BlockHeight = 16_000_000

	// TODO(yy): Carol and Dave will attempt to connect to each other
	// during restart. However, due to the race condition in peer
	// connection, they may both fail. Thus we need to ensure the
	// connection here. Once the race is fixed, we can remove this line.
	ht.EnsureConnected(dave, carol)

	err := wait.NoError(func() error {
		invoiceResp := dave.RPC.AddInvoice(daveParams)
		decodedReq := dave.RPC.DecodePayReq(invoiceResp.PaymentRequest)

		if len(decodedReq.RouteHints) != 1 {
			return fmt.Errorf("expected 1 route hint, got %v",
				decodedReq.RouteHints)
		}

		if len(decodedReq.RouteHints[0].HopHints) != 1 {
			return fmt.Errorf("expected 1 hop hint, got %v",
				len(decodedReq.RouteHints[0].HopHints))
		}

		hopHint := decodedReq.RouteHints[0].HopHints[0].ChanId
		if startingAlias.ToUint64() == hopHint {
			daveInvoice = invoiceResp
			return nil
		}

		return fmt.Errorf("unmatched alias, expected %v, got %v",
			startingAlias.ToUint64(), hopHint)
	}, defaultTimeout)
	require.NoError(ht, err)

	// Carol should be able to pay it.
	ht.CompletePaymentRequests(carol, []string{daveInvoice.PaymentRequest})

	// TODO(yy): remove this connection once the following bug is fixed.
	// When Carol restarts, she will try to make a persistent connection to
	// Bob. Meanwhile, Bob will also make a conn request as he notices the
	// connection is broken. If they make these conn requests at the same
	// time, they both have an outbound conn request, and will close the
	// inbound conn they receives, which ends up in no conn.
	ht.EnsureConnected(bob, carol)

	daveInvoice2 := dave.RPC.AddInvoice(daveParams)
	ht.CompletePaymentRequests(bob, []string{daveInvoice2.PaymentRequest})
}

// acceptChannel is used to accept a single channel that comes across. This
// should be run in a goroutine and is used to test nodes with the zero-conf
// feature bit.
func acceptChannel(t *testing.T, zeroConf bool, stream rpc.AcceptorClient) {
	t.Helper()

	req, err := stream.Recv()
	require.NoError(t, err)

	resp := &lnrpc.ChannelAcceptResponse{
		Accept:        true,
		PendingChanId: req.PendingChanId,
		ZeroConf:      zeroConf,
	}
	err = stream.Send(resp)
	require.NoError(t, err)
}

// testZeroConfReorg tests that a reorg does not cause a zero-conf channel to
// be deleted from the channel graph. This was previously the case due to logic
// in the function DisconnectBlockAtHeight.
func testZeroConfReorg(ht *lntest.HarnessTest) {
	if ht.IsNeutrinoBackend() {
		ht.Skipf("skipping zero-conf reorg test for neutrino backend")
	}

	// Since zero-conf is opt in, the harness nodes provided won't be able
	// to open zero-conf channels. In that case, we just spin up new nodes.
	zeroConfArgs := []string{
		"--protocol.option-scid-alias",
		"--protocol.zero-conf",
		"--protocol.anchors",
	}

	carol := ht.NewNode("Carol", zeroConfArgs)

	// Spin-up Dave so Carol can open a zero-conf channel to him.
	dave := ht.NewNode("Dave", zeroConfArgs)

	// We'll give Carol some coins in order to fund the channel.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// Ensure that both Carol and Dave are connected.
	ht.EnsureConnected(carol, dave)

	// Setup a ChannelAcceptor for Dave.
	acceptStream, cancel := dave.RPC.ChannelAcceptor()
	go acceptChannel(ht.T, true, acceptStream)

	// Open a private zero-conf anchors channel of 1M satoshis.
	params := lntest.OpenChannelParams{
		Amt:            btcutil.Amount(1_000_000),
		CommitmentType: lnrpc.CommitmentType_ANCHORS,
		ZeroConf:       true,
	}
	_ = ht.OpenChannelNoAnnounce(carol, dave, params)

	// Remove the ChannelAcceptor.
	cancel()

	// Attempt to send a 10K satoshi payment from Carol to Dave. This
	// requires that the edge exists in the graph.
	daveInvoiceParams := &lnrpc.Invoice{
		Value: int64(10_000),
	}
	daveInvoiceResp := dave.RPC.AddInvoice(daveInvoiceParams)

	payReqs := []string{daveInvoiceResp.PaymentRequest}
	ht.CompletePaymentRequests(carol, payReqs)

	// We will now attempt to query for the alias SCID in Carol's graph.
	// We will query for the starting alias, which is exported by the
	// aliasmgr package.
	carol.RPC.GetChanInfo(&lnrpc.ChanInfoRequest{
		ChanId: aliasmgr.StartingAlias.ToUint64(),
	})

	// Now we will trigger a reorg and we'll assert that the edge still
	// exists in the graph.
	//
	// First, we'll setup a new miner that we can use to cause a reorg.
	tempMiner := ht.SpawnTempMiner()

	// We now cause a fork, by letting our original miner mine 1 block and
	// our new miner will mine 2. We also expect the funding transition to
	// be mined.
	ht.MineBlocksAndAssertNumTxes(1, 1)
	tempMiner.MineEmptyBlocks(2)

	// Ensure the temp miner is one block ahead.
	ht.AssertMinerBlockHeightDelta(tempMiner, 1)

	// Wait for Carol to sync to the original miner's chain.
	minerHeight := int32(ht.CurrentHeight())
	ht.WaitForNodeBlockHeight(carol, minerHeight)

	// Now we'll disconnect Carol's chain backend from the original miner
	// so that we can connect the two miners together and let the original
	// miner sync to the temp miner's chain.
	ht.DisconnectMiner()

	// Connecting to the temporary miner should cause the original miner to
	// reorg to the longer chain.
	ht.ConnectToMiner(tempMiner)

	// They should now be on the same chain.
	ht.AssertMinerBlockHeightDelta(tempMiner, 0)

	// Now we disconnect the two miners and reconnect our original chain
	// backend.
	ht.DisconnectFromMiner(tempMiner)

	ht.ConnectMiner()

	// This should have caused a reorg and Carol should sync to the new
	// chain.
	_, tempMinerHeight := tempMiner.GetBestBlock()
	ht.WaitForNodeBlockHeight(carol, tempMinerHeight)

	// Make sure all active nodes are synced.
	ht.AssertActiveNodesSynced()

	// Carol should have the channel once synced.
	carol.RPC.GetChanInfo(&lnrpc.ChanInfoRequest{
		ChanId: aliasmgr.StartingAlias.ToUint64(),
	})

	// Mine the zero-conf funding transaction so the test doesn't fail.
	ht.MineBlocksAndAssertNumTxes(1, 1)
}
