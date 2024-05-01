package itest

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// testCoopCloseWithHtlcs tests whether we can successfully issue a coop close
// request while there are still active htlcs on the link. In all the tests, we
// will set up an HODL invoice to suspend settlement. Then we will attempt to
// close the channel which should appear as a noop for the time being. Then we
// will have the receiver settle the invoice and observe that the channel gets
// torn down after settlement.
func testCoopCloseWithHtlcs(ht *lntest.HarnessTest) {
	ht.Run("no restart", func(t *testing.T) {
		tt := ht.Subtest(t)
		coopCloseWithHTLCs(tt)
	})

	ht.Run("with restart", func(t *testing.T) {
		tt := ht.Subtest(t)
		coopCloseWithHTLCsWithRestart(tt)
	})
}

// coopCloseWithHTLCs tests the basic coop close scenario which occurs when one
// channel party initiates a channel shutdown while an HTLC is still pending on
// the channel.
func coopCloseWithHTLCs(ht *lntest.HarnessTest) {
	alice, bob := ht.Alice, ht.Bob
	ht.ConnectNodes(alice, bob)

	// Here we set up a channel between Alice and Bob, beginning with a
	// balance on Bob's side.
	chanPoint := ht.OpenChannel(bob, alice, lntest.OpenChannelParams{
		Amt: btcutil.Amount(1000000),
	})

	// Wait for Bob to understand that the channel is ready to use.
	ht.AssertTopologyChannelOpen(bob, chanPoint)

	// Here we set things up so that Alice generates a HODL invoice so we
	// can test whether the shutdown is deferred until the settlement of
	// that invoice.
	payAmt := btcutil.Amount(4)
	var preimage lntypes.Preimage
	copy(preimage[:], ht.Random32Bytes())
	payHash := preimage.Hash()

	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Memo:  "testing close",
		Value: int64(payAmt),
		Hash:  payHash[:],
	}
	resp := alice.RPC.AddHoldInvoice(invoiceReq)
	invoiceStream := alice.RPC.SubscribeSingleInvoice(payHash[:])

	// Here we wait for the invoice to be open and payable.
	ht.AssertInvoiceState(invoiceStream, lnrpc.Invoice_OPEN)

	// Now that the invoice is ready to be paid, let's have Bob open an
	// HTLC for it.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: resp.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitSat:    1000000,
	}
	ht.SendPaymentAndAssertStatus(bob, req, lnrpc.Payment_IN_FLIGHT)
	ht.AssertNumActiveHtlcs(bob, 1)

	// Assert at this point that the HTLC is open but not yet settled.
	ht.AssertInvoiceState(invoiceStream, lnrpc.Invoice_ACCEPTED)

	// Have alice attempt to close the channel.
	closeClient := alice.RPC.CloseChannel(&lnrpc.CloseChannelRequest{
		ChannelPoint: chanPoint,
		NoWait:       true,
		TargetConf:   6,
	})
	ht.AssertChannelInactive(bob, chanPoint)

	// Now that the channel is inactive we can be certain that the deferred
	// closure is set up. Let's settle the invoice.
	alice.RPC.SettleInvoice(preimage[:])

	// Pull the instant update off the wire to clear the path for the
	// close pending update.
	_, err := closeClient.Recv()
	require.NoError(ht, err)

	// Wait for the next channel closure update. Now that we have settled
	// the only HTLC this should be imminent.
	update, err := closeClient.Recv()
	require.NoError(ht, err)

	// This next update should be a GetClosePending as it should be the
	// negotiation of the coop close tx.
	closePending := update.GetClosePending()
	require.NotNil(ht, closePending)

	// Convert the txid we get from the PendingUpdate to a Hash so we can
	// wait for it to be mined.
	var closeTxid chainhash.Hash
	require.NoError(
		ht, closeTxid.SetBytes(closePending.Txid),
		"invalid closing txid",
	)

	// Wait for the close tx to be in the Mempool.
	ht.AssertTxInMempool(&closeTxid)

	// Wait for it to get mined and finish tearing down.
	ht.AssertStreamChannelCoopClosed(alice, chanPoint, false, closeClient)
}

// coopCloseWithHTLCsWithRestart also tests the coop close flow when an HTLC
// is still pending on the channel but this time it ensures that the shutdown
// process continues as expected even if a channel re-establish happens after
// one party has already initiated the shutdown.
func coopCloseWithHTLCsWithRestart(ht *lntest.HarnessTest) {
	alice, bob := ht.Alice, ht.Bob
	ht.ConnectNodes(alice, bob)

	// Open a channel between Alice and Bob with the balance split equally.
	// We do this to ensure that the close transaction will have 2 outputs
	// so that we can assert that the correct delivery address gets used by
	// the channel close initiator.
	chanPoint := ht.OpenChannel(bob, alice, lntest.OpenChannelParams{
		Amt:     btcutil.Amount(1000000),
		PushAmt: btcutil.Amount(1000000 / 2),
	})

	// Wait for Bob to understand that the channel is ready to use.
	ht.AssertTopologyChannelOpen(bob, chanPoint)

	// Set up a HODL invoice so that we can be sure that an HTLC is pending
	// on the channel at the time that shutdown is requested.
	var preimage lntypes.Preimage
	copy(preimage[:], ht.Random32Bytes())
	payHash := preimage.Hash()

	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Memo:  "testing close",
		Value: 400,
		Hash:  payHash[:],
	}
	resp := alice.RPC.AddHoldInvoice(invoiceReq)
	invoiceStream := alice.RPC.SubscribeSingleInvoice(payHash[:])

	// Wait for the invoice to be ready and payable.
	ht.AssertInvoiceState(invoiceStream, lnrpc.Invoice_OPEN)

	// Now that the invoice is ready to be paid, let's have Bob open an HTLC
	// for it.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: resp.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitSat:    1000000,
	}
	ht.SendPaymentAndAssertStatus(bob, req, lnrpc.Payment_IN_FLIGHT)
	ht.AssertNumActiveHtlcs(bob, 1)

	// Assert at this point that the HTLC is open but not yet settled.
	ht.AssertInvoiceState(invoiceStream, lnrpc.Invoice_ACCEPTED)

	// We will now let Alice initiate the closure of the channel. We will
	// also let her specify a specific delivery address to be used since we
	// want to test that this same address is used in the Shutdown message
	// on reconnection.
	newAddr := alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: AddrTypeWitnessPubkeyHash,
	})

	_ = alice.RPC.CloseChannel(&lnrpc.CloseChannelRequest{
		ChannelPoint:    chanPoint,
		NoWait:          true,
		DeliveryAddress: newAddr.Address,
		TargetConf:      6,
	})

	// Assert that both nodes see the channel as waiting for close.
	ht.AssertChannelInactive(bob, chanPoint)
	ht.AssertChannelInactive(alice, chanPoint)

	// Now restart Alice and Bob.
	ht.RestartNode(alice)
	ht.RestartNode(bob)

	ht.AssertConnected(alice, bob)

	// Show that both nodes still see the channel as waiting for close after
	// the restart.
	ht.AssertChannelInactive(bob, chanPoint)
	ht.AssertChannelInactive(alice, chanPoint)

	// Settle the invoice.
	alice.RPC.SettleInvoice(preimage[:])

	// Wait for the channel to appear in the waiting closed list.
	err := wait.Predicate(func() bool {
		pendingChansResp := alice.RPC.PendingChannels()
		waitingClosed := pendingChansResp.WaitingCloseChannels

		return len(waitingClosed) == 1
	}, defaultTimeout)
	require.NoError(ht, err)

	// Wait for the close tx to be in the Mempool and then mine 6 blocks
	// to confirm the close.
	closingTx := ht.AssertClosingTxInMempool(
		chanPoint, lnrpc.CommitmentType_LEGACY,
	)
	ht.MineBlocksAndAssertNumTxes(6, 1)

	// Finally, we inspect the closing transaction here to show that the
	// delivery address that Alice specified in her original close request
	// is the one that ended up being used in the final closing transaction.
	tx := alice.RPC.GetTransaction(&walletrpc.GetTransactionRequest{
		Txid: closingTx.TxHash().String(),
	})
	require.Len(ht, tx.OutputDetails, 2)

	// Find Alice's output in the coop-close transaction.
	var outputDetail *lnrpc.OutputDetail
	for _, output := range tx.OutputDetails {
		if output.IsOurAddress {
			outputDetail = output
			break
		}
	}
	require.NotNil(ht, outputDetail)

	// Show that the address used is the one she requested.
	require.Equal(ht, outputDetail.Address, newAddr.Address)
}
