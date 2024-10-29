package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// testLookupHtlcResolution checks that `LookupHtlcResolution` returns the
// correct HTLC information.
func testLookupHtlcResolution(ht *lntest.HarnessTest) {
	const chanAmt = btcutil.Amount(1000000)

	alice := ht.NewNodeWithCoins("Alice", nil)
	carol := ht.NewNode("Carol", []string{
		"--store-final-htlc-resolutions",
	})
	ht.EnsureConnected(alice, carol)

	// Open a channel between Alice and Carol.
	cp := ht.OpenChannel(
		alice, carol, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Channel should be ready for payments.
	const payAmt = 100

	// Create an invoice.
	invoice := &lnrpc.Invoice{
		Memo:      "alice to carol htlc lookup",
		RPreimage: ht.Random32Bytes(),
		Value:     payAmt,
	}

	// Carol adds the invoice to her database.
	resp := carol.RPC.AddInvoice(invoice)

	// Subscribe the invoice.
	stream := carol.RPC.SubscribeSingleInvoice(resp.RHash)

	// Alice pays Carol's invoice.
	ht.CompletePaymentRequests(alice, []string{resp.PaymentRequest})

	// Carol waits until the invoice is settled.
	ht.AssertInvoiceState(stream, lnrpc.Invoice_SETTLED)

	// Get the channel using the assert function.
	//
	// TODO(yy): make `ht.OpenChannel` return lnrpc.Channel instead of
	// lnrpc.ChannelPoint.
	channel := ht.AssertChannelExists(carol, cp)

	// Lookup the HTLC and assert the htlc is settled offchain.
	req := &lnrpc.LookupHtlcResolutionRequest{
		ChanId:    channel.ChanId,
		HtlcIndex: 0,
	}

	// Check that Alice will get an error from LookupHtlcResolution.
	err := alice.RPC.LookupHtlcResolutionAssertErr(req)
	gErr := status.Convert(err)
	require.Equal(ht, codes.Unavailable, gErr.Code())

	// Check that Carol can get the final htlc info.
	finalHTLC := carol.RPC.LookupHtlcResolution(req)
	require.True(ht, finalHTLC.Settled, "htlc should be settled")
	require.True(ht, finalHTLC.Offchain, "htlc should be Offchain")
}
