package itest

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// testRejectAbsurdCommitFeerate covers the two BOLT-02 checks that bound the
// commitment feerate a channel initiator can impose on the fundee: a
// feerate_per_kw the fundee considers unreasonably large, and an initial
// commitment whose outputs both sit at or below the reserve the initiator
// named. Each check is exercised in both directions, so a fundee that simply
// refused everything would fail the positive case.
//
// Both rejections are driven by what the initiator proposes, so each node is
// started with its own --fee.url pointing at a fee service this test controls.
// That is what lets the test dial feerate_per_kw through the ordinary funding
// flow rather than hand-crafting wire messages. The harness-wide fee service
// cannot be used, since both nodes would share it and the initiator and the
// fundee need to disagree about the current feerate.
func testRejectAbsurdCommitFeerate(ht *lntest.HarnessTest) {
	// The low feerate the fundee considers sane. The fundee's bound is
	// floored at 25000 sat/kw, so pointing it here makes that floor the
	// bound, and anything above 25000 sat/kw is unreasonably large to it.
	const lowFeeRate = chainfee.SatPerKWeight(2500)

	// A feerate no realistic mempool needs a commitment to beat.
	const absurdFeeRate = chainfee.SatPerKWeight(100000)

	// Start a fee service per node. --fee.url is appended after the
	// harness's own value, and the last occurrence of a scalar option
	// wins, so each node ends up with its own estimator.
	aliceFees := lntest.NewFeeService(ht.T)
	require.NoError(ht, aliceFees.Start())
	ht.Cleanup(func() {
		require.NoError(ht, aliceFees.Stop())
	})

	bobFees := lntest.NewFeeService(ht.T)
	require.NoError(ht, bobFees.Start())
	ht.Cleanup(func() {
		require.NoError(ht, bobFees.Stop())
	})

	aliceFees.SetFeeRate(lowFeeRate, 1)
	bobFees.SetFeeRate(lowFeeRate, 1)

	// Bob is the fundee and enforces both checks. Alice is the initiator,
	// and is given a high anchor fee cap so that the rate she may propose
	// is not what limits the absurd case below (the default cap is 10
	// sat/vbyte). The cap has no effect on a tweakless channel, which is
	// what the reserve case opens.
	alice := ht.NewNodeWithCoins("Alice", []string{
		"--fee.url=" + aliceFees.URL(),
		"--max-commit-fee-rate-anchors=400",
	})
	bob := ht.NewNode("Bob", []string{
		"--fee.url=" + bobFees.URL(),
	})

	ht.EnsureConnected(alice, bob)

	// estimateFee asks a node to estimate a fee through the same estimator
	// the funding manager consults, so this reads back the rate that node
	// would actually put in open_channel. The walletkit call is used
	// rather than the lnrpc one because it takes no address and so needs
	// no spendable funds, which Bob does not have.
	estimateFee := func(n *node.HarnessNode) chainfee.SatPerKWeight {
		resp, err := n.RPC.WalletKit.EstimateFee(
			ht.Context(),
			&walletrpc.EstimateFeeRequest{ConfTarget: 3},
		)
		require.NoError(ht, err, "%s EstimateFee", n.Name())

		return chainfee.SatPerKWeight(resp.SatPerKw)
	}

	// feerateSubTest runs one feerate case. The fundee always sits at
	// lowFeeRate, so its bound is the 25000 sat/kw floor; the initiator is
	// pointed at feeRate and the outcome is asserted against expectReject.
	feerateSubTest := func(feeRate chainfee.SatPerKWeight,
		expectReject bool) {

		aliceFees.SetFeeRate(feeRate, 1)
		bobFees.SetFeeRate(lowFeeRate, 1)

		proposed := estimateFee(alice)
		bound := estimateFee(bob) * 10
		ht.Logf("feerate case: initiator proposes %v, fundee bound %v",
			proposed, bound)

		params := lntest.OpenChannelParams{
			Amt:     1_000_000,
			Private: true,
		}

		if !expectReject {
			ht.OpenChannel(alice, bob, params)
			ht.AssertNumPendingOpenChannels(alice, 0)
			ht.AssertNumPendingOpenChannels(bob, 0)

			return
		}

		// The case only proves the feerate check if the initiator's
		// proposal really is above the fundee's bound.
		require.Greater(ht, int64(proposed), int64(bound),
			"the initiator's feerate must exceed the fundee's "+
				"bound for this case to mean anything")

		ht.OpenChannelAssertErr(
			alice, bob, params, fmt.Errorf("unreasonably large"),
		)
	}

	// A rate the fundee treats as unreasonably large must fail the
	// channel, and the same flow at a rate it accepts must succeed.
	feerateSubTest(absurdFeeRate, true)
	feerateSubTest(lowFeeRate, false)

	// Now the reserve check. It fires only when the initiator's balance and
	// the fundee's both sit at or below the reserve the initiator named,
	// which takes a commitment fee large enough to eat most of the
	// channel.
	//
	// The reserve the fundee will accept is capped at a fifth of the
	// channel capacity, so the initiator's balance has to fall below that
	// cap while staying above the two-times-dust guard in
	// NewChannelReservation -- otherwise an earlier check would be the one
	// refusing the open. The fundee is pushed nothing, so it sits at zero
	// and is trivially at or below the same reserve.
	//
	// The channel is opened tweakless, whose commitment weight has no
	// anchor outputs, so the whole commitment fee comes off the amount
	// left over after it.
	const (
		reserveChanAmt = btcutil.Amount(100_000)
		commitWeight   = input.CommitWeight
	)

	// Just under the fifth-of-capacity cap the fundee enforces.
	initiatorReserve := uint64(reserveChanAmt/5) - 1

	// The smallest commitment fee that drops the initiator's balance to or
	// below the reserve it named, and the feerate that produces it.
	minFee := uint64(reserveChanAmt) - initiatorReserve
	minFeeRate := (minFee*1000 + commitWeight - 1) / commitWeight
	reserveCaseFeeRate := chainfee.SatPerKWeight(minFeeRate)

	// The fundee's own rate must leave its feerate bound clear of the
	// rate the initiator proposes, or the feerate check would reject the
	// channel first and this case would prove nothing about reserves.
	const reserveCaseBobRate = chainfee.SatPerKWeight(20000)

	require.Less(ht, int64(minFeeRate), int64(reserveCaseBobRate)*10,
		"the reserve case needs the feerate check not to fire first")

	fee := reserveCaseFeeRate.FeeForWeight(commitWeight)
	initiatorBalance := reserveChanAmt - fee

	ht.Logf("reserve case: capacity=%v, fee=%v, initiator balance=%v, "+
		"reserve=%v", int64(reserveChanAmt), int64(fee),
		int64(initiatorBalance), initiatorReserve)

	// Guard the premises the rejection depends on, so a change to the
	// commitment format or the dust limit fails loudly here rather than
	// making the assertion below pass or fail for the wrong reason.
	require.LessOrEqual(ht, int64(initiatorBalance), int64(initiatorReserve),
		"the initiator's balance must be at or below the reserve it "+
			"named")
	require.Greater(ht, int64(initiatorBalance),
		2*int64(lnwallet.DustLimitUnknownWitness()),
		"the initiator's balance must clear the funder-balance-dust "+
			"guard, or that check would reject the open instead")

	aliceFees.SetFeeRate(reserveCaseFeeRate, 1)
	bobFees.SetFeeRate(reserveCaseBobRate, 1)

	proposed := estimateFee(alice)
	feerateBound := estimateFee(bob) * 10
	ht.Logf("reserve case: initiator proposes %v, fundee bound %v",
		proposed, feerateBound)
	require.LessOrEqual(ht, int64(proposed), int64(feerateBound),
		"the reserve case needs the feerate check not to fire first")

	ht.OpenChannelAssertErr(
		alice, bob, lntest.OpenChannelParams{
			Amt:                  reserveChanAmt,
			Private:              true,
			CommitmentType:       lnrpc.CommitmentType_STATIC_REMOTE_KEY,
			RemoteChanReserveSat: initiatorReserve,
		}, lnwallet.ErrBalancesBelowReserveBase,
	)

	// The positive direction: the same channel shape opens once the
	// initiator's balance clears the reserve it names.
	aliceFees.SetFeeRate(lowFeeRate, 1)
	bobFees.SetFeeRate(lowFeeRate, 1)

	ht.OpenChannel(alice, bob, lntest.OpenChannelParams{
		Amt:                  reserveChanAmt,
		Private:              true,
		CommitmentType:       lnrpc.CommitmentType_STATIC_REMOTE_KEY,
		RemoteChanReserveSat: initiatorReserve,
	})

	ht.AssertNumPendingOpenChannels(alice, 0)
	ht.AssertNumPendingOpenChannels(bob, 0)
}
