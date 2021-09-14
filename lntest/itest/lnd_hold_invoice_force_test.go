package itest

import (
	"context"
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

// testHoldInvoiceForceClose tests cancelation of accepted hold invoices which
// would otherwise trigger force closes when they expire.
func testHoldInvoiceForceClose(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Open a channel between alice and bob.
	chanReq := lntest.OpenChannelParams{
		Amt: 300000,
	}

	chanPoint := openChannelAndAssert(
		t, net, net.Alice, net.Bob, chanReq,
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

	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	bobInvoice, err := net.Bob.AddHoldInvoice(ctxt, invoiceReq)
	require.NoError(t.t, err)

	// Pay this invoice from Alice -> Bob, we should achieve this with a
	// single htlc.
	_, err = net.Alice.RouterClient.SendPaymentV2(
		ctxb, &routerrpc.SendPaymentRequest{
			PaymentRequest: bobInvoice.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)
	require.NoError(t.t, err)

	waitForInvoiceAccepted(t, net.Bob, payHash)

	// Once the HTLC has cleared, alice and bob should both have a single
	// htlc locked in.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob}
	err = wait.NoError(func() error {
		return assertActiveHtlcs(nodes, payHash[:])
	}, defaultTimeout)
	require.NoError(t.t, err)

	// Get our htlc expiry height and current block height so that we
	// can mine the exact number of blocks required to expire the htlc.
	chans, err := net.Alice.ListChannels(ctxb, &lnrpc.ListChannelsRequest{})
	require.NoError(t.t, err)
	require.Len(t.t, chans.Channels, 1)
	require.Len(t.t, chans.Channels[0].PendingHtlcs, 1)
	activeHtlc := chans.Channels[0].PendingHtlcs[0]

	require.NoError(t.t, net.Alice.WaitForBlockchainSync())
	require.NoError(t.t, net.Bob.WaitForBlockchainSync())

	info, err := net.Alice.GetInfo(ctxb, &lnrpc.GetInfoRequest{})
	require.NoError(t.t, err)

	// Now we will mine blocks until the htlc expires, and wait for each
	// node to sync to our latest height. Sanity check that we won't
	// underflow.
	require.Greater(
		t.t, activeHtlc.ExpirationHeight, info.BlockHeight,
		"expected expiry after current height",
	)
	blocksTillExpiry := activeHtlc.ExpirationHeight - info.BlockHeight

	// Alice will go to chain with some delta, sanity check that we won't
	// underflow and subtract this from our mined blocks.
	require.Greater(
		t.t, blocksTillExpiry,
		uint32(lncfg.DefaultOutgoingBroadcastDelta),
	)
	blocksTillForce := blocksTillExpiry - lncfg.DefaultOutgoingBroadcastDelta

	mineBlocksSlow(t, net, blocksTillForce, 0)

	require.NoError(t.t, net.Alice.WaitForBlockchainSync())
	require.NoError(t.t, net.Bob.WaitForBlockchainSync())

	// Our channel should not have been force closed, instead we expect our
	// channel to still be open and our invoice to have been canceled before
	// expiry.
	chanInfo, err := getChanInfo(net.Alice)
	require.NoError(t.t, err)

	fundingTxID, err := lnrpc.GetChanPointFundingTxid(chanPoint)
	require.NoError(t.t, err)
	chanStr := fmt.Sprintf("%v:%v", fundingTxID, chanPoint.OutputIndex)
	require.Equal(t.t, chanStr, chanInfo.ChannelPoint)

	err = wait.NoError(func() error {
		inv, err := net.Bob.LookupInvoice(ctxt, &lnrpc.PaymentHash{
			RHash: payHash[:],
		})
		if err != nil {
			return err
		}

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
	require.NoError(t.t, err, "expected canceled invoice")

	// Clean up the channel.
	closeChannelAndAssert(t, net, net.Alice, chanPoint, false)
}
