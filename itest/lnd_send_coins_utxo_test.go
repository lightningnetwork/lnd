package itest

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

const (
	defaultLnrpcFee = chainfee.SatPerKWeight(12500)
)

type sendCoinTestCase struct {
	// name is the name of the target test case.
	name string

	// initialCoins are the initial coins in Alice's wallet.
	initialCoins []btcutil.Amount

	// amt specifies the sum to be sent.
	amt int64

	// feeRate is an optional fee in satoshi/bytes used when completing the
	// transaction.
	feeRate btcutil.Amount

	// sendCoinShouldFail denotes if we expect the channel opening to fail.
	sendCoinShouldFail bool

	// expectedErrStr contains the expected error in case chanOpenShouldFail
	// is set to true.
	expectedErrStr string

	// reuseUtxo tries to spent a previously spent output.
	reuseUtxo bool

	sweepAll      bool
	selectedCoins []btcutil.Amount
}

// testChannelUtxoSelection checks various channel funding scenarios where the
// user instructed the wallet to use a selection funds available in the wallet.
func testSendCoin(ht *lntest.HarnessTest) {
	// Create two new nodes that open a channel between each other for these
	// tests.
	alice := ht.NewNode("Alice", nil)
	defer ht.Shutdown(alice)

	var tcs = []*sendCoinTestCase{
		{
			name: "selected amount > " +
				"selected coin input",
			initialCoins: []btcutil.Amount{100_000, 50_000,
				300_000},
			selectedCoins:      []btcutil.Amount{100_000},
			amt:                220_000,
			sendCoinShouldFail: true,
			expectedErrStr: "rpc error: code = Unknown desc = " +
				"insufficient funds available to construct " +
				"transaction",
		},
		{
			name: "spending a selected utxos partially",
			initialCoins: []btcutil.Amount{
				200_000, 700_000,
				100_000, 800_000,
			},
			selectedCoins: []btcutil.Amount{
				200_000, 100_000,
			},
			amt: 250_000,
		},
		// Confirm that already spent outputs can't be reused to fund
		// another channel.
		{
			name: "output already spent",
			initialCoins: []btcutil.Amount{
				200_000, 600_000, 500_000,
			},
			selectedCoins: []btcutil.Amount{200_000},
			amt:           50_000,
			reuseUtxo:     true,
		},
		{
			name: "sending coin with no utxo specified, but amount" +
				"specified, insufficient input amount",
			initialCoins: []btcutil.Amount{
				200_000,
			},
			amt:                200_000,
			sendCoinShouldFail: true,
			expectedErrStr: "rpc error: code = Unknown desc = " +
				"insufficient funds available to construct " +
				"transaction",
		},
		{
			name: "sending coin with no utxo specified, but amount" +
				"specified, sufficient input",
			initialCoins: []btcutil.Amount{
				200_000, 97_000, 1_000,
			},
			amt: 250_000,
		},
		// amount greater than wallet balance
		{
			name: "sending coin with no utxo specified, but amount" +
				"specified, amount > initialCoins",
			initialCoins: []btcutil.Amount{
				200_000,
			},
			amt:                250_000,
			sendCoinShouldFail: true,
			expectedErrStr: "rpc error: code = Unknown desc = " +
				"insufficient funds available to construct " +
				"transaction",
		},
		// sweep all coins from utxos
		{
			name: "sweep all coins in selected utxo",
			initialCoins: []btcutil.Amount{
				200_000, 100_000, 50_000,
			},
			selectedCoins: []btcutil.Amount{
				200_000, 50_000,
			},
			sweepAll: true,
		},
	}

	for _, tc := range tcs {
		success := ht.Run(
			tc.name, func(tt *testing.T) {
				runSendCoinTestCase(
					ht, tt, alice, tc,
				)
			},
		)

		// Stop at the first failure. Mimic behavior of original test
		if !success {
			break
		}
	}
}

// runUtxoSelectionTestCase runs a single test case asserting that test
// conditions are met.
func runSendCoinTestCase(ht *lntest.HarnessTest, t *testing.T, alice *node.
	HarnessNode, tc *sendCoinTestCase) {

	// fund initial coins
	totalinitialCoinSum := 0
	for _, initialCoin := range tc.initialCoins {
		ht.FundCoins(initialCoin, alice)
		totalinitialCoinSum = totalinitialCoinSum + int(initialCoin)
	}

	ht.AssertWalletAccountBalance(
		alice, lnwallet.DefaultAccountName,
		int64(totalinitialCoinSum),
		0,
	)

	defer func() {
		// Fund additional coins to sweep in case the wallet contains
		// dust.
		ht.FundCoins(100_000, alice)

		// Remove all funds from Alice.
		sweepNodeWalletAndAssert(ht, alice)
	}()

	// Create an outpoint lookup for each unique amount.
	lookup := make(map[int64]*lnrpc.OutPoint)
	res := alice.RPC.ListUnspent(&walletrpc.ListUnspentRequest{})
	for _, utxo := range res.Utxos {
		lookup[utxo.AmountSat] = utxo.Outpoint
	}

	// Map the selected coin to the respective outpoint.
	selectedOutpoints := []*lnrpc.OutPoint{}
	selectedOutpointsLookup := make(map[string]struct{})
	for _, selectedCoin := range tc.selectedCoins {
		if outpoint, ok := lookup[int64(selectedCoin)]; ok {
			selectedOutpointsLookup[outpoint.TxidStr] = struct{}{}
			selectedOutpoints = append(selectedOutpoints,
				outpoint)
		}
	}

	// Generate address for the request.
	p2wkhAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		make([]byte, 20), harnessNetParams,
	)

	require.NoError(t, err, "error generating target address "+
		"for request")

	sendCoinReq := &lnrpc.SendCoinsRequest{
		Addr:      p2wkhAddr.String(),
		SendAll:   tc.sweepAll,
		Outpoints: selectedOutpoints,
		Amount:    tc.amt,
	}

	// If we don't expect the send coin to work, we should
	// simply check for an error.
	if tc.sendCoinShouldFail {
		alice.RPC.SendCoinsAssertCheckErr(sendCoinReq, tc.expectedErrStr)
		return
	}

	alice.RPC.SendCoins(sendCoinReq)
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	tx := block.Transactions[1]

	if len(selectedOutpoints) > 0 {
		require.Equal(t, len(tx.TxIn),
			len(selectedOutpoints))

		for _, prevOutpoint := range tx.TxIn {
			_, ok := selectedOutpointsLookup[prevOutpoint.
				PreviousOutPoint.Hash.String()]
			require.True(t, ok, "previous outpoint in "+
				"transaction do not contain expected outpoints")
		}
	}

	// When re-selecting a spent output for funding another channel we
	// expect the respective error message.
	if tc.reuseUtxo {
		//expectedErrStr := fmt.Sprintf("outpoint already spent: %s:%d",
		//	selectedOutpoints[0].TxidStr,
		//	selectedOutpoints[0].OutputIndex)
		//expectedErr := fmt.Errorf(expectedErrStr)
		alice.RPC.SendCoinsAssertErr(sendCoinReq)
		return
	}
}
