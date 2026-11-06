package itest

import (
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/stretchr/testify/require"
)

// testBlindedPathHTLCMaximum checks that an invoice's advertised blinded path
// maximum can actually be delivered through its channels, including the fees
// owed to downstream forwarding nodes.
func testBlindedPathHTLCMaximum(ht *lntest.HarnessTest) {
	// Alice -> Bob (introduction) -> Carol -> Dave (recipient).
	// Disable policy padding to exercise the exact advertised boundary.
	cfgs := [][]string{nil, nil, nil, {
		"--routing.blinding.policy-increase-multiplier=1",
		"--routing.blinding.policy-decrease-multiplier=1",
	}}
	channels, nodes := ht.CreateSimpleNetwork(
		cfgs,
		lntest.OpenChannelParams{
			Amt:        100_000,
			UseBaseFee: true,
			UseFeeRate: true,
		},
	)
	alice, bob, carol, dave := nodes[0], nodes[1], nodes[2], nodes[3]

	const (
		channelMaximumMsat = 1_000_000
		downstreamFeeMsat  = 1_000
		recipientMaximum   = channelMaximumMsat - downstreamFeeMsat
	)

	// Bob's outgoing channel can carry at most 1,000,000 msat. Carol
	// charges 1,000 msat on the next channel, so only 999,000 msat can
	// reach Dave in a single HTLC. All channels have ample liquidity.
	updates := []struct {
		node    *node.HarnessNode
		channel *lnrpc.ChannelPoint
		baseFee int64
		maximum uint64
	}{
		{
			node:    bob,
			channel: channels[1],
			maximum: channelMaximumMsat,
		},
		{
			node:    carol,
			channel: channels[2],
			baseFee: downstreamFeeMsat,
			maximum: lntest.CalculateMaxHtlc(100_000),
		},
	}
	for _, update := range updates {
		update.node.RPC.UpdateChannelPolicy(&lnrpc.PolicyUpdateRequest{
			Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
				ChanPoint: update.channel,
			},
			BaseFeeMsat:   update.baseFee,
			TimeLockDelta: 40,
			MaxHtlcMsat:   update.maximum,
		})
		expected := &lnrpc.RoutingPolicy{
			FeeBaseMsat:   update.baseFee,
			TimeLockDelta: 40,
			MinHtlc:       1_000,
			MaxHtlcMsat:   update.maximum,
		}
		for _, observer := range nodes {
			ht.AssertChannelPolicyUpdate(
				observer, update.node, expected,
				update.channel, false,
			)
		}
	}

	// First prove that the real boundary is payable. Then try the
	// amount advertised by the invoice. SendToRouteV2 makes one
	// attempt, preventing automatic retries or MPP from masking a bad
	// per-HTLC maximum.
	amount := int64(recipientMaximum)
	for range 2 {
		invoice := dave.RPC.AddInvoice(&lnrpc.Invoice{
			ValueMsat: amount,
			IsBlinded: true,
			BlindedPathConfig: &lnrpc.BlindedPathConfig{
				MinNumRealHops: new(uint32(2)),
				NumHops:        new(uint32(2)),
			},
		})
		decoded := alice.RPC.DecodePayReq(invoice.PaymentRequest)
		require.Len(ht, decoded.BlindedPaths, 1)
		path := decoded.BlindedPaths[0]
		require.Equal(
			ht, bob.PubKey[:], path.BlindedPath.IntroductionNode,
		)
		require.Len(ht, path.BlindedPath.BlindedHops, 3)
		require.EqualValues(ht, downstreamFeeMsat, path.BaseFeeMsat)
		require.Zero(ht, path.ProportionalFeeRate)
		require.LessOrEqual(ht, uint64(amount), path.HtlcMaxMsat)
		require.LessOrEqual(
			ht, path.HtlcMaxMsat, uint64(channelMaximumMsat),
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
		settled := dave.RPC.LookupInvoice(invoice.RHash)
		require.Equal(ht, lnrpc.Invoice_SETTLED, settled.State)
		require.Equal(ht, amount, settled.AmtPaidMsat)

		amount = int64(path.HtlcMaxMsat)
	}
}
