package itest

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntemp"
	"github.com/lightningnetwork/lnd/lntemp/node"
	"github.com/lightningnetwork/lnd/lntemp/rpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// testZeroConfChannelOpen tests that opening a zero-conf channel works and
// sending payments also works.
func testZeroConfChannelOpen(ht *lntemp.HarnessTest) {
	// Since option-scid-alias is opt-in, the provided harness nodes will
	// not have the feature bit set. Also need to set anchors as those are
	// default-off in itests.
	scidAliasArgs := []string{
		"--protocol.option-scid-alias",
		"--protocol.zero-conf",
		"--protocol.anchors",
	}

	bob := ht.Bob
	carol := ht.NewNode("Carol", scidAliasArgs)
	ht.EnsureConnected(bob, carol)

	// We'll open a regular public channel between Bob and Carol here.
	chanAmt := btcutil.Amount(1_000_000)
	p := lntemp.OpenChannelParams{
		Amt: chanAmt,
	}
	chanPoint := ht.OpenChannel(bob, carol, p)

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
	params := lntemp.OpenChannelParams{
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

	ht.AssertTopologyChannelOpen(carol, fundingPoint2)
	ht.AssertTopologyChannelOpen(dave, fundingPoint2)

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

	ht.Miner.AssertTxInBlock(block, fundingTxID)

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

	ht.AssertTopologyChannelOpen(eve, fundingPoint3)
	ht.AssertTopologyChannelOpen(carol, fundingPoint3)

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
	ht.Miner.AssertTxInBlock(block, fundingTxID)

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
	ht.AssertTopologyChannelOpen(dave, fundingPoint3)
	ht.CompletePaymentRequests(
		dave, []string{eveInvoiceResp.PaymentRequest},
	)

	// Close standby node's channels.
	ht.CloseChannel(bob, chanPoint)
}

// testOptionScidAlias checks that opening an option_scid_alias channel-type
// channel or w/o the channel-type works properly.
func testOptionScidAlias(net *lntest.NetworkHarness, t *harnessTest) {
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
		success := t.t.Run(testCase.name, func(t *testing.T) {
			h := newHarnessTest(t, net)
			optionScidAliasScenario(
				net, h, testCase.chantype, testCase.private,
			)
		})
		if !success {
			break
		}
	}
}

func optionScidAliasScenario(net *lntest.NetworkHarness, t *harnessTest,
	chantype, private bool) {

	ctxb := context.Background()

	// Option-scid-alias is opt-in, as is anchors.
	scidAliasArgs := []string{
		"--protocol.option-scid-alias",
		"--protocol.anchors",
	}

	carol := net.NewNode(t.t, "Carol", scidAliasArgs)
	defer shutdownAndAssert(net, t, carol)

	dave := net.NewNode(t.t, "Dave", scidAliasArgs)
	defer shutdownAndAssert(net, t, dave)

	// Ensure Bob, Carol are connected.
	net.EnsureConnected(t.t, net.Bob, carol)

	// Ensure Carol, Dave are connected.
	net.EnsureConnected(t.t, carol, dave)

	// Give Carol some coins so she can open the channel.
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, carol)

	chanAmt := btcutil.Amount(1_000_000)

	params := lntest.OpenChannelParams{
		Amt:            chanAmt,
		Private:        private,
		CommitmentType: lnrpc.CommitmentType_ANCHORS,
		ScidAlias:      chantype,
	}
	fundingPoint := openChannelAndAssert(t, net, carol, dave, params)

	if !private {
		err := net.Bob.WaitForNetworkChannelOpen(fundingPoint)
		require.NoError(t.t, err, "bob didn't report channel")
	}

	err := carol.WaitForNetworkChannelOpen(fundingPoint)
	require.NoError(t.t, err, "carol didn't report channel")
	err = dave.WaitForNetworkChannelOpen(fundingPoint)
	require.NoError(t.t, err, "dave didn't report channel")

	// Assert that a payment from Carol to Dave works as expected.
	daveInvoiceParams := &lnrpc.Invoice{
		Value:   int64(10_000),
		Private: true,
	}
	daveInvoiceResp, err := dave.AddInvoice(ctxb, daveInvoiceParams)
	require.NoError(t.t, err)
	_ = sendAndAssertSuccess(
		t, carol, &routerrpc.SendPaymentRequest{
			PaymentRequest: daveInvoiceResp.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)

	// We'll now open a regular public channel between Bob and Carol and
	// assert that Bob can pay Dave. We'll also assert that the invoice
	// Dave issues has the startingAlias as a hop hint.
	fundingPoint2 := openChannelAndAssert(
		t, net, net.Bob, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	err = net.Bob.WaitForNetworkChannelOpen(fundingPoint2)
	require.NoError(t.t, err)
	err = carol.WaitForNetworkChannelOpen(fundingPoint2)
	require.NoError(t.t, err)

	// Wait until Dave receives the Bob<->Carol channel.
	err = dave.WaitForNetworkChannelOpen(fundingPoint2)
	require.NoError(t.t, err)

	daveInvoiceResp2, err := dave.AddInvoice(ctxb, daveInvoiceParams)
	require.NoError(t.t, err)
	davePayReq := &lnrpc.PayReqString{
		PayReq: daveInvoiceResp2.PaymentRequest,
	}

	decodedReq, err := dave.DecodePayReq(ctxb, davePayReq)
	require.NoError(t.t, err)

	if !private {
		require.Equal(t.t, 0, len(decodedReq.RouteHints))
		payReq := daveInvoiceResp2.PaymentRequest
		_ = sendAndAssertSuccess(
			t, net.Bob, &routerrpc.SendPaymentRequest{
				PaymentRequest: payReq,
				TimeoutSeconds: 60,
				FeeLimitMsat:   noFeeLimitMsat,
			},
		)
		return
	}

	require.Equal(t.t, 1, len(decodedReq.RouteHints))
	require.Equal(t.t, 1, len(decodedReq.RouteHints[0].HopHints))

	startingAlias := lnwire.ShortChannelID{
		BlockHeight: 16_000_000,
		TxIndex:     0,
		TxPosition:  0,
	}

	daveHopHint := decodedReq.RouteHints[0].HopHints[0].ChanId
	require.Equal(t.t, startingAlias.ToUint64(), daveHopHint)

	_ = sendAndAssertSuccess(
		t, net.Bob, &routerrpc.SendPaymentRequest{
			PaymentRequest: daveInvoiceResp2.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
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

// testUpdateChannelPolicyScidAlias checks that option-scid-alias, zero-conf
// channel-types, and option-scid-alias feature-bit-only channels have the
// expected graph and that payments work when updating the channel policy.
func testUpdateChannelPolicyScidAlias(net *lntest.NetworkHarness,
	t *harnessTest) {

	tests := []struct {
		name string

		// The option-scid-alias channel type.
		scidAliasType bool

		// The zero-conf channel type.
		zeroConf bool

		private bool
	}{
		{
			name:          "private scid-alias chantype update",
			scidAliasType: true,
			private:       true,
		},
		{
			name:     "private zero-conf update",
			zeroConf: true,
			private:  true,
		},
		{
			name:     "public zero-conf update",
			zeroConf: true,
		},
		{
			name: "public no-chan-type update",
		},
		{
			name:    "private no-chan-type update",
			private: true,
		},
	}

	for _, test := range tests {
		test := test

		success := t.t.Run(test.name, func(t *testing.T) {
			ht := newHarnessTest(t, net)
			testPrivateUpdateAlias(
				net, ht, test.zeroConf, test.scidAliasType,
				test.private,
			)
		})
		if !success {
			return
		}
	}
}

func testPrivateUpdateAlias(net *lntest.NetworkHarness, t *harnessTest,
	zeroConf, scidAliasType, private bool) {

	ctxb := context.Background()
	defer ctxb.Done()

	// We'll create a new node Eve that will not have option-scid-alias
	// channels.
	eve := net.NewNode(t.t, "Eve", nil)
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, eve)
	defer shutdownAndAssert(net, t, eve)

	// Since option-scid-alias is opt-in we'll need to specify the protocol
	// arguments when creating a new node.
	scidAliasArgs := []string{
		"--protocol.option-scid-alias",
		"--protocol.zero-conf",
		"--protocol.anchors",
	}

	carol := net.NewNode(t.t, "Carol", scidAliasArgs)
	defer shutdownAndAssert(net, t, carol)

	// Spin-up Dave who will have an option-scid-alias feature-bit-only or
	// channel-type channel with Carol.
	dave := net.NewNode(t.t, "Dave", scidAliasArgs)
	defer shutdownAndAssert(net, t, dave)

	// We'll give Carol some coins in order to fund the channel.
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, carol)

	// Ensure that Carol and Dave are connected.
	net.EnsureConnected(t.t, carol, dave)

	// We'll open a regular public channel between Eve and Carol here. Eve
	// will be the one receiving the onion-encrypted ChannelUpdate.
	net.EnsureConnected(t.t, eve, carol)

	chanAmt := btcutil.Amount(1_000_000)

	fundingPoint := openChannelAndAssert(
		t, net, eve, carol,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: chanAmt / 2,
		},
	)
	defer closeChannelAndAssert(t, net, eve, fundingPoint, false)

	// Wait for all to view the channel as active.
	err := eve.WaitForNetworkChannelOpen(fundingPoint)
	require.NoError(t.t, err, "eve didn't report channel")
	err = carol.WaitForNetworkChannelOpen(fundingPoint)
	require.NoError(t.t, err, "carol didn't report channel")
	err = dave.WaitForNetworkChannelOpen(fundingPoint)
	require.NoError(t.t, err, "dave didn't report channel")

	// Setup a ChannelAcceptor for Dave.
	ctxc, cancel := context.WithCancel(ctxb)
	acceptStream, err := dave.ChannelAcceptor(ctxc)
	require.NoError(t.t, err)
	go acceptChannel(t.t, zeroConf, acceptStream)

	// Open a private channel, optionally specifying a channel-type.
	params := lntest.OpenChannelParams{
		Amt:            chanAmt,
		Private:        private,
		CommitmentType: lnrpc.CommitmentType_ANCHORS,
		ZeroConf:       zeroConf,
		ScidAlias:      scidAliasType,
		PushAmt:        chanAmt / 2,
	}
	chanOpenUpdate := openChannelStream(t, net, carol, dave, params)

	// Remove the ChannelAcceptor.
	cancel()

	if !zeroConf {
		// If this is not a zero-conf channel, mine a single block to
		// confirm the channel.
		_ = mineBlocks(t, net, 1, 1)
	}

	// Wait for both Carol and Dave to see the channel as open.
	fundingPoint2, err := net.WaitForChannelOpen(chanOpenUpdate)
	require.NoError(t.t, err, "error while waiting for channel open")

	err = carol.WaitForNetworkChannelOpen(fundingPoint2)
	require.NoError(t.t, err, "carol didn't report channel")
	err = dave.WaitForNetworkChannelOpen(fundingPoint2)
	require.NoError(t.t, err, "dave didn't report channel")

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
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	_, err = carol.UpdateChannelPolicy(ctxt, updateFeeReq)
	require.NoError(t.t, err, "unable to update chan policy")

	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMsat:      int64(baseFeeMSat),
		FeeRateMilliMsat: testFeeBase * feeRate,
		TimeLockDelta:    timeLockDelta,
		MinHtlc:          1000, // default value
		MaxHtlcMsat:      calculateMaxHtlc(chanAmt),
	}

	// Assert that Dave receives Carol's policy update.
	assertChannelPolicyUpdate(
		t.t, dave, carol.PubKeyStr, expectedPolicy, fundingPoint2,
		true,
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
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = dave.UpdateChannelPolicy(ctxt, updateFeeReq)
	require.NoError(t.t, err, "unable to update chan policy")

	expectedPolicy = &lnrpc.RoutingPolicy{
		FeeBaseMsat:      int64(baseFeeMSat),
		FeeRateMilliMsat: testFeeBase * feeRate,
		TimeLockDelta:    timeLockDelta,
		MinHtlc:          1000,
		MaxHtlcMsat:      calculateMaxHtlc(chanAmt),
	}

	// Assert that Carol receives Dave's policy update.
	assertChannelPolicyUpdate(
		t.t, carol, dave.PubKeyStr, expectedPolicy, fundingPoint2,
		true,
	)

	// Assert that if Dave disables the channel, Carol sees it.
	disableReq := &routerrpc.UpdateChanStatusRequest{
		ChanPoint: fundingPoint2,
		Action:    routerrpc.ChanStatusAction_DISABLE,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = dave.RouterClient.UpdateChanStatus(ctxt, disableReq)
	require.NoError(t.t, err)

	davePolicy := getChannelPolicies(
		t, carol, dave.PubKeyStr, fundingPoint2,
	)[0]
	davePolicy.Disabled = true
	assertChannelPolicyUpdate(
		t.t, carol, dave.PubKeyStr, davePolicy, fundingPoint2, true,
	)

	// Assert that if Dave enables the channel, Carol sees it.
	enableReq := &routerrpc.UpdateChanStatusRequest{
		ChanPoint: fundingPoint2,
		Action:    routerrpc.ChanStatusAction_ENABLE,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = dave.RouterClient.UpdateChanStatus(ctxt, enableReq)
	require.NoError(t.t, err)

	davePolicy.Disabled = false
	assertChannelPolicyUpdate(
		t.t, carol, dave.PubKeyStr, davePolicy, fundingPoint2, true,
	)

	// Create an invoice for Carol to pay.
	invoiceParams := &lnrpc.Invoice{
		Value:   int64(10_000),
		Private: true,
	}
	daveInvoiceResp, err := dave.AddInvoice(ctxb, invoiceParams)
	require.NoError(t.t, err, "unable to add invoice")

	// Carol will attempt to send Dave an HTLC.
	payReqs := []string{daveInvoiceResp.PaymentRequest}
	require.NoError(
		t.t, completePaymentRequests(
			carol, carol.RouterClient, payReqs, true,
		), "unable to send payment",
	)

	// Now Eve will create an invoice that Dave will pay.
	eveInvoiceResp, err := eve.AddInvoice(ctxb, invoiceParams)
	require.NoError(t.t, err, "unable to add invoice")

	payReqs = []string{eveInvoiceResp.PaymentRequest}
	require.NoError(
		t.t, completePaymentRequests(
			dave, dave.RouterClient, payReqs, true,
		), "unable to send payment",
	)

	// If this is a public channel, it won't be included in the hop hints,
	// so we'll mine enough for 6 confs here. We only expect a tx in the
	// mempool for the zero-conf case.
	if !private {
		var expectTx int
		if zeroConf {
			expectTx = 1
		}
		_ = mineBlocks(t, net, 6, expectTx)

		// Sleep here so that the edge can be deleted and re-inserted.
		// This is necessary since the edge may have a policy for the
		// peer that is "correct" but has an invalid signature from the
		// PoV of BOLT#7.
		time.Sleep(time.Second * 5)
	}

	// Dave creates an invoice that Eve will pay.
	daveInvoiceResp2, err := dave.AddInvoice(ctxb, invoiceParams)
	require.NoError(t.t, err, "unable to add invoice")

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
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = carol.UpdateChannelPolicy(ctxt, updateFeeReq)
	require.NoError(t.t, err, "unable to update chan policy")

	expectedPolicy = &lnrpc.RoutingPolicy{
		FeeBaseMsat:      int64(baseFeeMSat),
		FeeRateMilliMsat: testFeeBase * feeRate,
		TimeLockDelta:    timeLockDelta,
		MinHtlc:          1000,
		MaxHtlcMsat:      calculateMaxHtlc(chanAmt),
	}

	// Assert Dave receives Carol's policy update.
	assertChannelPolicyUpdate(
		t.t, dave, carol.PubKeyStr, expectedPolicy, fundingPoint2,
		true,
	)

	// If the channel is public, check that Eve receives Carol's policy
	// update.
	if !private {
		assertChannelPolicyUpdate(
			t.t, eve, carol.PubKeyStr, expectedPolicy,
			fundingPoint2, true,
		)
	}

	// Eve will pay Dave's invoice and should use the updated base fee.
	payReqs = []string{daveInvoiceResp2.PaymentRequest}
	require.NoError(
		t.t, completePaymentRequests(
			eve, eve.RouterClient, payReqs, true,
		), "unable to send payment",
	)

	// Eve will issue an invoice that Dave will pay.
	eveInvoiceResp2, err := eve.AddInvoice(ctxb, invoiceParams)
	require.NoError(t.t, err, "unable to add invoice")

	payReqs = []string{eveInvoiceResp2.PaymentRequest}
	require.NoError(
		t.t, completePaymentRequests(
			dave, dave.RouterClient, payReqs, true,
		), "unable to send payment",
	)

	// If this is a private channel, we'll mine 6 blocks here to test the
	// funding manager logic that deals with ChannelUpdates. If this is not
	// a zero-conf channel, we don't expect a tx in the mempool.
	if private {
		var expectTx int
		if zeroConf {
			expectTx = 1
		}
		_ = mineBlocks(t, net, 6, expectTx)
	}

	// Dave will issue an invoice and Eve will pay it.
	daveInvoiceResp3, err := dave.AddInvoice(ctxb, invoiceParams)
	require.NoError(t.t, err, "unable to add invoice")

	payReqs = []string{daveInvoiceResp3.PaymentRequest}
	require.NoError(
		t.t, completePaymentRequests(
			eve, eve.RouterClient, payReqs, true,
		), "unable to send payment",
	)

	// Carol will disable the channel, assert that Dave sees it and Eve as
	// well if the channel is public.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = carol.RouterClient.UpdateChanStatus(ctxt, disableReq)
	require.NoError(t.t, err)

	carolPolicy := getChannelPolicies(
		t, dave, carol.PubKeyStr, fundingPoint2,
	)[0]
	carolPolicy.Disabled = true
	assertChannelPolicyUpdate(
		t.t, dave, carol.PubKeyStr, carolPolicy, fundingPoint2, true,
	)

	if !private {
		assertChannelPolicyUpdate(
			t.t, eve, carol.PubKeyStr, carolPolicy, fundingPoint2,
			true,
		)
	}

	// Carol will enable the channel, assert the same as above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = carol.RouterClient.UpdateChanStatus(ctxt, enableReq)
	require.NoError(t.t, err)

	carolPolicy.Disabled = false
	assertChannelPolicyUpdate(
		t.t, dave, carol.PubKeyStr, carolPolicy, fundingPoint2, true,
	)

	if !private {
		assertChannelPolicyUpdate(
			t.t, eve, carol.PubKeyStr, carolPolicy, fundingPoint2,
			true,
		)
	}

	// Dave will issue an invoice and Eve should pay it after Carol updates
	// her channel policy.
	daveInvoiceResp4, err := dave.AddInvoice(ctxb, invoiceParams)
	require.NoError(t.t, err, "unable to add invoice")

	feeRate = int64(3)
	updateFeeReq = &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   int64(baseFeeMSat),
		FeeRate:       float64(feeRate),
		TimeLockDelta: timeLockDelta,
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: fundingPoint2,
		},
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	_, err = carol.UpdateChannelPolicy(ctxt, updateFeeReq)
	require.NoError(t.t, err, "unable to update chan policy")

	expectedPolicy = &lnrpc.RoutingPolicy{
		FeeBaseMsat:      int64(baseFeeMSat),
		FeeRateMilliMsat: testFeeBase * feeRate,
		TimeLockDelta:    timeLockDelta,
		MinHtlc:          1000,
		MaxHtlcMsat:      calculateMaxHtlc(chanAmt),
	}

	// Assert Dave and optionally Eve receives Carol's update.
	assertChannelPolicyUpdate(
		t.t, dave, carol.PubKeyStr, expectedPolicy, fundingPoint2,
		true,
	)

	if !private {
		assertChannelPolicyUpdate(
			t.t, eve, carol.PubKeyStr, expectedPolicy,
			fundingPoint2, true,
		)
	}

	payReqs = []string{daveInvoiceResp4.PaymentRequest}
	require.NoError(
		t.t, completePaymentRequests(
			eve, eve.RouterClient, payReqs, true,
		), "unable to send payment",
	)
}

// testOptionScidUpgrade tests that toggling the option-scid-alias feature bit
// correctly upgrades existing channels.
func testOptionScidUpgrade(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// Start carol with anchors only.
	carolArgs := []string{
		"--protocol.anchors",
	}
	carol := net.NewNode(t.t, "carol", carolArgs)

	// Start dave with anchors + scid-alias.
	daveArgs := []string{
		"--protocol.anchors",
		"--protocol.option-scid-alias",
	}
	dave := net.NewNode(t.t, "dave", daveArgs)
	defer shutdownAndAssert(net, t, dave)

	// Give carol some coins.
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, carol)

	// Ensure carol and are connected.
	net.EnsureConnected(t.t, carol, dave)

	chanAmt := btcutil.Amount(1_000_000)

	fundingPoint := openChannelAndAssert(
		t, net, carol, dave,
		lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: chanAmt / 2,
			Private: true,
		},
	)
	defer closeChannelAndAssert(t, net, carol, fundingPoint, false)

	err := carol.WaitForNetworkChannelOpen(fundingPoint)
	require.NoError(t.t, err)
	err = dave.WaitForNetworkChannelOpen(fundingPoint)
	require.NoError(t.t, err)

	// Bob will open a channel to Carol now.
	net.EnsureConnected(t.t, net.Bob, carol)

	fundingPoint2 := openChannelAndAssert(
		t, net, net.Bob, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	err = net.Bob.WaitForNetworkChannelOpen(fundingPoint2)
	require.NoError(t.t, err)
	err = carol.WaitForNetworkChannelOpen(fundingPoint2)
	require.NoError(t.t, err)
	err = dave.WaitForNetworkChannelOpen(fundingPoint2)
	require.NoError(t.t, err)

	// Carol will now set the option-scid-alias feature bit and restart.
	carolArgs = append(carolArgs, "--protocol.option-scid-alias")
	carol.SetExtraArgs(carolArgs)
	err = net.RestartNode(carol, nil)
	require.NoError(t.t, err)

	// Dave will create an invoice for Carol to pay, it should contain an
	// alias in the hop hints.
	daveParams := &lnrpc.Invoice{
		Value:   int64(10_000),
		Private: true,
	}

	var daveInvoice *lnrpc.AddInvoiceResponse

	var startingAlias lnwire.ShortChannelID
	startingAlias.BlockHeight = 16_000_000

	err = wait.Predicate(func() bool {
		ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
		invoiceResp, err := dave.AddInvoice(ctxt, daveParams)
		if err != nil {
			return false
		}

		payReq := &lnrpc.PayReqString{
			PayReq: invoiceResp.PaymentRequest,
		}

		decodedReq, err := dave.DecodePayReq(ctxb, payReq)
		if err != nil {
			return false
		}

		if len(decodedReq.RouteHints) != 1 {
			return false
		}

		if len(decodedReq.RouteHints[0].HopHints) != 1 {
			return false
		}

		hopHint := decodedReq.RouteHints[0].HopHints[0].ChanId
		if startingAlias.ToUint64() == hopHint {
			daveInvoice = invoiceResp
			return true
		}

		return false
	}, defaultTimeout)
	require.NoError(t.t, err)

	// Carol should be able to pay it.
	_ = sendAndAssertSuccess(
		t, carol, &routerrpc.SendPaymentRequest{
			PaymentRequest: daveInvoice.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)

	daveInvoice2, err := dave.AddInvoice(ctxb, daveParams)
	require.NoError(t.t, err)

	_ = sendAndAssertSuccess(
		t, net.Bob, &routerrpc.SendPaymentRequest{
			PaymentRequest: daveInvoice2.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)
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
