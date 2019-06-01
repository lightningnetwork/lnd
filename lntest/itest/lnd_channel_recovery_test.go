// +build rpctest

package itest

import (
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"golang.org/x/net/context"
	"time"
)

// testChannelRecovery checks that if a channel funding workflow times out
// the correct recovery process will be executed for the channel initiator.
func testChannelRecovery(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	chanAmt := lnd.MaxBtcFundingAmount
	pushAmt := btcutil.Amount(100000)
	openChannelParameters := lntest.OpenChannelParams{
		Amt:     chanAmt,
		PushAmt: pushAmt,
	}

	srcNode, destNode := net.Alice, net.Bob

	chanOpenUpdate, err := net.OpenChannel(
		ctxb, srcNode, destNode, openChannelParameters,
	)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	srcNodePending, destNodePending := 1, 1
	assertCorrectNumberPendingChannels(t, ctxb, srcNode, destNode,
		srcNodePending, destNodePending, PendingOpen,
	)

	numberOfBlocks := lnd.MaxWaitNumBlocksFundingConf + 50
	mineEmptyBlocks(t, net, numberOfBlocks)

	expectedNumberTransactions := 2

	_, err = waitForNTxsInMempool(
		net.Miner.Node, expectedNumberTransactions, time.Second*60,
	)
	if err != nil {
		t.Fatalf("unable to find all txns in mempool: %v", err)
	}

	minedBlocks := mineBlocks(t, net, 100, expectedNumberTransactions)

	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	fundingChanPoint, err := net.WaitForChannelOpen(ctxt, chanOpenUpdate)

	err = wait.Predicate(func() bool {

		srcNodePending, destNodePending = 0, 0

		pendingChannelRequest := &lnrpc.PendingChannelsRequest{}

		pendingChanResp, err := srcNode.PendingChannels(ctxt,
			pendingChannelRequest,
		)

		pendingState := PendingOpen

		if err != nil {
			return false
		}

		err = checkCorrectPendingChannelResponse(pendingChanResp, srcNodePending,
			pendingState,
		)

		pendingChanResp, err = destNode.PendingChannels(ctxt,
			pendingChannelRequest,
		)

		err = checkCorrectPendingChannelResponse(pendingChanResp, destNodePending,
			pendingState,
		)

		if err != nil {
			return false
		}

		return true
	}, time.Second*60)

	fundingTxID, err := lnd.GetChanPointFundingTxid(fundingChanPoint)
	if err != nil || fundingTxID == nil {
		t.Fatalf("unable to get channel funding tx")
	}

	assertTxInBlock(t, minedBlocks[0], fundingTxID)

	chanPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: fundingChanPoint.OutputIndex,
	}

	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	isNotActive := false
	err = net.AssertChannelExists(ctxt, srcNode, &chanPoint, isNotActive)
	if err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, fundingChanPoint, true)

	// Cleanup by mining the force close and sweep transaction.
	cleanupForceClose(t, net, net.Alice, fundingChanPoint)

}
