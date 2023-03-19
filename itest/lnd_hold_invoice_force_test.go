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
	alice, bob := ht.Alice, ht.Bob
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
	bobInvoice := bob.RPC.AddHoldInvoice(invoiceReq)

	// Subscribe the invoice.
	stream := bob.RPC.SubscribeSingleInvoice(payHash[:])

	// Pay this invoice from Alice -> Bob, we should achieve this with a
	// single htlc.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: bobInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	alice.RPC.SendPayment(req)

	ht.AssertInvoiceState(stream, lnrpc.Invoice_ACCEPTED)

	// Once the HTLC has cleared, alice and bob should both have a single
	// htlc locked in.
	ht.AssertActiveHtlcs(alice, payHash[:])
	ht.AssertActiveHtlcs(bob, payHash[:])

	// Get our htlc expiry height and current block height so that we
	// can mine the exact number of blocks required to expire the htlc.
	channel := ht.QueryChannelByChanPoint(alice, chanPoint)
	require.Len(ht, channel.PendingHtlcs, 1)
	activeHtlc := channel.PendingHtlcs[0]

	_, currentHeight := ht.Miner.GetBestBlock()

	// Now we will mine blocks until the htlc expires, and wait for each
	// node to sync to our latest height. Sanity check that we won't
	// underflow.
	require.Greater(ht, activeHtlc.ExpirationHeight, uint32(currentHeight),
		"expected expiry after current height")
	blocksTillExpiry := activeHtlc.ExpirationHeight - uint32(currentHeight)

	// Alice will go to chain with some delta, sanity check that we won't
	// underflow and subtract this from our mined blocks.
	require.Greater(ht, blocksTillExpiry,
		uint32(lncfg.DefaultOutgoingBroadcastDelta))

	// blocksTillForce is the number of blocks should be mined to
	// trigger a force close from Alice iff the invoice cancelation
	// failed. This value is 48 in current test setup.
	blocksTillForce := blocksTillExpiry -
		lncfg.DefaultOutgoingBroadcastDelta

	// blocksTillCancel is the number of blocks should be mined to trigger
	// an invoice cancelation from Bob. This value is 30 in current test
	// setup.
	blocksTillCancel := blocksTillExpiry -
		lncfg.DefaultHoldInvoiceExpiryDelta

	// When using ht.MineBlocks, for bitcoind backend, the block height
	// synced differ significantly among subsystems. From observation, the
	// LNWL syncs much faster than other subsystems, with more than 10
	// blocks ahead. For this test case, CRTR may be lagging behind for
	// more than 20 blocks. Thus we use slow mining instead.
	// TODO(yy): fix block height asymmetry among all the subsystems.
	//
	// We first mine enough blocks to trigger an invoice cancelation.
	ht.MineBlocks(blocksTillCancel)

	// Wait for the nodes to be synced.
	ht.WaitForBlockchainSync(alice)
	ht.WaitForBlockchainSync(bob)

	// Check that the invoice is canceled by Bob.
	err := wait.NoError(func() error {
		inv := bob.RPC.LookupInvoice(payHash[:])

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

	// We now continue to mine more blocks to the point where it could have
	// triggered a force close if the invoice cancelation was failed.
	//
	// NOTE: we need to mine blocks in two sections because of a following
	// case has happened frequently with bitcoind backend,
	// - when mining all the blocks together, subsystems were syncing
	// blocks under very different speed.
	// - Bob would cancel the invoice in INVC, and send an UpdateFailHTLC
	// in PEER.
	// - Alice, however, would need to receive the message before her
	// subsystem CNCT being synced to the force close height. This didn't
	// happen in bitcoind backend, as Alice's CNCT was syncing way faster
	// than Bob's INVC, causing the channel being force closed before the
	// invoice cancelation message was received by Alice.
	ht.MineBlocks(blocksTillForce - blocksTillCancel)

	// Wait for the nodes to be synced.
	ht.WaitForBlockchainSync(alice)
	ht.WaitForBlockchainSync(bob)

	// Check that Alice has not closed the channel because there are no
	// outgoing HTLCs in her channel as the only HTLC has already been
	// canceled.
	ht.AssertNumPendingForceClose(alice, 0)

	// Clean up the channel.
	ht.CloseChannel(alice, chanPoint)
}
