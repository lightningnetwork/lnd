package itest

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// testChannelBalance creates a new channel between Alice and Bob, then checks
// channel balance to be equal amount specified while creation of channel.
func testChannelBalance(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// Open a channel with 0.16 BTC between Alice and Bob, ensuring the
	// channel has been opened properly.
	amount := funding.MaxBtcFundingAmount

	// Creates a helper closure to be used below which asserts the proper
	// response to a channel balance RPC.
	checkChannelBalance := func(node *lntest.HarnessNode,
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
		assertChannelBalanceResp(t, node, expectedResponse)
	}

	// Before beginning, make sure alice and bob are connected.
	net.EnsureConnected(t.t, net.Alice, net.Bob)

	chanPoint := openChannelAndAssert(
		t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: amount,
		},
	)

	// Wait for both Alice and Bob to recognize this new channel.
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't advertise channel before "+
			"timeout: %v", err)
	}

	cType, err := channelCommitType(net.Alice, chanPoint)
	if err != nil {
		t.Fatalf("unable to get channel type: %v", err)
	}

	// As this is a single funder channel, Alice's balance should be
	// exactly 0.5 BTC since now state transitions have taken place yet.
	checkChannelBalance(net.Alice, amount-cType.calcStaticFee(0), 0)

	// Ensure Bob currently has no available balance within the channel.
	checkChannelBalance(net.Bob, 0, amount-cType.calcStaticFee(0))

	// Finally close the channel between Alice and Bob, asserting that the
	// channel has been properly closed on-chain.
	closeChannelAndAssert(t, net, net.Alice, chanPoint, false)
}

// testChannelUnsettledBalance will test that the UnsettledBalance field
// is updated according to the number of Pending Htlcs.
// Alice will send Htlcs to Carol while she is in hodl mode. This will result
// in a build of pending Htlcs. We expect the channels unsettled balance to
// equal the sum of all the Pending Htlcs.
func testChannelUnsettledBalance(net *lntest.NetworkHarness, t *harnessTest) {
	const chanAmt = btcutil.Amount(1000000)
	ctxb := context.Background()

	// Creates a helper closure to be used below which asserts the proper
	// response to a channel balance RPC.
	checkChannelBalance := func(node *lntest.HarnessNode,
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
		assertChannelBalanceResp(t, node, expectedResponse)
	}

	// Create carol in hodl mode.
	carol := net.NewNode(t.t, "Carol", []string{"--hodl.exit-settle"})
	defer shutdownAndAssert(net, t, carol)

	// Connect Alice to Carol.
	net.ConnectNodes(t.t, net.Alice, carol)

	// Open a channel between Alice and Carol.
	chanPointAlice := openChannelAndAssert(
		t, net, net.Alice, carol,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Wait for Alice and Carol to receive the channel edge from the
	// funding manager.
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPointAlice)
	if err != nil {
		t.Fatalf("alice didn't see the alice->carol channel before "+
			"timeout: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointAlice)
	if err != nil {
		t.Fatalf("alice didn't see the alice->carol channel before "+
			"timeout: %v", err)
	}

	cType, err := channelCommitType(net.Alice, chanPointAlice)
	require.NoError(t.t, err, "unable to get channel type")

	// Check alice's channel balance, which should have zero remote and zero
	// pending balance.
	checkChannelBalance(net.Alice, chanAmt-cType.calcStaticFee(0), 0, 0, 0)

	// Check carol's channel balance, which should have zero local and zero
	// pending balance.
	checkChannelBalance(carol, 0, chanAmt-cType.calcStaticFee(0), 0, 0)

	// Channel should be ready for payments.
	const (
		payAmt      = 100
		numInvoices = 6
	)

	// Simulateneously send numInvoices payments from Alice to Carol.
	carolPubKey := carol.PubKey[:]
	errChan := make(chan error)
	for i := 0; i < numInvoices; i++ {
		go func() {
			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			_, err := net.Alice.RouterClient.SendPaymentV2(ctxt,
				&routerrpc.SendPaymentRequest{
					Dest:           carolPubKey,
					Amt:            int64(payAmt),
					PaymentHash:    makeFakePayHash(t),
					FinalCltvDelta: chainreg.DefaultBitcoinTimeLockDelta,
					TimeoutSeconds: 60,
					FeeLimitMsat:   noFeeLimitMsat,
				})

			if err != nil {
				errChan <- err
			}
		}()
	}

	// Test that the UnsettledBalance for both Alice and Carol
	// is equal to the amount of invoices * payAmt.
	var unsettledErr error
	nodes := []*lntest.HarnessNode{net.Alice, carol}
	err = wait.Predicate(func() bool {
		// There should be a number of PendingHtlcs equal
		// to the amount of Invoices sent.
		unsettledErr = assertNumActiveHtlcs(nodes, numInvoices)
		if unsettledErr != nil {
			return false
		}

		// Set the amount expected for the Unsettled Balance for
		// this channel.
		expectedBalance := numInvoices * payAmt

		// Check each nodes UnsettledBalance field.
		for _, node := range nodes {
			// Get channel info for the node.
			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			chanInfo, err := getChanInfo(ctxt, node)
			if err != nil {
				unsettledErr = err
				return false
			}

			// Check that UnsettledBalance is what we expect.
			if int(chanInfo.UnsettledBalance) != expectedBalance {
				unsettledErr = fmt.Errorf("unsettled balance failed "+
					"expected: %v, received: %v", expectedBalance,
					chanInfo.UnsettledBalance)
				return false
			}
		}

		return true
	}, defaultTimeout)
	if err != nil {
		t.Fatalf("unsettled balace error: %v", unsettledErr)
	}

	// Check for payment errors.
	select {
	case err := <-errChan:
		t.Fatalf("payment error: %v", err)
	default:
	}

	// Check alice's channel balance, which should have a remote unsettled
	// balance that equals to the amount of invoices * payAmt. The remote
	// balance remains zero.
	aliceLocal := chanAmt - cType.calcStaticFee(0) - numInvoices*payAmt
	checkChannelBalance(net.Alice, aliceLocal, 0, 0, numInvoices*payAmt)

	// Check carol's channel balance, which should have a local unsettled
	// balance that equals to the amount of invoices * payAmt. The local
	// balance remains zero.
	checkChannelBalance(carol, 0, aliceLocal, numInvoices*payAmt, 0)

	// Force and assert the channel closure.
	closeChannelAndAssert(t, net, net.Alice, chanPointAlice, true)

	// Cleanup by mining the force close and sweep transaction.
	cleanupForceClose(t, net, net.Alice, chanPointAlice)
}
