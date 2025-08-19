package itest

import (
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

var fundUtxoSelectionTestCases = []*lntest.TestCase{
	{
		Name:     "funding error",
		TestFunc: testChannelUtxoSelectionError,
	},
	{
		Name:     "selected valid chan size",
		TestFunc: testUtxoSelectionSelectedValidChanSize,
	},
	{
		Name:     "selected valid chan reserve",
		TestFunc: testUtxoSelectionSelectedValidChanReserve,
	},
	{
		Name:     "selected reserve from selected",
		TestFunc: testUtxoSelectionReserveFromSelected,
	},
	{
		Name:     "fundmax",
		TestFunc: testUtxoSelectionFundmax,
	},
	{
		Name:     "fundmax reserve",
		TestFunc: testUtxoSelectionFundmaxReserve,
	},
	{
		Name:     "reused utxo",
		TestFunc: testUtxoSelectionReuseUTXO,
	},
}

type chanFundUtxoSelectionTestCase struct {
	// name is the name of the target test case.
	name string

	// initialCoins are the initial coins in Alice's wallet.
	initialCoins []btcutil.Amount

	// selectedCoins are the coins alice is selecting for funding a channel.
	selectedCoins []btcutil.Amount

	// localAmt is the local portion of the channel funding amount.
	localAmt btcutil.Amount

	// pushAmt is the amount to be pushed to Bob.
	pushAmt btcutil.Amount

	// feeRate is an optional fee in satoshi/bytes used when opening a
	// channel.
	feeRate btcutil.Amount

	// expectedBalance is Alice's expected balance in her channel.
	expectedBalance btcutil.Amount

	// remainingWalletBalance is Alice's expected remaining wallet balance
	// after she opened a channgel.
	remainingWalletBalance btcutil.Amount

	// chanOpenShouldFail denotes if we expect the channel opening to fail.
	chanOpenShouldFail bool

	// expectedErrStr contains the expected error in case chanOpenShouldFail
	// is set to true.
	expectedErrStr string

	// commitmentType allows to define the exact type when opening the
	// channel.
	commitmentType lnrpc.CommitmentType

	// reuseUtxo tries to spent a previously spent output.
	reuseUtxo bool
}

// testChannelUtxoSelectionError checks various channel funding error scenarios
// where the user instructed the wallet to use a selection funds available in
// the wallet.
func testChannelUtxoSelectionError(ht *lntest.HarnessTest) {
	// Create two new nodes that open a channel between each other for these
	// tests.
	args := lntest.NodeArgsForCommitType(lnrpc.CommitmentType_ANCHORS)
	alice := ht.NewNode("Alice", args)
	bob := ht.NewNode("Bob", args)

	// Ensure both sides are connected so the funding flow can be properly
	// executed.
	ht.EnsureConnected(alice, bob)

	// Calculate reserve amount for one channel.
	reserveResp, _ := alice.RPC.WalletKit.RequiredReserve(
		ht.Context(), &walletrpc.RequiredReserveRequest{
			AdditionalPublicChannels: 1,
		},
	)

	reserveAmount := btcutil.Amount(reserveResp.RequiredReserve)

	var tcs = []*chanFundUtxoSelectionTestCase{
		// Selected coins would leave a dust output after subtracting
		// miner fees.
		{
			name:               "fundmax, wallet amount is dust",
			initialCoins:       []btcutil.Amount{2_000},
			selectedCoins:      []btcutil.Amount{2_000},
			chanOpenShouldFail: true,
			feeRate:            15,
			expectedErrStr: "output amount(0.00000174 BTC) after " +
				"subtracting fees(0.00001826 BTC) below dust " +
				"limit(0.00000330 BTC)",
		},
		// Selected coins don't cover the minimum channel size.
		{
			name: "fundmax, local amount < min chan " +
				"size",
			initialCoins:       []btcutil.Amount{18_000},
			selectedCoins:      []btcutil.Amount{18_000},
			feeRate:            1,
			chanOpenShouldFail: true,
			expectedErrStr: "available funds(0.00017877 BTC) " +
				"below the minimum amount(0.00020000 BTC)",
		},
		// The local amount exceeds the value of the selected coins.
		{
			name: "selected, local amount > " +
				"selected amount",
			initialCoins:       []btcutil.Amount{100_000, 50_000},
			selectedCoins:      []btcutil.Amount{100_000},
			localAmt:           btcutil.Amount(210_337),
			chanOpenShouldFail: true,
			expectedErrStr: "not enough witness outputs to " +
				"create funding transaction, need 0.00210337 " +
				"BTC only have 0.00100000 BTC available",
		},
		// Select all coins in wallet and use more than the maximum
		// available local amount to fund an anchor channel.
		{
			name: "selected, local amount leaves insufficient " +
				"reserve",
			initialCoins: []btcutil.Amount{
				200_000, 100_000,
			},
			selectedCoins:  []btcutil.Amount{200_000, 100_000},
			commitmentType: lnrpc.CommitmentType_ANCHORS,
			localAmt: btcutil.Amount(300_000) -
				reserveAmount + 1 - fundingFee(2, true),
			chanOpenShouldFail: true,
			expectedErrStr: "reserved wallet balance " +
				"invalidated: transaction would leave " +
				"insufficient funds for fee bumping anchor " +
				"channel closings",
		},
	}

	for _, tc := range tcs {
		success := ht.Run(
			tc.name, func(tt *testing.T) {
				runUtxoSelectionTestCase(
					ht, alice, bob, tc, reserveAmount,
				)
			},
		)

		// Stop at the first failure. Mimic behavior of original test
		if !success {
			break
		}
	}
}

// testChannelUtxoSelection checks various channel funding scenarios where the
// user instructed the wallet to use a selection funds available in the wallet.
func testUtxoSelectionSelectedValidChanSize(ht *lntest.HarnessTest) {
	// Create two new nodes that open a channel between each other for these
	// tests.
	args := lntest.NodeArgsForCommitType(lnrpc.CommitmentType_ANCHORS)
	alice := ht.NewNode("Alice", args)
	bob := ht.NewNode("Bob", args)

	// Ensure both sides are connected so the funding flow can be properly
	// executed.
	ht.EnsureConnected(alice, bob)

	// Calculate reserve amount for one channel.
	reserveResp, _ := alice.RPC.WalletKit.RequiredReserve(
		ht.Context(), &walletrpc.RequiredReserveRequest{
			AdditionalPublicChannels: 1,
		},
	)

	reserveAmount := btcutil.Amount(reserveResp.RequiredReserve)

	// We are spending two selected coins partially out of three available
	// in the wallet and expect a change output and the unselected coin as
	// remaining wallet balance.
	tc := &chanFundUtxoSelectionTestCase{
		name: "selected, local amount > min chan size",
		initialCoins: []btcutil.Amount{
			200_000, 50_000, 100_000,
		},
		selectedCoins: []btcutil.Amount{
			200_000, 100_000,
		},
		localAmt:        btcutil.Amount(250_000),
		expectedBalance: btcutil.Amount(250_000),
		remainingWalletBalance: btcutil.Amount(350_000) -
			btcutil.Amount(250_000) - fundingFee(2, true),
	}

	runUtxoSelectionTestCase(ht, alice, bob, tc, reserveAmount)
}

// testChannelUtxoSelection checks various channel funding scenarios where the
// user instructed the wallet to use a selection funds available in the wallet.
func testUtxoSelectionSelectedValidChanReserve(ht *lntest.HarnessTest) {
	// Create two new nodes that open a channel between each other for these
	// tests.
	args := lntest.NodeArgsForCommitType(lnrpc.CommitmentType_ANCHORS)
	alice := ht.NewNode("Alice", args)
	bob := ht.NewNode("Bob", args)

	// Ensure both sides are connected so the funding flow can be properly
	// executed.
	ht.EnsureConnected(alice, bob)

	// Calculate reserve amount for one channel.
	reserveResp, _ := alice.RPC.WalletKit.RequiredReserve(
		ht.Context(), &walletrpc.RequiredReserveRequest{
			AdditionalPublicChannels: 1,
		},
	)

	reserveAmount := btcutil.Amount(reserveResp.RequiredReserve)

	// Select all coins in wallet and use the maximum available
	// local amount to fund an anchor channel.
	tc := &chanFundUtxoSelectionTestCase{
		name: "selected, local amount leaves sufficient reserve",
		initialCoins: []btcutil.Amount{
			200_000, 100_000,
		},
		selectedCoins:  []btcutil.Amount{200_000, 100_000},
		commitmentType: lnrpc.CommitmentType_ANCHORS,
		localAmt: btcutil.Amount(300_000) -
			reserveAmount - fundingFee(2, true),
		expectedBalance: btcutil.Amount(300_000) -
			reserveAmount - fundingFee(2, true),
		remainingWalletBalance: reserveAmount,
	}

	runUtxoSelectionTestCase(ht, alice, bob, tc, reserveAmount)
}

// testChannelUtxoSelection checks various channel funding scenarios where the
// user instructed the wallet to use a selection funds available in the wallet.
func testUtxoSelectionReserveFromSelected(ht *lntest.HarnessTest) {
	// Create two new nodes that open a channel between each other for these
	// tests.
	args := lntest.NodeArgsForCommitType(lnrpc.CommitmentType_ANCHORS)
	alice := ht.NewNode("Alice", args)
	bob := ht.NewNode("Bob", args)

	// Ensure both sides are connected so the funding flow can be properly
	// executed.
	ht.EnsureConnected(alice, bob)

	// Calculate reserve amount for one channel.
	reserveResp, _ := alice.RPC.WalletKit.RequiredReserve(
		ht.Context(), &walletrpc.RequiredReserveRequest{
			AdditionalPublicChannels: 1,
		},
	)

	reserveAmount := btcutil.Amount(reserveResp.RequiredReserve)

	// Select all coins in wallet towards local amount except for an anchor
	// reserve portion. Because the UTXOs are sorted by size by default,
	// the reserve amount is just left in the wallet.
	tc := &chanFundUtxoSelectionTestCase{
		name: "selected, reserve from selected",
		initialCoins: []btcutil.Amount{
			200_000, reserveAmount, 100_000,
		},
		selectedCoins: []btcutil.Amount{
			200_000, reserveAmount, 100_000,
		},
		commitmentType: lnrpc.CommitmentType_ANCHORS,
		localAmt: btcutil.Amount(300_000) -
			fundingFee(2, true),
		expectedBalance: btcutil.Amount(300_000) -
			fundingFee(2, true),
		remainingWalletBalance: reserveAmount,
	}

	runUtxoSelectionTestCase(ht, alice, bob, tc, reserveAmount)
}

// testChannelUtxoSelection checks various channel funding scenarios where the
// user instructed the wallet to use a selection funds available in the wallet.
func testUtxoSelectionFundmax(ht *lntest.HarnessTest) {
	// Create two new nodes that open a channel between each other for these
	// tests.
	args := lntest.NodeArgsForCommitType(lnrpc.CommitmentType_ANCHORS)
	alice := ht.NewNode("Alice", args)
	bob := ht.NewNode("Bob", args)

	// Ensure both sides are connected so the funding flow can be properly
	// executed.
	ht.EnsureConnected(alice, bob)

	// Calculate reserve amount for one channel.
	reserveResp, _ := alice.RPC.WalletKit.RequiredReserve(
		ht.Context(), &walletrpc.RequiredReserveRequest{
			AdditionalPublicChannels: 1,
		},
	)

	reserveAmount := btcutil.Amount(reserveResp.RequiredReserve)

	// We fund an anchor channel with a single coin and just keep enough
	// funds in the wallet to cover for the anchor reserve.
	tc := &chanFundUtxoSelectionTestCase{
		name: "fundmax, sufficient reserve",
		initialCoins: []btcutil.Amount{
			200_000, reserveAmount,
		},
		selectedCoins:  []btcutil.Amount{200_000},
		commitmentType: lnrpc.CommitmentType_ANCHORS,
		expectedBalance: btcutil.Amount(200_000) -
			fundingFee(1, false),
		remainingWalletBalance: reserveAmount,
	}

	runUtxoSelectionTestCase(ht, alice, bob, tc, reserveAmount)
}

// testChannelUtxoSelection checks various channel funding scenarios where the
// user instructed the wallet to use a selection funds available in the wallet.
func testUtxoSelectionFundmaxReserve(ht *lntest.HarnessTest) {
	// Create two new nodes that open a channel between each other for these
	// tests.
	args := lntest.NodeArgsForCommitType(lnrpc.CommitmentType_ANCHORS)
	alice := ht.NewNode("Alice", args)
	bob := ht.NewNode("Bob", args)

	// Ensure both sides are connected so the funding flow can be properly
	// executed.
	ht.EnsureConnected(alice, bob)

	// Calculate reserve amount for one channel.
	reserveResp, _ := alice.RPC.WalletKit.RequiredReserve(
		ht.Context(), &walletrpc.RequiredReserveRequest{
			AdditionalPublicChannels: 1,
		},
	)

	reserveAmount := btcutil.Amount(reserveResp.RequiredReserve)

	// We fund an anchor channel with a single coin and expect the reserve
	// amount left in the wallet.
	tc := &chanFundUtxoSelectionTestCase{
		name: "fundmax, sufficient reserve from channel " +
			"balance carve out",
		initialCoins: []btcutil.Amount{
			200_000,
		},
		selectedCoins:  []btcutil.Amount{200_000},
		commitmentType: lnrpc.CommitmentType_ANCHORS,
		expectedBalance: btcutil.Amount(200_000) -
			reserveAmount - fundingFee(1, true),
		remainingWalletBalance: reserveAmount,
	}

	runUtxoSelectionTestCase(ht, alice, bob, tc, reserveAmount)
}

// testChannelUtxoSelection checks various channel funding scenarios where the
// user instructed the wallet to use a selection funds available in the wallet.
func testUtxoSelectionReuseUTXO(ht *lntest.HarnessTest) {
	// Create two new nodes that open a channel between each other for these
	// tests.
	args := lntest.NodeArgsForCommitType(lnrpc.CommitmentType_ANCHORS)
	alice := ht.NewNode("Alice", args)
	bob := ht.NewNode("Bob", args)

	// Ensure both sides are connected so the funding flow can be properly
	// executed.
	ht.EnsureConnected(alice, bob)

	// Calculate reserve amount for one channel.
	reserveResp, _ := alice.RPC.WalletKit.RequiredReserve(
		ht.Context(), &walletrpc.RequiredReserveRequest{
			AdditionalPublicChannels: 1,
		},
	)

	reserveAmount := btcutil.Amount(reserveResp.RequiredReserve)

	// Confirm that already spent outputs can't be reused to fund another
	// channel.
	tc := &chanFundUtxoSelectionTestCase{
		name: "output already spent",
		initialCoins: []btcutil.Amount{
			200_000,
		},
		selectedCoins: []btcutil.Amount{200_000},
		reuseUtxo:     true,
	}

	runUtxoSelectionTestCase(ht, alice, bob, tc, reserveAmount)
}

// runUtxoSelectionTestCase runs a single test case asserting that test
// conditions are met.
func runUtxoSelectionTestCase(ht *lntest.HarnessTest, alice,
	bob *node.HarnessNode, tc *chanFundUtxoSelectionTestCase,
	reserveAmount btcutil.Amount) {

	// Fund initial coins.
	for _, initialCoin := range tc.initialCoins {
		ht.FundCoins(initialCoin, alice)
	}

	// Create an outpoint lookup for each unique amount.
	lookup := make(map[int64]*lnrpc.OutPoint)
	res := alice.RPC.ListUnspent(&walletrpc.ListUnspentRequest{})
	for _, utxo := range res.Utxos {
		lookup[utxo.AmountSat] = utxo.Outpoint
	}

	// Map the selected coin to the respective outpoint.
	selectedOutpoints := []*lnrpc.OutPoint{}
	for _, selectedCoin := range tc.selectedCoins {
		if outpoint, ok := lookup[int64(selectedCoin)]; ok {
			selectedOutpoints = append(
				selectedOutpoints, outpoint,
			)
		}
	}

	commitType := tc.commitmentType
	if commitType == lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE {
		commitType = lnrpc.CommitmentType_STATIC_REMOTE_KEY
	}

	// The parameters to try opening the channel with.
	fundMax := false
	if tc.localAmt == 0 {
		fundMax = true
	}
	chanParams := lntest.OpenChannelParams{
		Amt:            tc.localAmt,
		FundMax:        fundMax,
		PushAmt:        tc.pushAmt,
		CommitmentType: commitType,
		SatPerVByte:    tc.feeRate,
		Outpoints:      selectedOutpoints,
	}

	// If we don't expect the channel opening to be
	// successful, simply check for an error.
	if tc.chanOpenShouldFail {
		expectedErr := errors.New(tc.expectedErrStr)
		ht.OpenChannelAssertErr(alice, bob, chanParams, expectedErr)

		// Fund additional coins to sweep in case the wallet contains
		// dust.
		ht.FundCoins(100_000, alice)

		// Remove all funds from Alice.
		sweepNodeWalletAndAssert(ht, alice)

		return
	}

	// Otherwise, if we expect to open a channel use the helper function.
	chanPoint := ht.OpenChannel(alice, bob, chanParams)
	defer ht.CloseChannel(alice, chanPoint)

	// When re-selecting a spent output for funding another channel we
	// expect the respective error message.
	if tc.reuseUtxo {
		expectedErrStr := fmt.Sprintf("outpoint already spent or "+
			"locked by another subsystem: %s:%d",
			selectedOutpoints[0].TxidStr,
			selectedOutpoints[0].OutputIndex)
		expectedErr := errors.New(expectedErrStr)
		ht.OpenChannelAssertErr(
			alice, bob, chanParams, expectedErr,
		)

		return
	}

	cType := ht.GetChannelCommitType(alice, chanPoint)

	// Alice's balance should be her amount subtracted by the commitment
	// transaction fee.
	checkChannelBalance(
		ht, alice, tc.expectedBalance-lntest.CalcStaticFee(cType, 0),
		tc.pushAmt,
	)

	// Ensure Bob's balance within the channel is equal to the push amount.
	checkChannelBalance(
		ht, bob, tc.pushAmt,
		tc.expectedBalance-lntest.CalcStaticFee(cType, 0),
	)

	ht.AssertWalletAccountBalance(
		alice, lnwallet.DefaultAccountName,
		int64(tc.remainingWalletBalance),
		0,
	)

	// Ensure the anchor channel reserve was carved out.
	if commitType == lnrpc.CommitmentType_ANCHORS {
		balance := alice.RPC.WalletBalance()
		require.EqualValues(
			ht, reserveAmount, balance.ReservedBalanceAnchorChan,
		)
	}
}
