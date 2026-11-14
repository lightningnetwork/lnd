package itest

import (
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testBlindedPathHTLCMaximum checks the introduction node's outgoing channel
// as the bottleneck: Alice -> Bob (intro) -> Carol -> Dave. Bob's channel
// permits 1,000 sats, including Carol's 1-sat fee, leaving 999 sats for Dave.
func testBlindedPathHTLCMaximum(ht *lntest.HarnessTest) {
	assertBlindedPathHTLCMaximum(ht, 1, 1_000_000)
}

// testBlindedPathHTLCMaximumMiddle checks a bottleneck inside the hidden path:
// Alice -> Bob (intro) -> Carol -> Dave -> Eve. Bob's outgoing channel permits
// 1,000 sats, but Carol's outgoing channel permits only 500 sats. Dave's 1-sat
// downstream fee must fit inside Carol's limit, leaving 499 sats for Eve.
// Bob remains the introduction node even though Carol limits the path.
func testBlindedPathHTLCMaximumMiddle(ht *lntest.HarnessTest) {
	assertBlindedPathHTLCMaximum(ht, 2, 500_000)
}

// assertBlindedPathHTLCMaximum builds a chain with Bob as introduction node.
// bottleneckIndex identifies the limited outgoing channel and its source in
// the chain; one fee-charging forwarder and the recipient follow that source.
// It proves the fee-adjusted boundary is payable, then attempts the advertised
// maximum, which must also fit every outgoing channel in the blinded path.
func assertBlindedPathHTLCMaximum(ht *lntest.HarnessTest,
	bottleneckIndex int, channelMaximumMsat uint64) {

	// Disable policy padding to exercise the exact advertised boundary.
	numNodes := bottleneckIndex + 3
	cfgs := make([][]string, numNodes)
	cfgs[numNodes-1] = []string{
		"--routing.blinding.policy-increase-multiplier=1",
		"--routing.blinding.policy-decrease-multiplier=1",
	}
	channels, nodes := ht.CreateSimpleNetwork(
		cfgs,
		lntest.OpenChannelParams{
			Amt:        100_000,
			UseBaseFee: true,
			UseFeeRate: true,
		},
	)
	alice, bob, recipient := nodes[0], nodes[1], nodes[numNodes-1]

	const downstreamFeeMsat = 1_000

	// All channels have ample liquidity. Configure the limited channel
	// and the fee on the final channel. Earlier channels carry that fee
	// as well as the recipient amount; the introduction channel is not
	// necessarily the bottleneck.
	for i := 1; i < len(channels); i++ {
		maximum := uint64(1_000_000)
		if i == bottleneckIndex {
			maximum = channelMaximumMsat
		}
		var baseFee int64
		if i == len(channels)-1 {
			baseFee = downstreamFeeMsat
		}
		nodes[i].RPC.UpdateChannelPolicy(&lnrpc.PolicyUpdateRequest{
			Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
				ChanPoint: channels[i],
			},
			BaseFeeMsat:   baseFee,
			TimeLockDelta: 40,
			MaxHtlcMsat:   maximum,
		})
		expected := &lnrpc.RoutingPolicy{
			FeeBaseMsat:   baseFee,
			TimeLockDelta: 40,
			MinHtlc:       1_000,
			MaxHtlcMsat:   maximum,
		}
		for _, observer := range nodes {
			ht.AssertChannelPolicyUpdate(
				observer, nodes[i], expected, channels[i],
				false,
			)
		}
	}

	// First prove that the real boundary is payable. Then try the
	// amount advertised by the invoice. SendToRouteV2 makes one
	// attempt, preventing automatic retries or MPP from masking a bad
	// per-HTLC maximum.
	amount := int64(channelMaximumMsat - downstreamFeeMsat)
	for range 2 {
		invoice := recipient.RPC.AddInvoice(&lnrpc.Invoice{
			ValueMsat: amount,
			IsBlinded: true,
			BlindedPathConfig: &lnrpc.BlindedPathConfig{
				MinNumRealHops: new(uint32(numNodes - 2)),
				NumHops:        new(uint32(numNodes - 2)),
			},
		})
		decoded := alice.RPC.DecodePayReq(invoice.PaymentRequest)
		require.Len(ht, decoded.BlindedPaths, 1)
		path := decoded.BlindedPaths[0]
		require.Equal(
			ht, bob.PubKey[:], path.BlindedPath.IntroductionNode,
		)
		require.Len(ht, path.BlindedPath.BlindedHops, numNodes-1)
		require.EqualValues(ht, downstreamFeeMsat, path.BaseFeeMsat)
		require.Zero(ht, path.ProportionalFeeRate)
		require.LessOrEqual(ht, uint64(amount), path.HtlcMaxMsat)
		require.LessOrEqual(
			ht, path.HtlcMaxMsat, channelMaximumMsat,
		)

		routes := alice.RPC.QueryRoutes(&lnrpc.QueryRoutesRequest{
			AmtMsat:             amount,
			BlindedPaymentPaths: decoded.BlindedPaths,
			FeeLimit: &lnrpc.FeeLimit{
				Limit: &lnrpc.FeeLimit_FixedMsat{
					FixedMsat: 10_000,
				},
			},
		})
		require.Len(ht, routes.Routes, 1)
		attempt := alice.RPC.SendToRouteV2(
			&routerrpc.SendToRouteRequest{
				PaymentHash: invoice.RHash,
				Route:       routes.Routes[0],
			},
		)
		ht.Logf("recipient amount=%d, advertised maximum=%d, "+
			"status=%v, failure=%v", amount, path.HtlcMaxMsat,
			attempt.Status, attempt.Failure)
		require.Equal(
			ht, lnrpc.HTLCAttempt_SUCCEEDED, attempt.Status,
			"an amount admitted by the blinded path must be "+
				"payable: %v", attempt.Failure,
		)
		settled := recipient.RPC.LookupInvoice(invoice.RHash)
		require.Equal(ht, lnrpc.Invoice_SETTLED, settled.State)
		require.Equal(ht, amount, settled.AmtPaidMsat)

		amount = int64(path.HtlcMaxMsat)
	}
}
