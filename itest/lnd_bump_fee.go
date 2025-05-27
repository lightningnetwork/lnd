package itest

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/sweep"
	"github.com/stretchr/testify/require"
)

// testBumpFeeLowBudget checks that when the requested ideal budget cannot be
// met, the sweeper still sweeps the input with the actual budget.
func testBumpFeeLowBudget(ht *lntest.HarnessTest) {
	// Create a new node with a large `maxfeerate` so it's easier to run the
	// test.
	alice := ht.NewNode("Alice", []string{
		"--sweeper.maxfeerate=10000",
	})

	// Fund Alice 2 UTXOs, each has 100k sats. One of the UTXOs will be used
	// to create a tx which she sends some coins to herself. The other will
	// be used as the budget when CPFPing the above tx.
	coin := btcutil.Amount(100_000)
	ht.FundCoins(coin, alice)
	ht.FundCoins(coin, alice)

	// Alice sends 50k sats to herself.
	tx := ht.SendCoins(alice, alice, coin/2)
	txid := tx.TxHash()

	// Get Alice's wallet balance to calculate the fees used in the above
	// tx.
	resp := alice.RPC.WalletBalance()

	// balance is the expected final balance. Alice's initial balance is
	// 200k sats, with 100k sats as the budget for the sweeping tx, which
	// means her final balance should be 100k sats minus the mining fees
	// used in the above `SendCoins`.
	balance := btcutil.Amount(
		resp.UnconfirmedBalance + resp.ConfirmedBalance,
	)
	fee := coin*2 - balance
	ht.Logf("Alice's expected final balance=%v, fee=%v", balance, fee)

	// Alice now tries to bump the first output on this tx.
	op := &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(0),
	}
	value := btcutil.Amount(tx.TxOut[0].Value)

	// assertPendingSweepResp is a helper closure that asserts the response
	// from `PendingSweep` RPC is returned with expected values. It also
	// returns the sweeping tx for further checks.
	assertPendingSweepResp := func(budget uint64,
		deadline uint32) *wire.MsgTx {

		// Alice should still have one pending sweep.
		pendingSweep := ht.AssertNumPendingSweeps(alice, 1)[0]

		// Validate all fields returned from `PendingSweeps` are as
		// expected.
		require.Equal(ht, op.TxidBytes, pendingSweep.Outpoint.TxidBytes)
		require.Equal(ht, op.OutputIndex,
			pendingSweep.Outpoint.OutputIndex)
		require.Equal(ht, walletrpc.WitnessType_TAPROOT_PUB_KEY_SPEND,
			pendingSweep.WitnessType)
		require.EqualValuesf(ht, value, pendingSweep.AmountSat,
			"amount not matched: want=%d, got=%d", value,
			pendingSweep.AmountSat)
		require.True(ht, pendingSweep.Immediate)

		require.EqualValuesf(ht, budget, pendingSweep.Budget,
			"budget not matched: want=%d, got=%d", budget,
			pendingSweep.Budget)

		// Since the request doesn't specify a deadline, we expect the
		// existing deadline to be used.
		require.Equalf(ht, deadline, pendingSweep.DeadlineHeight,
			"deadline height not matched: want=%d, got=%d",
			deadline, pendingSweep.DeadlineHeight)

		// We expect to see Alice's original tx and her CPFP tx in the
		// mempool.
		txns := ht.GetNumTxsFromMempool(2)

		// Find the sweeping tx - assume it's the first item, if it has
		// the same txid as the parent tx, use the second item.
		sweepTx := txns[0]
		if sweepTx.TxHash() == tx.TxHash() {
			sweepTx = txns[1]
		}

		return sweepTx
	}

	// Use a budget that Alice cannot cover using her wallet UTXOs.
	budget := coin * 2

	// Use a deadlineDelta of 3 such that the fee func is initialized as,
	// - starting fee rate: 1 sat/vbyte
	// - deadline: 3
	// - budget: 200% of Alice's available funds.
	deadlineDelta := 3

	// First bump request - we expect it to succeed as Alice's current funds
	// can cover the fees used here given the position of the fee func is at
	// 0.
	bumpFeeReq := &walletrpc.BumpFeeRequest{
		Outpoint:      op,
		Budget:        uint64(budget),
		Immediate:     true,
		DeadlineDelta: uint32(deadlineDelta),
	}
	alice.RPC.BumpFee(bumpFeeReq)

	// Calculate the deadline height.
	deadline := ht.CurrentHeight() + uint32(deadlineDelta)

	// Assert the pending sweep is created with the expected values:
	// - deadline: 3+current height.
	// - budget: 2x the wallet balance.
	sweepTx1 := assertPendingSweepResp(uint64(budget), deadline)

	// Mine a block to trigger Alice's sweeper to fee bump the tx.
	//
	// Second bump request - we expect it to succeed as Alice's current
	// funds can cover the fees used here, which is 66.7% of her available
	// funds given the position of the fee func is at 1.
	ht.MineEmptyBlocks(1)

	// Assert the old sweeping tx has been replaced.
	ht.AssertTxNotInMempool(sweepTx1.TxHash())

	// Assert a new sweeping tx is made.
	sweepTx2 := assertPendingSweepResp(uint64(budget), deadline)

	// Mine a block to trigger Alice's sweeper to fee bump the tx.
	//
	// Third bump request - we expect it to fail as Alice's current funds
	// cannot cover the fees now, which is 133.3% of her available funds
	// given the position of the fee func is at 2.
	ht.MineEmptyBlocks(1)

	// Assert the above sweeping tx is still in the mempool.
	ht.AssertTxInMempool(sweepTx2.TxHash())

	// Fund Alice 200k sats, which will be used to cover the budget.
	//
	// TODO(yy): We are funding Alice more than enough - at this stage Alice
	// has a confirmed UTXO of `coin` amount in her wallet, so ideally we
	// should only fund another UTXO of `coin` amount. However, since the
	// confirmed wallet UTXO has already been used in sweepTx2, there's no
	// easy way to tell her wallet to reuse that UTXO in the upcoming
	// sweeping tx.
	// To properly fix it, we should provide more granular UTXO management
	// here by leveraing `LeaseOutput` - whenever we use a wallet UTXO, we
	// should lock it first. And when the sweeping attempt fails, we should
	// release it so the UTXO can be used again in another batch.
	walletTx := ht.FundCoinsUnconfirmed(coin*2, alice)

	// Mine a block to confirm the above funding coin.
	//
	// Fourth bump request - we expect it to succeed as Alice's current
	// funds can cover the full budget.
	ht.MineBlockWithTx(walletTx)

	flakeRaceInBitcoinClientNotifications(ht)

	// Assert Alice's previous sweeping tx has been replaced.
	ht.AssertTxNotInMempool(sweepTx2.TxHash())

	// Assert the pending sweep is created with the expected values:
	// - deadline: 3+current height.
	// - budget: 2x the wallet balance.
	sweepTx3 := assertPendingSweepResp(uint64(budget), deadline)
	require.NotEqual(ht, sweepTx2.TxHash(), sweepTx3.TxHash())

	// Mine the sweeping tx.
	ht.MineBlocksAndAssertNumTxes(1, 2)

	// Assert Alice's wallet balance.       a
	ht.WaitForBalanceConfirmed(alice, balance)
}

// testBumpFee checks that when a new input is requested, it's first bumped via
// CPFP, then RBF. Along the way, we check the `BumpFee` can properly update
// the fee function used by supplying new params.
func testBumpFee(ht *lntest.HarnessTest) {
	alice := ht.NewNodeWithCoins("Alice", nil)

	runBumpFee(ht, alice)
}

// runBumpFee checks the `BumpFee` RPC can properly bump the fee of a given
// input.
func runBumpFee(ht *lntest.HarnessTest, alice *node.HarnessNode) {
	// Skip this test for neutrino, as it's not aware of mempool
	// transactions.
	if ht.IsNeutrinoBackend() {
		ht.Skipf("skipping BumpFee test for neutrino backend")
	}

	// startFeeRate is the min fee rate in sats/vbyte. This value should be
	// used as the starting fee rate when the default no deadline is used.
	startFeeRate := uint64(1)

	// We'll start the test by sending Alice some coins, which she'll use
	// to send to herself.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, alice)

	// Alice sends a coin to herself.
	tx := ht.SendCoins(alice, alice, btcutil.SatoshiPerBitcoin)
	txid := tx.TxHash()

	// Alice now tries to bump the first output on this tx.
	op := &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(0),
	}
	value := btcutil.Amount(tx.TxOut[0].Value)

	// assertPendingSweepResp is a helper closure that asserts the response
	// from `PendingSweep` RPC is returned with expected values. It also
	// returns the sweeping tx for further checks.
	assertPendingSweepResp := func(broadcastAttempts uint32, budget uint64,
		deadline uint32, startingFeeRate uint64) *wire.MsgTx {

		err := wait.NoError(func() error {
			// Alice should still have one pending sweep.
			ps := ht.AssertNumPendingSweeps(alice, 1)[0]

			// Validate all fields returned from `PendingSweeps` are
			// as expected.
			//
			// These fields should stay the same during the test so
			// we assert the values without wait.
			require.Equal(ht, op.TxidBytes, ps.Outpoint.TxidBytes)
			require.Equal(ht, op.OutputIndex,
				ps.Outpoint.OutputIndex)
			require.Equal(ht,
				walletrpc.WitnessType_TAPROOT_PUB_KEY_SPEND,
				ps.WitnessType)
			require.EqualValuesf(ht, value, ps.AmountSat,
				"amount not matched: want=%d, got=%d", value,
				ps.AmountSat)

			// The following fields can change during the test so we
			// return an error if they don't match, which will be
			// checked again in this wait call.
			if !ps.Immediate {
				return fmt.Errorf("immediate should be true")
			}

			if broadcastAttempts != ps.BroadcastAttempts {
				return fmt.Errorf("broadcastAttempts not "+
					"matched: want=%d, got=%d",
					broadcastAttempts, ps.BroadcastAttempts)
			}
			if budget != ps.Budget {
				return fmt.Errorf("budget not matched: "+
					"want=%d, got=%d", budget, ps.Budget)
			}

			// Since the request doesn't specify a deadline, we
			// expect the existing deadline to be used.
			if deadline != ps.DeadlineHeight {
				return fmt.Errorf("deadline height not "+
					"matched: want=%d, got=%d", deadline,
					ps.DeadlineHeight)
			}

			// Since the request specifies a starting fee rate, we
			// expect that to be used as the starting fee rate.
			if startingFeeRate != ps.RequestedSatPerVbyte {
				return fmt.Errorf("requested starting fee "+
					"rate not matched: want=%d, got=%d",
					startingFeeRate,
					ps.RequestedSatPerVbyte)
			}

			return nil
		}, wait.DefaultTimeout)
		require.NoError(ht, err, "timeout checking pending sweep")

		// We expect to see Alice's original tx and her CPFP tx in the
		// mempool.
		txns := ht.GetNumTxsFromMempool(2)

		// Find the sweeping tx - assume it's the first item, if it has
		// the same txid as the parent tx, use the second item.
		sweepTx := txns[0]
		if sweepTx.TxHash() == tx.TxHash() {
			sweepTx = txns[1]
		}

		return sweepTx
	}

	// assertFeeRateEqual is a helper closure that asserts the fee rate of
	// the pending sweep tx is equal to the expected fee rate.
	assertFeeRateEqual := func(expected uint64) {
		err := wait.NoError(func() error {
			// Alice should still have one pending sweep.
			pendingSweep := ht.AssertNumPendingSweeps(alice, 1)[0]

			if pendingSweep.SatPerVbyte == expected {
				return nil
			}

			return fmt.Errorf("expected current fee rate %d, got "+
				"%d", expected, pendingSweep.SatPerVbyte)
		}, wait.DefaultTimeout)
		require.NoError(ht, err, "fee rate not updated")
	}

	// assertFeeRateGreater is a helper closure that asserts the fee rate
	// of the pending sweep tx is greater than the expected fee rate.
	assertFeeRateGreater := func(expected uint64) {
		err := wait.NoError(func() error {
			// Alice should still have one pending sweep.
			pendingSweep := ht.AssertNumPendingSweeps(alice, 1)[0]

			if pendingSweep.SatPerVbyte > expected {
				return nil
			}

			return fmt.Errorf("expected current fee rate greater "+
				"than %d, got %d", expected,
				pendingSweep.SatPerVbyte)
		}, wait.DefaultTimeout)
		require.NoError(ht, err, "fee rate not updated")
	}

	// First bump request - we'll specify nothing except `Immediate` to let
	// the sweeper handle the fee, and we expect a fee func that has,
	// - starting fee rate: 1 sat/vbyte (min relay fee rate).
	// - deadline: 1008 (default deadline).
	// - budget: 50% of the input value.
	bumpFeeReq := &walletrpc.BumpFeeRequest{
		Outpoint: op,
		// We use a force param to create the sweeping tx immediately.
		Immediate: true,
	}
	alice.RPC.BumpFee(bumpFeeReq)

	// Since the request doesn't specify a deadline, we expect the default
	// deadline to be used.
	currentHeight := int32(ht.CurrentHeight())
	deadline := uint32(currentHeight + sweep.DefaultDeadlineDelta)

	// Assert the pending sweep is created with the expected values:
	// - broadcast attempts: 1.
	// - starting fee rate: 1 sat/vbyte (min relay fee rate).
	// - deadline: 1008 (default deadline).
	// - budget: 50% of the input value.
	sweepTx1 := assertPendingSweepResp(1, uint64(value/2), deadline, 0)

	// Since the request doesn't specify a starting fee rate, we expect the
	// min relay fee rate is used as the current fee rate.
	assertFeeRateEqual(startFeeRate)

	// First we test the case where we specify the conf target to increase
	// the starting fee rate of the fee function.
	confTargetFeeRate := chainfee.SatPerVByte(50)
	ht.SetFeeEstimateWithConf(confTargetFeeRate.FeePerKWeight(), 3)

	// Second bump request - we will specify the conf target and expect a
	// starting fee rate that is estimated using the provided estimator.
	// - starting fee rate: 50 sat/vbyte (conf target 3).
	// - deadline: 1008 (default deadline).
	// - budget: 50% of the input value.
	bumpFeeReq = &walletrpc.BumpFeeRequest{
		Outpoint: op,
		// We use a force param to create the sweeping tx immediately.
		Immediate:  true,
		TargetConf: 3,
	}

	alice.RPC.BumpFee(bumpFeeReq)

	// Alice's old sweeping tx should be replaced.
	ht.AssertTxNotInMempool(sweepTx1.TxHash())

	// Assert the pending sweep is created with the expected values:
	// - broadcast attempts: 2.
	// - starting fee rate: 50 sat/vbyte (conf target 3).
	// - deadline: 1008 (default deadline).
	// - budget: 50% of the input value.
	sweepTx2 := assertPendingSweepResp(
		2, uint64(value/2), deadline, uint64(confTargetFeeRate),
	)

	// testFeeRate sepcifies a starting fee rate in sat/vbyte.
	const testFeeRate = uint64(100)

	// Third bump request - we will specify the fee rate and expect a fee
	// func to change the starting fee rate of the fee function,
	// - starting fee rate: 100 sat/vbyte.
	// - deadline: 1008 (default deadline).
	// - budget: 50% of the input value.
	bumpFeeReq = &walletrpc.BumpFeeRequest{
		Outpoint: op,
		// We use a force param to create the sweeping tx immediately.
		Immediate:   true,
		SatPerVbyte: testFeeRate,
	}
	alice.RPC.BumpFee(bumpFeeReq)

	// Alice's old sweeping tx should be replaced.
	ht.AssertTxNotInMempool(sweepTx2.TxHash())

	// Assert the pending sweep is created with the expected values:
	// - broadcast attempts: 3.
	// - starting fee rate: 100 sat/vbyte.
	// - deadline: 1008 (default deadline).
	// - budget: 50% of the input value.
	sweepTx3 := assertPendingSweepResp(
		3, uint64(value/2), deadline, testFeeRate,
	)

	// We expect the requested starting fee rate to be the current fee
	// rate.
	assertFeeRateEqual(testFeeRate)

	// testBudget specifies a budget in sats.
	testBudget := uint64(float64(value) * 0.1)

	// Fourth bump request - we will specify the budget and expect a fee
	// func that has,
	// - starting fee rate: 100 sat/vbyte, stays unchanged.
	// - deadline: 1008 (default deadline).
	// - budget: 10% of the input value.
	bumpFeeReq = &walletrpc.BumpFeeRequest{
		Outpoint: op,
		// We use a force param to create the sweeping tx immediately.
		Immediate: true,
		Budget:    testBudget,
	}
	alice.RPC.BumpFee(bumpFeeReq)

	// Alice's old sweeping tx should be replaced.
	ht.AssertTxNotInMempool(sweepTx3.TxHash())

	// Assert the pending sweep is created with the expected values:
	// - broadcast attempts: 4.
	// - starting fee rate: 100 sat/vbyte, stays unchanged.
	// - deadline: 1008 (default deadline).
	// - budget: 10% of the input value.
	sweepTx4 := assertPendingSweepResp(4, testBudget, deadline, testFeeRate)

	// We expect the current fee rate to be increased because we ensure the
	// initial broadcast always succeeds.
	assertFeeRateGreater(testFeeRate)

	// Create a test deadline delta to use in the next test.
	testDeadlineDelta := uint32(100)
	deadlineHeight := uint32(currentHeight) + testDeadlineDelta

	// Fifth bump request - we will specify the deadline and expect a fee
	// func that has,
	// - starting fee rate: 100 sat/vbyte, stays unchanged.
	// - deadline: 100.
	// - budget: 10% of the input value, stays unchanged.
	bumpFeeReq = &walletrpc.BumpFeeRequest{
		Outpoint: op,
		// We use a force param to create the sweeping tx immediately.
		Immediate:     true,
		DeadlineDelta: testDeadlineDelta,
		Budget:        testBudget,
	}
	alice.RPC.BumpFee(bumpFeeReq)

	// Alice's old sweeping tx should be replaced.
	ht.AssertTxNotInMempool(sweepTx4.TxHash())

	// Assert the pending sweep is created with the expected values:
	// - broadcast attempts: 5.
	// - starting fee rate: 100 sat/vbyte, stays unchanged.
	// - deadline: 100.
	// - budget: 10% of the input value, stays unchanged.
	sweepTx5 := assertPendingSweepResp(
		5, testBudget, deadlineHeight, testFeeRate,
	)

	// We expect the current fee rate to be increased because we ensure the
	// initial broadcast always succeeds.
	assertFeeRateGreater(testFeeRate)

	// Sixth bump request - we test the behavior of `Immediate` - every
	// time it's called, the fee function will keep increasing the fee rate
	// until the broadcast can succeed. The fee func that has,
	// - starting fee rate: 100 sat/vbyte, stays unchanged.
	// - deadline: 100, stays unchanged.
	// - budget: 10% of the input value, stays unchanged.
	bumpFeeReq = &walletrpc.BumpFeeRequest{
		Outpoint: op,
		// We use a force param to create the sweeping tx immediately.
		Immediate: true,
	}
	alice.RPC.BumpFee(bumpFeeReq)

	// Alice's old sweeping tx should be replaced.
	ht.AssertTxNotInMempool(sweepTx5.TxHash())

	// Assert the pending sweep is created with the expected values:
	// - broadcast attempts: 6.
	// - starting fee rate: 100 sat/vbyte, stays unchanged.
	// - deadline: 100, stays unchanged.
	// - budget: 10% of the input value, stays unchanged.
	sweepTx6 := assertPendingSweepResp(
		6, testBudget, deadlineHeight, testFeeRate,
	)

	// We expect the current fee rate to be increased because we ensure the
	// initial broadcast always succeeds.
	assertFeeRateGreater(testFeeRate)

	smallBudget := uint64(1000)

	// Finally, we test the behavior of lowering the fee rate. The fee func
	// that has,
	// - starting fee rate: 1 sat/vbyte.
	// - deadline: 1.
	// - budget: 1000 sats.
	bumpFeeReq = &walletrpc.BumpFeeRequest{
		Outpoint: op,
		// We use a force param to create the sweeping tx immediately.
		Immediate:   true,
		SatPerVbyte: startFeeRate,
		// The budget and the deadline delta must be set together.
		Budget:        smallBudget,
		DeadlineDelta: 1,
	}
	alice.RPC.BumpFee(bumpFeeReq)

	// Calculate the ending fee rate, which is used in the above fee bump
	// when fee function's max posistion is reached.
	txWeight := ht.CalculateTxWeight(sweepTx6)
	endingFeeRate := chainfee.NewSatPerKWeight(
		btcutil.Amount(smallBudget), txWeight,
	)

	// Since the fee function has been maxed out, the starting fee rate for
	// the next sweep attempt should be the ending fee rate.
	//
	// TODO(yy): The weight estimator used in the sweeper gives a different
	// result than the weight calculated here, which is the result from
	// `blockchain.GetTransactionWeight`. For this particular tx:
	// - result from the `weightEstimator`: 445 wu
	// - result from `GetTransactionWeight`: 444 wu
	//
	// This means the fee rates are different,
	// - `weightEstimator`: 2247 sat/kw, or 8 sat/vb (8.988 round down)
	// - here we have 2252 sat/kw, or 9 sat/vb (9.008 round down)
	//
	// We should investigate and check whether if it's possible to make the
	// `weightEstimator` more accurate.
	expectedStartFeeRate := uint64(endingFeeRate.FeePerVByte()) - 1

	// Assert the pending sweep is created with the expected values:
	// - broadcast attempts: 7.
	// - starting fee rate: 8 sat/vbyte.
	// - deadline: 1.
	// - budget: 1000 sats.
	sweepTx7 := assertPendingSweepResp(
		7, smallBudget, uint32(currentHeight+1), expectedStartFeeRate,
	)

	// Since this budget is too small to cover the RBF, we expect the
	// sweeping attempt to fail.
	require.Equal(ht, sweepTx6.TxHash(), sweepTx7.TxHash(), "tx6 should "+
		"not be replaced: tx6=%v, tx7=%v", sweepTx6.TxHash(),
		sweepTx7.TxHash())

	// We expect the current fee rate to be increased because we ensure the
	// initial broadcast always succeeds.
	assertFeeRateGreater(testFeeRate)

	// Clean up the mempool.
	ht.MineBlocksAndAssertNumTxes(1, 2)
}

// testBumpFeeExternalInput assert that when the bump fee RPC is called with an
// outpoint unknown to the node's wallet, an error is returned.
func testBumpFeeExternalInput(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", nil)
	bob := ht.NewNode("Bob", nil)

	// We'll start the test by sending Alice some coins, which she'll use
	// to send to Bob.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, alice)

	// Alice sends 0.5 BTC to Bob. This tx should have two outputs - one
	// that belongs to Bob, the other is Alice's change output.
	tx := ht.SendCoins(alice, bob, btcutil.SatoshiPerBitcoin/2)
	txid := tx.TxHash()

	// Find the wrong index to perform the fee bump. We assume the first
	// output belongs to Bob, and switch to the second if the second output
	// has a larger output value. Given we've funded Alice 1 btc, she then
	// sends 0.5 btc to Bob, her change output will be below 0.5 btc after
	// paying the mining fees.
	wrongIndex := 0
	if tx.TxOut[0].Value < tx.TxOut[1].Value {
		wrongIndex = 1
	}

	// Alice now tries to bump the wrong output on this tx.
	op := &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(wrongIndex),
	}

	// Create a request with the wrong outpoint.
	bumpFeeReq := &walletrpc.BumpFeeRequest{
		Outpoint: op,
		// We use a force param to create the sweeping tx immediately.
		Immediate: true,
	}
	err := alice.RPC.BumpFeeAssertErr(bumpFeeReq)
	require.ErrorContains(ht, err, "does not belong to the wallet")

	// Clean up the mempool.
	ht.MineBlocksAndAssertNumTxes(1, 1)
}
