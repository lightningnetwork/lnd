package itest

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"time"

	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// completePaymentRequests sends payments from a lightning node to complete all
// payment requests. If the awaitResponse parameter is true, this function
// does not return until all payments successfully complete without errors.
func completePaymentRequests(client lnrpc.LightningClient,
	routerClient routerrpc.RouterClient, paymentRequests []string,
	awaitResponse bool) error {

	ctxb := context.Background()
	ctx, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// We start by getting the current state of the client's channels. This
	// is needed to ensure the payments actually have been committed before
	// we return.
	req := &lnrpc.ListChannelsRequest{}
	listResp, err := client.ListChannels(ctx, req)
	if err != nil {
		return err
	}

	// send sends a payment and returns an error if it doesn't succeeded.
	send := func(payReq string) error {
		ctxc, cancel := context.WithCancel(ctx)
		defer cancel()

		payStream, err := routerClient.SendPaymentV2(
			ctxc,
			&routerrpc.SendPaymentRequest{
				PaymentRequest: payReq,
				TimeoutSeconds: 60,
				FeeLimitMsat:   noFeeLimitMsat,
			},
		)
		if err != nil {
			return err
		}

		resp, err := getPaymentResult(payStream)
		if err != nil {
			return err
		}
		if resp.Status != lnrpc.Payment_SUCCEEDED {
			return errors.New(resp.FailureReason)
		}

		return nil
	}

	// Launch all payments simultaneously.
	results := make(chan error)
	for _, payReq := range paymentRequests {
		payReqCopy := payReq
		go func() {
			err := send(payReqCopy)
			if awaitResponse {
				results <- err
			}
		}()
	}

	// If awaiting a response, verify that all payments succeeded.
	if awaitResponse {
		for range paymentRequests {
			err := <-results
			if err != nil {
				return err
			}
		}
		return nil
	}

	// We are not waiting for feedback in the form of a response, but we
	// should still wait long enough for the server to receive and handle
	// the send before cancelling the request. We wait for the number of
	// updates to one of our channels has increased before we return.
	err = wait.Predicate(func() bool {
		ctxt, _ := context.WithTimeout(ctx, defaultTimeout)
		newListResp, err := client.ListChannels(ctxt, req)
		if err != nil {
			return false
		}

		// If the number of open channels is now lower than before
		// attempting the payments, it means one of the payments
		// triggered a force closure (for example, due to an incorrect
		// preimage). Return early since it's clear the payment was
		// attempted.
		if len(newListResp.Channels) < len(listResp.Channels) {
			return true
		}

		for _, c1 := range listResp.Channels {
			for _, c2 := range newListResp.Channels {
				if c1.ChannelPoint != c2.ChannelPoint {
					continue
				}

				// If this channel has an increased numbr of
				// updates, we assume the payments are
				// committed, and we can return.
				if c2.NumUpdates > c1.NumUpdates {
					return true
				}
			}
		}

		return false
	}, defaultTimeout)
	if err != nil {
		return err
	}

	return nil
}

// makeFakePayHash creates random pre image hash
func makeFakePayHash(t *harnessTest) []byte {
	randBuf := make([]byte, 32)

	if _, err := rand.Read(randBuf); err != nil {
		t.Fatalf("internal error, cannot generate random string: %v", err)
	}

	return randBuf
}

// createPayReqs is a helper method that will create a slice of payment
// requests for the given node.
func createPayReqs(node *lntest.HarnessNode, paymentAmt btcutil.Amount,
	numInvoices int) ([]string, [][]byte, []*lnrpc.Invoice, error) {

	payReqs := make([]string, numInvoices)
	rHashes := make([][]byte, numInvoices)
	invoices := make([]*lnrpc.Invoice, numInvoices)
	for i := 0; i < numInvoices; i++ {
		preimage := make([]byte, 32)
		_, err := rand.Read(preimage)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("unable to generate "+
				"preimage: %v", err)
		}
		invoice := &lnrpc.Invoice{
			Memo:      "testing",
			RPreimage: preimage,
			Value:     int64(paymentAmt),
		}
		ctxt, _ := context.WithTimeout(
			context.Background(), defaultTimeout,
		)
		resp, err := node.AddInvoice(ctxt, invoice)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("unable to add "+
				"invoice: %v", err)
		}

		// Set the payment address in the invoice so the caller can
		// properly use it.
		invoice.PaymentAddr = resp.PaymentAddr

		payReqs[i] = resp.PaymentRequest
		rHashes[i] = resp.RHash
		invoices[i] = invoice
	}
	return payReqs, rHashes, invoices, nil
}

// getChanInfo is a helper method for getting channel info for a node's sole
// channel.
func getChanInfo(ctx context.Context, node *lntest.HarnessNode) (
	*lnrpc.Channel, error) {

	req := &lnrpc.ListChannelsRequest{}
	channelInfo, err := node.ListChannels(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(channelInfo.Channels) != 1 {
		return nil, fmt.Errorf("node should only have a single "+
			"channel, instead it has %v", len(channelInfo.Channels))
	}

	return channelInfo.Channels[0], nil
}

// commitType is a simple enum used to run though the basic funding flow with
// different commitment formats.
type commitType byte

const (
	// commitTypeLegacy is the old school commitment type.
	commitTypeLegacy commitType = iota

	// commiTypeTweakless is the commitment type where the remote key is
	// static (non-tweaked).
	commitTypeTweakless

	// commitTypeAnchors is the kind of commitment that has extra outputs
	// used for anchoring down to commitment using CPFP.
	commitTypeAnchors
)

// String returns that name of the commitment type.
func (c commitType) String() string {
	switch c {
	case commitTypeLegacy:
		return "legacy"
	case commitTypeTweakless:
		return "tweakless"
	case commitTypeAnchors:
		return "anchors"
	default:
		return "invalid"
	}
}

// Args returns the command line flag to supply to enable this commitment type.
func (c commitType) Args() []string {
	switch c {
	case commitTypeLegacy:
		return []string{"--protocol.legacy.committweak"}
	case commitTypeTweakless:
		return []string{}
	case commitTypeAnchors:
		return []string{"--protocol.anchors"}
	}

	return nil
}

// calcStaticFee calculates appropriate fees for commitment transactions.  This
// function provides a simple way to allow test balance assertions to take fee
// calculations into account.
func (c commitType) calcStaticFee(numHTLCs int) btcutil.Amount {
	const htlcWeight = input.HTLCWeight
	var (
		feePerKw     = chainfee.SatPerKVByte(50000).FeePerKWeight()
		commitWeight = input.CommitWeight
		anchors      = btcutil.Amount(0)
	)

	// The anchor commitment type is slightly heavier, and we must also add
	// the value of the two anchors to the resulting fee the initiator
	// pays. In addition the fee rate is capped at 10 sat/vbyte for anchor
	// channels.
	if c == commitTypeAnchors {
		feePerKw = chainfee.SatPerKVByte(
			lnwallet.DefaultAnchorsCommitMaxFeeRateSatPerVByte * 1000,
		).FeePerKWeight()
		commitWeight = input.AnchorCommitWeight
		anchors = 2 * anchorSize
	}

	return feePerKw.FeeForWeight(int64(commitWeight+htlcWeight*numHTLCs)) +
		anchors
}

// channelCommitType retrieves the active channel commitment type for the given
// chan point.
func channelCommitType(node *lntest.HarnessNode,
	chanPoint *lnrpc.ChannelPoint) (commitType, error) {

	ctxb := context.Background()
	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)

	req := &lnrpc.ListChannelsRequest{}
	channels, err := node.ListChannels(ctxt, req)
	if err != nil {
		return 0, fmt.Errorf("listchannels failed: %v", err)
	}

	for _, c := range channels.Channels {
		if c.ChannelPoint == txStr(chanPoint) {
			switch c.CommitmentType {

			// If the anchor output size is non-zero, we are
			// dealing with the anchor type.
			case lnrpc.CommitmentType_ANCHORS:
				return commitTypeAnchors, nil

			// StaticRemoteKey means it is tweakless,
			case lnrpc.CommitmentType_STATIC_REMOTE_KEY:
				return commitTypeTweakless, nil

			// Otherwise legacy.
			default:
				return commitTypeLegacy, nil
			}
		}
	}

	return 0, fmt.Errorf("channel point %v not found", chanPoint)
}

// calculateMaxHtlc re-implements the RequiredRemoteChannelReserve of the
// funding manager's config, which corresponds to the maximum MaxHTLC value we
// allow users to set when updating a channel policy.
func calculateMaxHtlc(chanCap btcutil.Amount) uint64 {
	reserve := lnwire.NewMSatFromSatoshis(chanCap / 100)
	max := lnwire.NewMSatFromSatoshis(chanCap) - reserve
	return uint64(max)
}

// waitForNodeBlockHeight queries the node for its current block height until
// it reaches the passed height.
func waitForNodeBlockHeight(ctx context.Context, node *lntest.HarnessNode,
	height int32) error {
	var predErr error
	err := wait.Predicate(func() bool {
		ctxt, _ := context.WithTimeout(ctx, defaultTimeout)
		info, err := node.GetInfo(ctxt, &lnrpc.GetInfoRequest{})
		if err != nil {
			predErr = err
			return false
		}

		if int32(info.BlockHeight) != height {
			predErr = fmt.Errorf("expected block height to "+
				"be %v, was %v", height, info.BlockHeight)
			return false
		}
		return true
	}, defaultTimeout)
	if err != nil {
		return predErr
	}
	return nil
}

// getNTxsFromMempool polls until finding the desired number of transactions in
// the provided miner's mempool and returns the full transactions to the caller.
func getNTxsFromMempool(miner *rpcclient.Client, n int,
	timeout time.Duration) ([]*wire.MsgTx, error) {

	txids, err := waitForNTxsInMempool(miner, n, timeout)
	if err != nil {
		return nil, err
	}

	var txes []*wire.MsgTx
	for _, txid := range txids {
		tx, err := miner.GetRawTransaction(txid)
		if err != nil {
			return nil, err
		}
		txes = append(txes, tx.MsgTx())
	}
	return txes, nil
}

// getTxFee retrieves parent transactions and reconstructs the fee paid.
func getTxFee(miner *rpcclient.Client, tx *wire.MsgTx) (btcutil.Amount, error) {
	var balance btcutil.Amount
	for _, in := range tx.TxIn {
		parentHash := in.PreviousOutPoint.Hash
		rawTx, err := miner.GetRawTransaction(&parentHash)
		if err != nil {
			return 0, err
		}
		parent := rawTx.MsgTx()
		balance += btcutil.Amount(
			parent.TxOut[in.PreviousOutPoint.Index].Value,
		)
	}

	for _, out := range tx.TxOut {
		balance -= btcutil.Amount(out.Value)
	}

	return balance, nil
}

// channelSubscription houses the proxied update and error chans for a node's
// channel subscriptions.
type channelSubscription struct {
	updateChan chan *lnrpc.ChannelEventUpdate
	errChan    chan error
	quit       chan struct{}
}

// subscribeChannelNotifications subscribes to channel updates and launches a
// goroutine that forwards these to the returned channel.
func subscribeChannelNotifications(ctxb context.Context, t *harnessTest,
	node *lntest.HarnessNode) channelSubscription {

	// We'll first start by establishing a notification client which will
	// send us notifications upon channels becoming active, inactive or
	// closed.
	req := &lnrpc.ChannelEventSubscription{}
	ctx, cancelFunc := context.WithCancel(ctxb)

	chanUpdateClient, err := node.SubscribeChannelEvents(ctx, req)
	if err != nil {
		t.Fatalf("unable to create channel update client: %v", err)
	}

	// We'll launch a goroutine that will be responsible for proxying all
	// notifications recv'd from the client into the channel below.
	errChan := make(chan error, 1)
	quit := make(chan struct{})
	chanUpdates := make(chan *lnrpc.ChannelEventUpdate, 20)
	go func() {
		defer cancelFunc()
		for {
			select {
			case <-quit:
				return
			default:
				chanUpdate, err := chanUpdateClient.Recv()
				select {
				case <-quit:
					return
				default:
				}

				if err == io.EOF {
					return
				} else if err != nil {
					select {
					case errChan <- err:
					case <-quit:
					}
					return
				}

				select {
				case chanUpdates <- chanUpdate:
				case <-quit:
					return
				}
			}
		}
	}()

	return channelSubscription{
		updateChan: chanUpdates,
		errChan:    errChan,
		quit:       quit,
	}
}

// findTxAtHeight gets all of the transactions that a node's wallet has a record
// of at the target height, and finds and returns the tx with the target txid,
// failing if it is not found.
func findTxAtHeight(ctx context.Context, t *harnessTest, height int32,
	target string, node *lntest.HarnessNode) *lnrpc.Transaction {

	txns, err := node.LightningClient.GetTransactions(
		ctx, &lnrpc.GetTransactionsRequest{
			StartHeight: height,
			EndHeight:   height,
		},
	)
	require.NoError(t.t, err, "could not get transactions")

	for _, tx := range txns.Transactions {
		if tx.TxHash == target {
			return tx
		}
	}

	return nil
}
