package itest

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// testChannelBalance creates a new channel between Alice and Bob, then checks
// channel balance to be equal amount specified while creation of channel.
func testChannelBalance(ht *lntest.HarnessTest) {
	// Open a channel with 0.16 BTC between Alice and Bob, ensuring the
	// channel has been opened properly.
	amount := funding.MaxBtcFundingAmount

	// Creates a helper closure to be used below which asserts the proper
	// response to a channel balance RPC.
	checkChannelBalance := func(node *node.HarnessNode,
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

	// Before beginning, make sure alice and bob are connected.
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNode("Bob", nil)
	ht.EnsureConnected(alice, bob)

	chanPoint := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: amount},
	)
	cType := ht.GetChannelCommitType(alice, chanPoint)

	// As this is a single funder channel, Alice's balance should be
	// exactly 0.5 BTC since now state transitions have taken place yet.
	checkChannelBalance(alice, amount-lntest.CalcStaticFee(cType, 0), 0)

	// Ensure Bob currently has no available balance within the channel.
	checkChannelBalance(bob, 0, amount-lntest.CalcStaticFee(cType, 0))
}

// testChannelUnsettledBalance will test that the UnsettledBalance field
// is updated according to the number of Pending Htlcs.
// Alice will send Htlcs to Carol while she is in hodl mode. This will result
// in a build of pending Htlcs. We expect the channels unsettled balance to
// equal the sum of all the Pending Htlcs.
func testChannelUnsettledBalance(ht *lntest.HarnessTest) {
	const chanAmt = btcutil.Amount(1000000)

	// Creates a helper closure to be used below which asserts the proper
	// response to a channel balance RPC.
	checkChannelBalance := func(node *node.HarnessNode,
		local, remote, unsettledLocal, unsettledRemote btcutil.Amount) {

		expectedResponse := &lnrpc.ChannelBalanceResponse{
			LocalBalance: &lnrpc.Amount{
				Sat: uint64(local),
				Msat: uint64(lnwire.NewMSatFromSatoshis(
					local,
				)),
			},
			RemoteBalance: &lnrpc.Amount{
				Sat: uint64(remote),
				Msat: uint64(lnwire.NewMSatFromSatoshis(
					remote,
				)),
			},
			UnsettledLocalBalance: &lnrpc.Amount{
				Sat: uint64(unsettledLocal),
				Msat: uint64(lnwire.NewMSatFromSatoshis(
					unsettledLocal,
				)),
			},
			UnsettledRemoteBalance: &lnrpc.Amount{
				Sat: uint64(unsettledRemote),
				Msat: uint64(lnwire.NewMSatFromSatoshis(
					unsettledRemote,
				)),
			},
			PendingOpenLocalBalance:  &lnrpc.Amount{},
			PendingOpenRemoteBalance: &lnrpc.Amount{},
			// Deprecated fields.
			Balance: int64(local),
		}
		ht.AssertChannelBalanceResp(node, expectedResponse)
	}

	// Create carol in hodl mode.
	carol := ht.NewNode("Carol", []string{"--hodl.exit-settle"})

	// Connect Alice to Carol.
	alice := ht.NewNodeWithCoins("Alice", nil)
	ht.ConnectNodes(alice, carol)

	// Open a channel between Alice and Carol.
	chanPointAlice := ht.OpenChannel(
		alice, carol, lntest.OpenChannelParams{Amt: chanAmt},
	)
	cType := ht.GetChannelCommitType(alice, chanPointAlice)

	// Check alice's channel balance, which should have zero remote and zero
	// pending balance.
	checkChannelBalance(
		alice, chanAmt-lntest.CalcStaticFee(cType, 0), 0, 0, 0,
	)

	// Check carol's channel balance, which should have zero local and zero
	// pending balance.
	checkChannelBalance(
		carol, 0, chanAmt-lntest.CalcStaticFee(cType, 0), 0, 0,
	)

	// Channel should be ready for payments.
	const (
		payAmt      = 100
		numInvoices = 6
	)

	// Simulateneously send numInvoices payments from Alice to Carol.
	for i := 0; i < numInvoices; i++ {
		go func() {
			req := &routerrpc.SendPaymentRequest{
				Dest:           carol.PubKey[:],
				Amt:            int64(payAmt),
				PaymentHash:    ht.Random32Bytes(),
				FinalCltvDelta: finalCltvDelta,
				FeeLimitMsat:   noFeeLimitMsat,
			}
			ht.SendPaymentAssertInflight(alice, req)
		}()
	}

	// There should be a number of PendingHtlcs equal
	// to the amount of Invoices sent.
	ht.AssertNumActiveHtlcs(alice, numInvoices)
	ht.AssertNumActiveHtlcs(carol, numInvoices)

	// Set the amount expected for the Unsettled Balance for this channel.
	expectedBalance := numInvoices * payAmt

	// Test that the UnsettledBalance for both Alice and Carol
	// is equal to the amount of invoices * payAmt.
	checkUnsettledBalance := func() error {
		// Get channel info for the Alice.
		chanInfo := ht.QueryChannelByChanPoint(alice, chanPointAlice)

		// Check that UnsettledBalance is what we expect.
		if int(chanInfo.UnsettledBalance) != expectedBalance {
			return fmt.Errorf("unsettled balance failed "+
				"expected: %v, received: %v", expectedBalance,
				chanInfo.UnsettledBalance)
		}

		// Get channel info for the Carol.
		chanInfo = ht.QueryChannelByChanPoint(carol, chanPointAlice)

		// Check that UnsettledBalance is what we expect.
		if int(chanInfo.UnsettledBalance) != expectedBalance {
			return fmt.Errorf("unsettled balance failed "+
				"expected: %v, received: %v", expectedBalance,
				chanInfo.UnsettledBalance)
		}

		return nil
	}
	require.NoError(ht, wait.NoError(checkUnsettledBalance, defaultTimeout),
		"timeout while checking unsettled balance")

	// Check alice's channel balance, which should have a remote unsettled
	// balance that equals to the amount of invoices * payAmt. The remote
	// balance remains zero.
	fee := lntest.CalcStaticFee(cType, 0)
	aliceLocal := chanAmt - fee - numInvoices*payAmt
	checkChannelBalance(alice, aliceLocal, 0, 0, numInvoices*payAmt)

	// Check carol's channel balance, which should have a local unsettled
	// balance that equals to the amount of invoices * payAmt. The local
	// balance remains zero.
	checkChannelBalance(carol, 0, aliceLocal, numInvoices*payAmt, 0)
}
