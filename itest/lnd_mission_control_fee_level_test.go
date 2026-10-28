package itest

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// testMissionControlFeeLevel tests that fee levels reduce the number of
// payment attempts. Dave has three cheap channels without inbound liquidity
// and one expensive channel via Carol that works:
//
//	       +- Bob1 -(depleted, cheap)-+
//	Alice -+- Bob2 -(depleted, cheap)-+- Dave
//	       +- Bob3 -(depleted, cheap)-+
//	       +- Carol -(liquid, 1000 PPM)-+
//
// Without fee levels, Alice tries each cheap channel before Carol. With fee
// levels, the first failure raises the fee level of Dave, so the next attempt
// skips the other cheap channels.
func testMissionControlFeeLevel(ht *lntest.HarnessTest) {
	const (
		chanAmt    = btcutil.Amount(1_000_000)
		paymentAmt = btcutil.Amount(100_000)
	)

	alice := ht.NewNodeWithCoins("Alice", nil)
	carol := ht.NewNodeWithCoins(
		"Carol", []string{"--bitcoin.feerate=1000"},
	)
	dave := ht.NewNodeWithCoins("Dave", nil)

	// Dave funds the channels to the Bob nodes, so the Bob nodes have no
	// balance towards Dave.
	params := lntest.OpenChannelParams{Amt: chanAmt}
	var bobs []*node.HarnessNode
	for _, name := range []string{"Bob1", "Bob2", "Bob3"} {
		bob := ht.NewNode(name, nil)
		ht.ConnectNodes(alice, bob)
		ht.ConnectNodes(dave, bob)
		ht.OpenChannel(alice, bob, params)
		ht.OpenChannel(dave, bob, params)
		bobs = append(bobs, bob)
	}
	ht.ConnectNodes(alice, carol)
	ht.ConnectNodes(carol, dave)
	ht.OpenChannel(alice, carol, params)
	ht.OpenChannel(carol, dave, params)

	// Wait until Alice knows all eight channels and their policies.
	ht.AssertNumEdges(alice, 8, false)
	for _, bob := range bobs {
		ht.AssertNumEdges(bob, 8, false)
	}

	pay := func() *lnrpc.Payment {
		invoice := dave.RPC.AddInvoice(&lnrpc.Invoice{
			Value: int64(paymentAmt),
		})

		return ht.SendPaymentAssertSettled(
			alice, &routerrpc.SendPaymentRequest{
				PaymentRequest: invoice.PaymentRequest,
				TimeoutSeconds: 60,
				FeeLimitSat:    1_000,
				MaxParts:       1,
			},
		)
	}

	// The fee level is off by default, so Alice tries all three Bob
	// nodes before Carol.
	payment := pay()
	require.Len(ht, payment.Htlcs, 4)

	// The default config is asymmetric, so a routing node cannot raise
	// the fee levels of the nodes before it. It caps fee levels at
	// 1000 PPM.
	cfg := alice.RPC.GetMissionControlConfig().Config
	require.False(ht, cfg.FeeLevelParameters.Symmetric)
	require.EqualValues(ht, 1_000, cfg.FeeLevelParameters.MaxLevelPpm)

	// Turn the fee level on and forget the failures of the first payment.
	cfg.FeeLevelParameters = &routerrpc.FeeLevelParameters{
		LevelPpm:     2_000,
		Reach:        1,
		DecaySeconds: 3600,
		Symmetric:    true,
		MaxLevelPpm:  2_000,
	}
	alice.RPC.SetMissionControlConfig(cfg)
	alice.RPC.ResetMissionControl()

	// The first failure raises the fee level of Dave above the fee of the
	// other Bob nodes, so the second attempt goes via Carol.
	payment = pay()
	require.Len(ht, payment.Htlcs, 2)

	// Dave has the full fee level, which has not decayed much yet.
	err := wait.NoError(func() error {
		resp, err := alice.RPC.Router.QueryMissionControl(
			ht.Context(), &routerrpc.QueryMissionControlRequest{},
		)
		if err != nil {
			return err
		}

		for _, level := range resp.FeeLevels {
			if string(level.Node) != string(dave.PubKey[:]) {
				continue
			}

			require.InDelta(ht, 2_000, level.FeeLevel, 10)

			return nil
		}

		return fmt.Errorf("no fee level for Dave")
	}, wait.DefaultTimeout)
	require.NoError(ht, err)
}
