package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/stretchr/testify/require"
)

func testMultiHopPayments(ht *lntest.HarnessTest) {
	const chanAmt = btcutil.Amount(100000)

	// As preliminary setup, we'll create two new nodes: Carol and Dave,
	// such that we now have a 4 node, 3 channel topology. Dave will make a
	// channel with Alice, and Carol with Dave. After this setup, the
	// network topology should now look like:
	//     Carol -> Dave -> Alice -> Bob
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNode("Bob", nil)

	daveArgs := []string{"--protocol.legacy.onion"}
	dave := ht.NewNode("Dave", daveArgs)
	carol := ht.NewNode("Carol", nil)

	// Subscribe events early so we don't miss it out.
	aliceEvents := alice.RPC.SubscribeHtlcEvents()
	bobEvents := bob.RPC.SubscribeHtlcEvents()
	carolEvents := carol.RPC.SubscribeHtlcEvents()
	daveEvents := dave.RPC.SubscribeHtlcEvents()

	// Once subscribed, the first event will be UNKNOWN.
	ht.AssertHtlcEventType(aliceEvents, routerrpc.HtlcEvent_UNKNOWN)
	ht.AssertHtlcEventType(bobEvents, routerrpc.HtlcEvent_UNKNOWN)
	ht.AssertHtlcEventType(carolEvents, routerrpc.HtlcEvent_UNKNOWN)
	ht.AssertHtlcEventType(daveEvents, routerrpc.HtlcEvent_UNKNOWN)

	// Connect the nodes.
	ht.ConnectNodes(alice, bob)
	ht.ConnectNodes(dave, alice)
	ht.ConnectNodes(carol, dave)

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	chanPointAlice := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// We'll create Dave and establish a channel to Alice. Dave will be
	// running an older node that requires the legacy onion payload.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, dave)
	chanPointDave := ht.OpenChannel(
		dave, alice, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Next, we'll create Carol and establish a channel to from her to
	// Dave.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)
	chanPointCarol := ht.OpenChannel(
		carol, dave, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Create 5 invoices for Bob, which expect a payment from Carol for 1k
	// satoshis with a different preimage each time.
	const numPayments = 5
	const paymentAmt = 1000
	payReqs, _, _ := ht.CreatePayReqs(bob, paymentAmt, numPayments)

	// Set the fee policies of the Alice -> Bob and the Dave -> Alice
	// channel edges to relatively large non default values. This makes it
	// possible to pick up more subtle fee calculation errors.
	maxHtlc := lntest.CalculateMaxHtlc(chanAmt)
	const aliceBaseFeeSat = 20
	const aliceFeeRatePPM = 100000
	updateChannelPolicy(
		ht, alice, chanPointAlice, aliceBaseFeeSat*1000,
		aliceFeeRatePPM, 0, 0, false,
		chainreg.DefaultBitcoinTimeLockDelta, maxHtlc, carol,
	)

	// Define a negative inbound fee for Alice, to verify that this is
	// backwards compatible with an older sender ignoring the discount.
	const (
		aliceInboundBaseFeeMsat = -2000
		aliceInboundFeeRate     = -50000 // 5%
	)

	// We update the channel twice. The first time we set the inbound fee,
	// the second time we don't. This is done to test whether the switch is
	// still aware of the inbound fees.
	updateChannelPolicy(
		ht, alice, chanPointDave, 0, 0,
		aliceInboundBaseFeeMsat, aliceInboundFeeRate, true,
		chainreg.DefaultBitcoinTimeLockDelta, maxHtlc,
		dave,
	)

	updateChannelPolicy(
		ht, alice, chanPointDave, 0, 0,
		aliceInboundBaseFeeMsat, aliceInboundFeeRate, false,
		chainreg.DefaultBitcoinTimeLockDelta, maxHtlc,
		dave,
	)

	const daveBaseFeeSat = 5
	const daveFeeRatePPM = 150000
	updateChannelPolicy(
		ht, dave, chanPointDave, daveBaseFeeSat*1000, daveFeeRatePPM,
		0, 0, true,
		chainreg.DefaultBitcoinTimeLockDelta, maxHtlc, carol,
	)

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ht.CompletePaymentRequests(carol, payReqs)

	// At this point all the channels within our proto network should be
	// shifted by 5k satoshis in the direction of Bob, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Carol->David->Alice->Bob, order is Bob,
	// Alice, David, Carol.

	// The final node bob expects to get paid five times 1000 sat.
	expectedAmountPaidAtoB := int64(numPayments * paymentAmt)

	ht.AssertAmountPaid("Alice(local) => Bob(remote)", bob,
		chanPointAlice, int64(0), expectedAmountPaidAtoB)
	ht.AssertAmountPaid("Alice(local) => Bob(remote)", alice,
		chanPointAlice, expectedAmountPaidAtoB, int64(0))

	// To forward a payment of 1000 sat, Alice is charging a fee of 20 sat +
	// 10% = 120 sat, plus the inbound fee over 1120 (= 1000 + 120) sat of
	// -2 sat - 5% = -58 sat. This makes a total of 62 sat per payment. For
	// 5 payments, it works out to 310 sat.
	const expectedFeeAlice = 310

	// Dave needs to pay what Alice pays plus Alice's fee.
	expectedAmountPaidDtoA := expectedAmountPaidAtoB + expectedFeeAlice

	ht.AssertAmountPaid("Dave(local) => Alice(remote)", alice,
		chanPointDave, int64(0), expectedAmountPaidDtoA)
	ht.AssertAmountPaid("Dave(local) => Alice(remote)", dave,
		chanPointDave, expectedAmountPaidDtoA, int64(0))

	// To forward a payment of 1062 sat, Dave is charging a fee of 5 sat +
	// 15% = 164.3 sat. For 5 payments this is 821.5 sat. This test works
	// with sats, so we need to round down to 821.
	const expectedFeeDave = 821

	// Carol needs to pay what Dave pays plus Dave's fee.
	expectedAmountPaidCtoD := expectedAmountPaidDtoA + expectedFeeDave

	ht.AssertAmountPaid("Carol(local) => Dave(remote)", dave,
		chanPointCarol, int64(0), expectedAmountPaidCtoD)
	ht.AssertAmountPaid("Carol(local) => Dave(remote)", carol,
		chanPointCarol, expectedAmountPaidCtoD, int64(0))

	// Now that we know all the balances have been settled out properly,
	// we'll ensure that our internal record keeping for completed circuits
	// was properly updated.

	// First, check that the FeeReport response shows the proper fees
	// accrued over each time range. Dave should've earned 170 satoshi for
	// each of the forwarded payments.
	ht.AssertFeeReport(
		dave, expectedFeeDave, expectedFeeDave, expectedFeeDave,
	)

	// Next, ensure that if we issue the vanilla query for the forwarding
	// history, it returns 5 values, and each entry is formatted properly.
	// From David's perspective he receives a payement from Carol and
	// forwards it to Alice. So let's ensure that the forwarding history
	// returns Carol's peer alias as inbound and Alice's alias as outbound.
	info := carol.RPC.GetInfo()
	carolAlias := info.Alias

	info = alice.RPC.GetInfo()
	aliceAlias := info.Alias

	fwdingHistory := dave.RPC.ForwardingHistory(nil)
	require.Len(ht, fwdingHistory.ForwardingEvents, numPayments)

	expectedForwardingFee := uint64(expectedFeeDave / numPayments)
	for _, event := range fwdingHistory.ForwardingEvents {
		// Each event should show a fee of 170 satoshi.
		require.Equal(ht, expectedForwardingFee, event.Fee)

		// Check that peer aliases are empty since the
		// ForwardingHistoryRequest did not specify the PeerAliasLookup
		// flag.
		require.Empty(ht, event.PeerAliasIn)
		require.Empty(ht, event.PeerAliasOut)
	}

	// Lookup the forwarding history again but this time also lookup the
	// peers' alias names.
	fwdingHistory = dave.RPC.ForwardingHistory(
		&lnrpc.ForwardingHistoryRequest{
			PeerAliasLookup: true,
		},
	)
	require.Len(ht, fwdingHistory.ForwardingEvents, numPayments)
	for _, event := range fwdingHistory.ForwardingEvents {
		// Each event should show a fee of 170 satoshi.
		require.Equal(ht, expectedForwardingFee, event.Fee)

		// Check that peer aliases adhere to payment flow, namely
		// Carol->Dave->Alice.
		require.Equal(ht, carolAlias, event.PeerAliasIn)
		require.Equal(ht, aliceAlias, event.PeerAliasOut)
	}

	// Verify HTLC IDs are not nil and unique across all forwarding events.
	seenIDs := make(map[uint64]bool)
	for _, event := range fwdingHistory.ForwardingEvents {
		// We check that the incoming and outgoing htlc indices are not
		// set to nil. The indices are required for any forwarding event
		// recorded after v0.20.
		require.NotNil(ht, event.IncomingHtlcId)
		require.NotNil(ht, event.OutgoingHtlcId)

		require.False(ht, seenIDs[*event.IncomingHtlcId])
		require.False(ht, seenIDs[*event.OutgoingHtlcId])
		seenIDs[*event.IncomingHtlcId] = true
		seenIDs[*event.OutgoingHtlcId] = true
	}

	// The HTLC IDs should be exactly 0, 1, 2, 3, 4.
	expectedIDs := map[uint64]bool{
		0: true,
		1: true,
		2: true,
		3: true,
		4: true,
	}
	require.Equal(ht, expectedIDs, seenIDs)

	// We expect Carol to have successful forwards and settles for
	// her sends.
	ht.AssertHtlcEvents(
		carolEvents, numPayments, 0, numPayments, 0,
		routerrpc.HtlcEvent_SEND,
	)

	// Dave and Alice should both have forwards and settles for
	// their role as forwarding nodes.
	ht.AssertHtlcEvents(
		daveEvents, numPayments, 0, numPayments*2, 0,
		routerrpc.HtlcEvent_FORWARD,
	)
	ht.AssertHtlcEvents(
		aliceEvents, numPayments, 0, numPayments*2, 0,
		routerrpc.HtlcEvent_FORWARD,
	)

	// Bob should only have settle events for his receives.
	ht.AssertHtlcEvents(
		bobEvents, 0, 0, numPayments, 0, routerrpc.HtlcEvent_RECEIVE,
	)
}

// updateChannelPolicy updates the channel policy of node to the given fees and
// timelock delta. This function blocks until listenerNode has received the
// policy update.
//
// NOTE: only used in current test.
func updateChannelPolicy(ht *lntest.HarnessTest, hn *node.HarnessNode,
	chanPoint *lnrpc.ChannelPoint, baseFee int64, feeRate int64,
	inboundBaseFee, inboundFeeRate int32, updateInboundFee bool,
	timeLockDelta uint32, maxHtlc uint64, listenerNode *node.HarnessNode) {

	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMsat:             baseFee,
		FeeRateMilliMsat:        feeRate,
		TimeLockDelta:           timeLockDelta,
		MinHtlc:                 1000, // default value
		MaxHtlcMsat:             maxHtlc,
		InboundFeeBaseMsat:      inboundBaseFee,
		InboundFeeRateMilliMsat: inboundFeeRate,
	}

	var inboundFee *lnrpc.InboundFee
	if updateInboundFee {
		inboundFee = &lnrpc.InboundFee{
			BaseFeeMsat: inboundBaseFee,
			FeeRatePpm:  inboundFeeRate,
		}
	}

	updateFeeReq := &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   baseFee,
		FeeRate:       float64(feeRate) / testFeeBase,
		TimeLockDelta: timeLockDelta,
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: chanPoint,
		},
		MaxHtlcMsat: maxHtlc,
		InboundFee:  inboundFee,
	}

	hn.RPC.UpdateChannelPolicy(updateFeeReq)

	// Wait for listener node to receive the channel update from node.
	ht.AssertChannelPolicyUpdate(
		listenerNode, hn, expectedPolicy, chanPoint, false,
	)
}
