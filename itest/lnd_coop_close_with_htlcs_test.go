package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// testCoopCloseWithHtlcs tests whether we can successfully issue a coop close
// request while there are still active htlcs on the link. Here we will set up
// an HODL invoice to suspend settlement. Then we will attempt to close the
// channel which should appear as a noop for the time being. Then we will have
// the receiver settle the invoice and observe that the channel gets torn down
// after settlement.
func testCoopCloseWithHtlcs(ht *lntest.HarnessTest) {
	alice, bob := ht.Alice, ht.Bob

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
	ht.Miner.AssertTxInMempool(&closeTxid)

	// Wait for it to get mined and finish tearing down.
	ht.AssertStreamChannelCoopClosed(alice, chanPoint, false, closeClient)
}
