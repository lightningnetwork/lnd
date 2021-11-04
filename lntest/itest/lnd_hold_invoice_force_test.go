package itest

import (
	"fmt"

	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// testHoldInvoiceForceClose tests cancellation of accepted hold invoices which
// would otherwise trigger force closes when they expire.
func testHoldInvoiceForceClose(ht *lntest.HarnessTest) {
	// Open a channel between alice and bob.
	alice, bob := ht.Alice(), ht.Bob()
	chanPoint := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: 300000},
	)

	// Create a non-dust hold invoice for bob.
	var (
		preimage = lntypes.Preimage{1, 2, 3}
		payHash  = preimage.Hash()
	)
	invoiceReq := &invoicesrpc.AddHoldInvoiceRequest{
		Value:      30000,
		CltvExpiry: 40,
		Hash:       payHash[:],
	}
	bobInvoice := ht.AddHoldInvoice(invoiceReq, bob)

	// Pay this invoice from Alice -> Bob, we should achieve this with a
	// single htlc.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: bobInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	ht.SendPayment(alice, req)

	ht.AssertInvoiceState(bob, payHash, lnrpc.Invoice_ACCEPTED)

	// Once the HTLC has cleared, alice and bob should both have a single
	// htlc locked in.
	ht.AssertActiveHtlcs(alice, payHash[:])
	ht.AssertActiveHtlcs(bob, payHash[:])

	// Get our htlc expiry height and current block height so that we
	// can mine the exact number of blocks required to expire the htlc.
	channel := ht.QueryChannelByChanPoint(alice, chanPoint)
	require.Len(ht, channel.PendingHtlcs, 1)
	activeHtlc := channel.PendingHtlcs[0]

	info := ht.GetInfo(alice)

	// Now we will mine blocks until the htlc expires, and wait for each
	// node to sync to our latest height. Sanity check that we won't
	// underflow.
	require.Greater(ht, activeHtlc.ExpirationHeight, info.BlockHeight,
		"expected expiry after current height")
	blocksTillExpiry := activeHtlc.ExpirationHeight - info.BlockHeight

	// Alice will go to chain with some delta, sanity check that we won't
	// underflow and subtract this from our mined blocks.
	require.Greater(ht, blocksTillExpiry,
		uint32(lncfg.DefaultOutgoingBroadcastDelta))
	blocksTillForce := blocksTillExpiry - lncfg.DefaultOutgoingBroadcastDelta

	ht.MineBlocks(blocksTillForce)

	ht.WaitForBlockchainSync(alice)
	ht.WaitForBlockchainSync(bob)

	// Our channel should not have been force closed, instead we expect our
	// channel to still be open and our invoice to have been canceled before
	// expiry.
	ht.AssertNumPendingCloseChannels(alice, 0, 0)

	err := wait.NoError(func() error {
		inv := ht.LookupInvoice(bob, payHash[:])

		if inv.State != lnrpc.Invoice_CANCELED {
			return fmt.Errorf("expected canceled invoice, got: %v",
				inv.State)
		}

		for _, htlc := range inv.Htlcs {
			if htlc.State != lnrpc.InvoiceHTLCState_CANCELED {
				return fmt.Errorf("expected htlc canceled, "+
					"got: %v", htlc.State)
			}
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err, "expected canceled invoice")

	// Clean up the channel.
	ht.CloseChannel(alice, chanPoint, false)
}
