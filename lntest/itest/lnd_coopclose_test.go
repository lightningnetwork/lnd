package itest

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// testCoopClose tests that the coop close process adheres to the BOLT#02
// specification.
func testCoopClose(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	chanReq := lntest.OpenChannelParams{
		Amt: 300000,
	}

	chanPoint := openChannelAndAssert(
		t, net, net.Alice, net.Bob, chanReq,
	)

	// Create a hodl invoice for Bob. This will allow us to exercise the
	// Shutdown logic.
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

	// Alice will now pay this invoice and get a single HTLC locked-in on
	// each of the commitment transactions.
	_, err = net.Alice.RouterClient.SendPaymentV2(
		ctxb, &routerrpc.SendPaymentRequest{
			PaymentRequest: bobInvoice.PaymentRequest,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		},
	)
	require.NoError(t.t, err)

	waitForInvoiceAccepted(t, net.Bob, payHash)

	// Alice and Bob should both have a single HTLC locked in.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob}
	err = wait.NoError(func() error {
		return assertActiveHtlcs(nodes, payHash[:])
	}, defaultTimeout)
	require.NoError(t.t, err)

	closeReq := &lnrpc.CloseChannelRequest{
		ChannelPoint: chanPoint,
	}

	// Alice will initiate the coop close and we'll obtain the close
	// channel client to assert that the channel is closed once the htlc is
	// settled.
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	closeRespStream, err := net.Alice.CloseChannel(ctxt, closeReq)
	require.NoError(t.t, err)

	time.Sleep(time.Second * 10)

	// Bob will now settle the invoice.
	settle := &invoicesrpc.SettleInvoiceMsg{
		Preimage: preimage[:],
	}
	_, err = net.Bob.SettleInvoice(ctxt, settle)
	require.NoError(t.t, err)

	// The coop close negotiation should now complete.
	var closeTxid *chainhash.Hash
	err = wait.NoError(func() error {
		closeResp, err := closeRespStream.Recv()
		if err != nil {
			return fmt.Errorf("unable to Recv() from stream: %v",
				err)
		}

		pendingClose, ok := closeResp.Update.(*lnrpc.CloseStatusUpdate_ClosePending)
		if !ok {
			return fmt.Errorf("expected channel close update, "+
				"instead got %v", pendingClose)
		}

		closeTxid, err = chainhash.NewHash(
			pendingClose.ClosePending.Txid,
		)
		if err != nil {
			return fmt.Errorf("unable to decode closeTxid: %v",
				err)
		}

		coopTx, err := waitForTxInMempool(
			net.Miner.Client, minerMempoolTimeout,
		)
		if err != nil {
			return err
		}

		if !coopTx.IsEqual(closeTxid) {
			return fmt.Errorf("mempool tx is not coop close tx")
		}

		return nil
	}, channelCloseTimeout)
	require.NoError(t.t, err)

	// The closing transaction will be mined and Alice and Bob should both
	// see the channel as successfully resolved.
	block := mineBlocks(t, net, 1, 1)[0]
	assertTxInBlock(t, block, closeTxid)

	// Sleep again to ensure the ChannelArbitrator can advance to
	// StateFullyResolved.
	time.Sleep(10 * time.Second)

	aliceReq := &lnrpc.ClosedChannelsRequest{}
	aliceClosedList, err := net.Alice.ClosedChannels(ctxt, aliceReq)
	require.NoError(t.t, err, "alice list closed channels")
	require.Len(t.t, aliceClosedList.Channels, 1, "alice closed channels")

	bobReq := &lnrpc.ClosedChannelsRequest{}
	bobClosedList, err := net.Bob.ClosedChannels(ctxt, bobReq)
	require.NoError(t.t, err, "bob list closed channels")
	require.Len(t.t, bobClosedList.Channels, 1, "bob closed channels")
}
