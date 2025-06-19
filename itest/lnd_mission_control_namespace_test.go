package itest

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testMissionControlNamespace tests that payments can use different mission
// control namespaces to maintain separate routing histories.
func testMissionControlNamespace(ht *lntest.HarnessTest) {
	const chanAmt = btcutil.Amount(1_000_000)

	// We'll create a simple two node network for this tesat.
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNode("Bob", nil)
	ht.EnsureConnected(alice, bob)
	chanPoint := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	ht.AssertChannelInGraph(alice, chanPoint)
	ht.AssertChannelInGraph(bob, chanPoint)

	// Create an invoice for Bob that we'll pay below.
	const paymentAmt = 10_000
	invoice := bob.RPC.AddInvoice(&lnrpc.Invoice{
		Value: paymentAmt,
		Memo:  "test payment default namespace",
	})

	// Reset mission control for the default namespace to ensure clean
	// state.
	_, err := alice.RPC.Router.ResetMissionControl(
		ht.Context(), &routerrpc.ResetMissionControlRequest{
			MissionControlNamespace: "",
		},
	)
	require.NoError(ht.T, err, "failed to reset default mission control")

	ht.Run("send_payment_with_namespaces", func(t *testing.T) {
		// Send a payment using the default namespace.
		reqDefault := &routerrpc.SendPaymentRequest{
			PaymentRequest:          invoice.PaymentRequest,
			TimeoutSeconds:          60,
			MissionControlNamespace: "",
		}
		ht.SendPaymentAssertSettled(alice, reqDefault)

		// Make another payment with Bob but use our custom namespace
		// this time instead.
		invoice2 := bob.RPC.AddInvoice(&lnrpc.Invoice{
			Value: paymentAmt,
			Memo:  "test payment custom namespace",
		})
		customNs := "custom-namespace-send"
		reqCustom := &routerrpc.SendPaymentRequest{
			PaymentRequest:          invoice2.PaymentRequest,
			TimeoutSeconds:          60,
			MissionControlNamespace: customNs,
		}
		payment := ht.SendPaymentAssertSettled(alice, reqCustom)
		require.Equal(t, lnrpc.Payment_SUCCEEDED, payment.Status,
			"payment with custom namespace should succeed")

		// If we query for the default mc, then we should find that the
		// records are populated.
		mcReqDefault := &routerrpc.QueryMissionControlRequest{
			MissionControlNamespace: "",
		}
		defaultMC, err := alice.RPC.Router.QueryMissionControl(
			ht.Context(), mcReqDefault,
		)
		require.NoError(t, err)
		require.NotEmpty(t, defaultMC.Pairs,
			"default namespace should have recorded channel usage")

		// Similarly, we should find that the custom namespace also has
		// entries.
		mcReqCustom := &routerrpc.QueryMissionControlRequest{
			MissionControlNamespace: customNs,
		}
		customMC, err := alice.RPC.Router.QueryMissionControl(
			ht.Context(), mcReqCustom,
		)
		require.NoError(t, err)
		require.NotEmpty(t, customMC.Pairs,
			"custom namespace should have recorded channel usage")

		require.True(t, len(defaultMC.Pairs) > 0, "Default MC empty")
		require.True(t, len(customMC.Pairs) > 0, "Custom MC empty")
	})

	ht.Run("reset_mission_control_with_namespace", func(t *testing.T) {
		customNsReset := "namespace-to-reset"

		// Send a payment to populate this new custom namespace.
		invReset := bob.RPC.AddInvoice(&lnrpc.Invoice{Value: 100})
		reqPopulate := &routerrpc.SendPaymentRequest{
			PaymentRequest:          invReset.PaymentRequest,
			TimeoutSeconds:          60,
			MissionControlNamespace: customNsReset,
		}
		ht.SendPaymentAssertSettled(alice, reqPopulate)

		// Query the namespace, we should find now that it has some
		// entries populated.
		mcBeforeReset, err := alice.RPC.Router.QueryMissionControl(
			ht.Context(), &routerrpc.QueryMissionControlRequest{
				MissionControlNamespace: customNsReset,
			},
		)
		require.NoError(t, err)
		require.NotEmpty(
			t, mcBeforeReset.Pairs, "MC should be populated "+
				"before reset",
		)

		// Now, we'll reset the ns, then check below that it's actually
		// empty.
		_, errReset := alice.RPC.Router.ResetMissionControl(
			ht.Context(), &routerrpc.ResetMissionControlRequest{
				MissionControlNamespace: customNsReset,
			},
		)
		require.NoError(t, errReset)

		// Query again to confirm it's empty.
		mcAfterReset, err := alice.RPC.Router.QueryMissionControl(
			ht.Context(), &routerrpc.QueryMissionControlRequest{
				MissionControlNamespace: customNsReset,
			},
		)
		require.NoError(t, err)
		require.Empty(
			t, mcAfterReset.Pairs, "MC should be empty after reset",
		)

		// Now we'll test isolation: the default namespace shouldn't
		// have been affected.
		defaultMC, err := alice.RPC.Router.QueryMissionControl(
			ht.Context(), &routerrpc.QueryMissionControlRequest{
				MissionControlNamespace: "",
			},
		)
		require.NoError(t, err)
		require.NotEmpty(
			t, defaultMC.Pairs, "Default MC should not be empty",
		)
	})

	ht.Run("ximport_mission_control_with_namespace", func(t *testing.T) {
		customNs := "namespace-to-import"

		// We'll make some fake data to import as weights.
		importData := &routerrpc.XImportMissionControlRequest{ //nolint:ll
			MissionControlNamespace: customNs,
			Pairs: []*routerrpc.PairHistory{
				{
					NodeFrom: alice.PubKey[:],
					NodeTo:   bob.PubKey[:],
					History: &routerrpc.PairData{
						FailTime:       time.Now().Unix() - 1000,
						FailAmtMsat:    5000,
						SuccessTime:    time.Now().Unix(),
						SuccessAmtMsat: 10000,
					},
				},
			},
			Force: true,
		}

		_, err := alice.RPC.Router.XImportMissionControl(
			ht.Context(), importData,
		)
		require.NoError(t, err)

		// That data that we created above should be found now.
		mcAfterImport, err := alice.RPC.Router.QueryMissionControl(
			ht.Context(), &routerrpc.QueryMissionControlRequest{
				MissionControlNamespace: customNs,
			},
		)
		require.NoError(t, err)
		require.NotEmpty(
			t, mcAfterImport.Pairs, "MC should have imported data",
		)
		require.Equal(
			t, int64(10000),
			mcAfterImport.Pairs[0].History.SuccessAmtMsat,
		)
	})
}
