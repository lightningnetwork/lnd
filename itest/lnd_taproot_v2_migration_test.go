package itest

import (
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testPrivateTaprootV2Migration tests that a private taproot channel survives
// the V1→V2 SQL data migration. The test:
//
//  1. Starts Alice with --dev.skip-taproot-v2-migration so the migration
//     does not run on first startup.
//  2. Opens a private taproot channel between Alice and Bob.
//  3. Sends a payment from Alice to Bob to verify the channel works.
//  4. Restarts Alice WITHOUT the skip flag so the migration runs.
//  5. Verifies the channel is still active and payments still work.
//  6. Bob updates his channel policy and Alice correctly receives and
//     persists the V1 update against the now-V2-stored channel.
func testPrivateTaprootV2Migration(ht *lntest.HarnessTest) {
	// Args for taproot channel support.
	taprootArgs := lntest.NodeArgsForCommitType(
		lnrpc.CommitmentType_SIMPLE_TAPROOT,
	)

	// Start Alice with the skip flag so the migration doesn't run yet.
	skipMigArgs := append(
		taprootArgs, "--dev.skip-taproot-v2-migration",
	)
	alice := ht.NewNodeWithCoins("Alice", skipMigArgs)
	bob := ht.NewNodeWithCoins("Bob", taprootArgs)

	ht.EnsureConnected(alice, bob)

	// Open a private taproot channel.
	const chanAmt = 1_000_000
	params := lntest.OpenChannelParams{
		Amt:            chanAmt,
		CommitmentType: lnrpc.CommitmentType_SIMPLE_TAPROOT,
		Private:        true,
	}
	pendingChan := ht.OpenChannelAssertPending(alice, bob, params)
	chanPoint := lntest.ChanPointFromPendingUpdate(pendingChan)

	// Mine blocks to confirm the channel.
	ht.MineBlocksAndAssertNumTxes(6, 1)
	ht.AssertChannelActive(alice, chanPoint)
	ht.AssertChannelActive(bob, chanPoint)

	// Send a payment pre-migration to verify the channel works.
	const paymentAmt = 10_000
	invoice := bob.RPC.AddInvoice(&lnrpc.Invoice{
		Value: paymentAmt,
	})
	ht.CompletePaymentRequests(alice, []string{invoice.PaymentRequest})

	// Restart Alice WITHOUT the skip flag. This allows the taproot V2
	// migration to run, converting the private taproot channel from V1
	// workaround storage to canonical V2.
	alice.SetExtraArgs(taprootArgs)
	ht.RestartNode(alice)

	// Ensure reconnection.
	ht.EnsureConnected(alice, bob)

	// Verify the channel is still active after migration.
	ht.AssertChannelActive(alice, chanPoint)

	// Send another payment post-migration to verify the channel still
	// works for pathfinding and forwarding.
	invoice2 := bob.RPC.AddInvoice(&lnrpc.Invoice{
		Value: paymentAmt,
	})
	ht.CompletePaymentRequests(alice, []string{invoice2.PaymentRequest})

	// Also send a payment in the other direction to verify both policy
	// directions survived the migration.
	invoice3 := alice.RPC.AddInvoice(&lnrpc.Invoice{
		Value: paymentAmt,
	})
	ht.CompletePaymentRequests(bob, []string{invoice3.PaymentRequest})

	// Now test that Bob can update his channel policy and Alice
	// correctly receives the V1 ChannelUpdate and persists it against
	// the now-V2-stored channel. This exercises the UpdateEdgePolicy
	// shim that converts incoming V1 policy updates to V2 for migrated
	// private taproot channels.
	const (
		newBaseFee      = 5000
		newFeeRate      = 500
		newTimeLockDelta = 80
	)

	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMsat:      newBaseFee,
		FeeRateMilliMsat: newFeeRate,
		TimeLockDelta:    newTimeLockDelta,
		MinHtlc:          1000,
		MaxHtlcMsat:      990_000_000,
	}

	req := &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   newBaseFee,
		FeeRate:       float64(newFeeRate) / 1_000_000,
		TimeLockDelta: newTimeLockDelta,
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: chanPoint,
		},
	}
	updateResp := bob.RPC.UpdateChannelPolicy(req)
	require.Empty(ht, updateResp.FailedUpdates)

	// Wait for Alice to receive Bob's policy update. Use
	// includeUnannounced=true since this is a private channel.
	ht.AssertChannelPolicyUpdate(
		alice, bob, expectedPolicy, chanPoint, true,
	)

	// Verify Alice can still route a payment to Bob using the
	// updated policy. This confirms the policy was correctly
	// persisted as V2 in Alice's graph.
	invoice4 := bob.RPC.AddInvoice(&lnrpc.Invoice{
		Value: paymentAmt,
	})
	ht.CompletePaymentRequests(alice, []string{invoice4.PaymentRequest})

	// Now test the reverse: Alice (migrated, channel stored as V2)
	// updates her own policy. This exercises the path where Alice's
	// gossiper reads the V2 channel via the shim (projected as V1),
	// builds and signs a V1 ChannelUpdate, persists it as V2 via
	// the UpdateEdgePolicy shim, and sends the V1 update to Bob
	// (the legacy peer). Bob must correctly receive and apply it.
	const (
		aliceBaseFee       = 3000
		aliceFeeRate       = 300
		aliceTimeLockDelta = 60
	)

	aliceExpectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMsat:      aliceBaseFee,
		FeeRateMilliMsat: aliceFeeRate,
		TimeLockDelta:    aliceTimeLockDelta,
		MinHtlc:          1000,
		MaxHtlcMsat:      990_000_000,
	}

	aliceReq := &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   aliceBaseFee,
		FeeRate:       float64(aliceFeeRate) / 1_000_000,
		TimeLockDelta: aliceTimeLockDelta,
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: chanPoint,
		},
	}
	aliceUpdateResp := alice.RPC.UpdateChannelPolicy(aliceReq)
	require.Empty(ht, aliceUpdateResp.FailedUpdates)

	// Wait for Bob (legacy peer, no migration) to receive Alice's
	// V1 policy update.
	ht.AssertChannelPolicyUpdate(
		bob, alice, aliceExpectedPolicy, chanPoint, true,
	)

	// Verify Bob can route a payment to Alice using the updated
	// policy. This confirms Alice's V1 ChannelUpdate was correctly
	// constructed from V2 storage and received by the legacy peer.
	invoice5 := alice.RPC.AddInvoice(&lnrpc.Invoice{
		Value: paymentAmt,
	})
	ht.CompletePaymentRequests(bob, []string{invoice5.PaymentRequest})

	// Clean up.
	ht.CloseChannel(alice, chanPoint)
}
