package itest

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

type chanFundMaxTestCase struct {
	// name is the name of the target test case.
	name string

	// initialWalletBalance is the amount in Alice's wallet.
	initialWalletBalance btcutil.Amount

	// pushAmt is the amount to be pushed to Bob.
	pushAmt btcutil.Amount

	// feeRate is an optional fee in satoshi/bytes used when opening a
	// channel.
	feeRate btcutil.Amount

	// expectedBalanceAlice is Alice's expected balance in her channel.
	expectedBalanceAlice btcutil.Amount

	// chanOpenShouldFail denotes if we expect the channel opening to fail.
	chanOpenShouldFail bool

	// expectedErrStr contains the expected error in case chanOpenShouldFail
	// is set to true.
	expectedErrStr string

	// commitmentType allows to define the exact type when opening the
	// channel.
	commitmentType lnrpc.CommitmentType

	// private denotes if the channel opening is announced to the network or
	// not.
	private bool
}

// testChannelFundMax checks various channel funding scenarios where the user
// instructed the wallet to use all remaining funds.
func testChannelFundMax(ht *lntest.HarnessTest) {
	// Create two new nodes that open a channel between each other for these
	// tests.
	args := lntest.NodeArgsForCommitType(lnrpc.CommitmentType_ANCHORS)
	alice := ht.NewNode("Alice", args)
	defer ht.Shutdown(alice)

	bob := ht.NewNode("Bob", args)
	defer ht.Shutdown(bob)

	// Ensure both sides are connected so the funding flow can be properly
	// executed.
	ht.EnsureConnected(alice, bob)

	// Calculate reserve amount for one channel.
	reserveResp, _ := alice.RPC.WalletKit.RequiredReserve(
		context.Background(), &walletrpc.RequiredReserveRequest{
			AdditionalPublicChannels: 1,
		},
	)
	reserveAmount := btcutil.Amount(reserveResp.RequiredReserve)

	var testCases = []*chanFundMaxTestCase{
		{
			name:                 "wallet amount is dust",
			initialWalletBalance: 2_000,
			chanOpenShouldFail:   true,
			feeRate:              20,
			expectedErrStr: "output amount(-0.00000435 BTC) " +
				"after subtracting fees(0.00002435 BTC) " +
				"below dust limit(0.0000033 BTC)",
		},
		{
			name: "wallet amount < min chan size " +
				"(~18000sat)",
			initialWalletBalance: 18_000,
			// Using a feeRate of 1 sat/vByte ensures that we test
			// for min chan size and not excessive fees.
			feeRate:            1,
			chanOpenShouldFail: true,
			expectedErrStr: "available funds(0.00017877 BTC) " +
				"below the minimum amount(0.0002 BTC)",
		},
		{
			name: "wallet amount > min chan " +
				"size (37000sat)",
			initialWalletBalance: 37_000,
			// The transaction fee to open the channel must be
			// subtracted from Alice's balance.
			// (since wallet balance < max-chan-size)
			expectedBalanceAlice: btcutil.Amount(37_000) -
				fundingFee(1, false),
		},
		{
			name: "wallet amount > max chan size " +
				"(20000000sat)",
			initialWalletBalance: 20_000_000,
			expectedBalanceAlice: lnd.MaxFundingAmount,
		},
		// Expects, that if the maximum funding amount for a channel is
		// pushed to the remote side, then the funding flow is failing
		// because the push amount has to be less than the local channel
		// amount.
		{
			name: "wallet amount > max chan size, " +
				"push amount == max-chan-size",
			initialWalletBalance: 20_000_000,
			pushAmt:              lnd.MaxFundingAmount,
			chanOpenShouldFail:   true,
			expectedErrStr: "amount pushed to remote peer for " +
				"initial state must be below the local " +
				"funding amount",
		},
		// Expects that if the maximum funding amount for a channel is
		// pushed to the remote side then the funding flow is failing
		// due to insufficient funds in the local balance to cover for
		// fees in the channel opening. By that the test also ensures
		// that the fees are not covered by the remaining wallet
		// balance.
		{
			name: "wallet amount > max chan size, " +
				"push amount == max-chan-size - 1_000",
			initialWalletBalance: 20_000_000,
			pushAmt:              lnd.MaxFundingAmount - 1_000,
			chanOpenShouldFail:   true,
			expectedErrStr: "funder balance too small (-8050000) " +
				"with fee=9050 sat, minimum=708 sat required",
		},
		{
			name: "wallet amount > max chan size, " +
				"push amount 16766000",
			initialWalletBalance: 20_000_000,
			pushAmt:              16_766_000,
			expectedBalanceAlice: lnd.MaxFundingAmount - 16_766_000,
		},

		{
			name:                 "anchor reserved value",
			initialWalletBalance: 100_000,
			commitmentType:       lnrpc.CommitmentType_ANCHORS,
			expectedBalanceAlice: btcutil.Amount(100_000) -
				fundingFee(1, true) - reserveAmount,
		},
		// Funding a private anchor channel should omit the achor
		// reserve and produce no change output.
		{
			name: "private anchor no reserved " +
				"value",
			private:              true,
			initialWalletBalance: 100_000,
			commitmentType:       lnrpc.CommitmentType_ANCHORS,
			expectedBalanceAlice: btcutil.Amount(100_000) -
				fundingFee(1, false),
		},
	}

	for _, testCase := range testCases {
		success := ht.Run(
			testCase.name, func(tt *testing.T) {
				runFundMaxTestCase(
					ht, tt, alice, bob, testCase,
					reserveAmount,
				)
			},
		)

		// Stop at the first failure. Mimic behavior of original test
		// framework.
		if !success {
			break
		}
	}
}

// runTestCase runs a single test case asserting that test conditions are met.
func runFundMaxTestCase(ht *lntest.HarnessTest, t *testing.T, alice,
	bob *node.HarnessNode, testCase *chanFundMaxTestCase,
	reserveAmount btcutil.Amount) {

	ht.FundCoins(testCase.initialWalletBalance, alice)

	defer func() {
		if testCase.initialWalletBalance <= 2_000 {
			// Add additional funds to sweep "dust" UTXO.
			ht.FundCoins(100_000, alice)
		}

		// Remove all funds from Alice.
		sweepNodeWalletAndAssert(ht, alice)
	}()

	commitType := testCase.commitmentType
	if commitType == lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE {
		commitType = lnrpc.CommitmentType_STATIC_REMOTE_KEY
	}

	// The parameters to try opening the channel with.
	chanParams := lntest.OpenChannelParams{
		Amt:            0,
		PushAmt:        testCase.pushAmt,
		SatPerVByte:    testCase.feeRate,
		CommitmentType: commitType,
		FundMax:        true,
		Private:        testCase.private,
	}

	// If we don't expect the channel opening to be
	// successful, simply check for an error.
	if testCase.chanOpenShouldFail {
		expectedErr := fmt.Errorf(testCase.expectedErrStr)
		ht.OpenChannelAssertErr(
			alice, bob, chanParams, expectedErr,
		)

		return
	}

	// Otherwise, if we expect to open a channel use the helper function.
	chanPoint := ht.OpenChannel(alice, bob, chanParams)

	// Close the channel between Alice and Bob, asserting
	// that the channel has been properly closed on-chain.
	defer ht.CloseChannel(alice, chanPoint)

	cType := ht.GetChannelCommitType(alice, chanPoint)

	// Alice's balance should be her amount subtracted by the commitment
	// transaction fee.
	checkChannelBalance(
		ht, alice,
		testCase.expectedBalanceAlice-lntest.CalcStaticFee(cType, 0),
		testCase.pushAmt,
	)

	// Ensure Bob's balance within the channel is equal to the push amount.
	checkChannelBalance(
		ht, bob, testCase.pushAmt,
		testCase.expectedBalanceAlice-lntest.CalcStaticFee(cType, 0),
	)

	if lntest.CommitTypeHasAnchors(testCase.commitmentType) &&
		!testCase.private {

		ht.AssertWalletAccountBalance(
			alice, lnwallet.DefaultAccountName,
			int64(reserveAmount), 0,
		)
	}
}

// Creates a helper closure to be used below which asserts the proper
// response to a channel balance RPC.
func checkChannelBalance(ht *lntest.HarnessTest, node *node.HarnessNode,
	local, remote btcutil.Amount) {

	expectedResponse := &lnrpc.ChannelBalanceResponse{
		LocalBalance: &lnrpc.Amount{
			Sat:  uint64(local),
			Msat: uint64(lnwire.NewMSatFromSatoshis(local)),
		},
		RemoteBalance: &lnrpc.Amount{
			Sat: uint64(remote),
			Msat: uint64(lnwire.NewMSatFromSatoshis(
				remote,
			)),
		},
		UnsettledLocalBalance:    &lnrpc.Amount{},
		UnsettledRemoteBalance:   &lnrpc.Amount{},
		PendingOpenLocalBalance:  &lnrpc.Amount{},
		PendingOpenRemoteBalance: &lnrpc.Amount{},
		// Deprecated fields.
		Balance: int64(local),
	}
	ht.AssertChannelBalanceResp(node, expectedResponse)
}

// fundingFee returns the fee estimate used for a tx with the given number of
// inputs and the optional change output. This matches the estimate done by the
// wallet.
func fundingFee(numInput int, change bool) btcutil.Amount {
	var weightEstimate input.TxWeightEstimator

	// The standard fee rate used for a funding transaction.
	var feeRate = chainfee.SatPerKWeight(12500)

	// All inputs.
	for i := 0; i < numInput; i++ {
		weightEstimate.AddP2WKHInput()
	}

	// The multisig funding output.
	weightEstimate.AddP2WSHOutput()

	// Optionally count a change output.
	if change {
		weightEstimate.AddP2TROutput()
	}

	totalWeight := int64(weightEstimate.Weight())

	return feeRate.FeeForWeight(totalWeight)
}

// sweepNodeWalletAndAssert sweeps funds from a node wallet.
func sweepNodeWalletAndAssert(ht *lntest.HarnessTest, node *node.HarnessNode) {
	// New miner address we will sweep all funds to.
	minerAddr, err := ht.Miner.NewAddress()
	require.NoError(ht, err)

	// Send all funds back to the miner node.
	node.RPC.SendCoins(&lnrpc.SendCoinsRequest{
		Addr:    minerAddr.String(),
		SendAll: true,
	})

	// Ensures we don't leave any transaction in the mempool after sweeping.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Ensure that the node's balance is 0
	checkChannelBalance(ht, node, 0, 0)
}
