// +build rpctest

package itest

import (
	"context"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
)

func testMultiHopPayments(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	const chanAmt = btcutil.Amount(100000)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	networkChans = append(networkChans, chanPointAlice)

	aliceChanTXID, err := lnd.GetChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// As preliminary setup, we'll create two new nodes: Carol and Dave,
	// such that we now have a 4 node, 3 channel topology. Dave will make a
	// channel with Alice, and Carol with Dave. After this setup, the
	// network topology should now look like:
	//     Carol -> Dave -> Alice -> Bob
	//
	// First, we'll create Dave and establish a channel to Alice. Dave will
	// be running an older node that requires the legacy onion payload.
	daveArgs := []string{"--protocol.legacyonion"}
	dave, err := net.NewNode("Dave", daveArgs)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, dave, net.Alice); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, btcutil.SatoshiPerBitcoin, dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointDave := openChannelAndAssert(
		ctxt, t, net, dave, net.Alice,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	networkChans = append(networkChans, chanPointDave)
	daveChanTXID, err := lnd.GetChanPointFundingTxid(chanPointDave)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	daveFundPoint := wire.OutPoint{
		Hash:  *daveChanTXID,
		Index: chanPointDave.OutputIndex,
	}

	// Next, we'll create Carol and establish a channel to from her to
	// Dave.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	if err := net.ConnectNodes(ctxt, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = net.SendCoins(ctxt, btcutil.SatoshiPerBitcoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, carol, dave,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	networkChans = append(networkChans, chanPointCarol)

	carolChanTXID, err := lnd.GetChanPointFundingTxid(chanPointCarol)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolFundPoint := wire.OutPoint{
		Hash:  *carolChanTXID,
		Index: chanPointCarol.OutputIndex,
	}

	// Wait for all nodes to have seen all channels.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol, dave}
	nodeNames := []string{"Alice", "Bob", "Carol", "Dave"}
	for _, chanPoint := range networkChans {
		for i, node := range nodes {
			txid, err := lnd.GetChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, point, err)
			}
		}
	}

	// Create 5 invoices for Bob, which expect a payment from Carol for 1k
	// satoshis with a different preimage each time.
	const numPayments = 5
	const paymentAmt = 1000
	payReqs, _, _, err := createPayReqs(
		net.Bob, paymentAmt, numPayments,
	)
	if err != nil {
		t.Fatalf("unable to create pay reqs: %v", err)
	}

	// We'll wait for all parties to recognize the new channels within the
	// network.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPointDave)
	if err != nil {
		t.Fatalf("dave didn't advertise his channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointCarol)
	if err != nil {
		t.Fatalf("carol didn't advertise her channel in time: %v",
			err)
	}

	time.Sleep(time.Millisecond * 50)

	// Set the fee policies of the Alice -> Bob and the Dave -> Alice
	// channel edges to relatively large non default values. This makes it
	// possible to pick up more subtle fee calculation errors.
	maxHtlc := uint64(calculateMaxHtlc(chanAmt))
	updateChannelPolicy(
		t, net.Alice, chanPointAlice, 1000, 100000,
		lnd.DefaultBitcoinTimeLockDelta, maxHtlc, carol,
	)

	updateChannelPolicy(
		t, dave, chanPointDave, 5000, 150000,
		lnd.DefaultBitcoinTimeLockDelta, maxHtlc, carol,
	)

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	err = completePaymentRequests(ctxt, carol, payReqs, true)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// When asserting the amount of satoshis moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.
	const baseFee = 1

	// At this point all the channels within our proto network should be
	// shifted by 5k satoshis in the direction of Bob, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Carol->David->Alice->Bob, order is Bob,
	// Alice, David, Carol.

	// The final node bob expects to get paid five times 1000 sat.
	expectedAmountPaidAtoB := int64(5 * 1000)

	assertAmountPaid(t, "Alice(local) => Bob(remote)", net.Bob,
		aliceFundPoint, int64(0), expectedAmountPaidAtoB)
	assertAmountPaid(t, "Alice(local) => Bob(remote)", net.Alice,
		aliceFundPoint, expectedAmountPaidAtoB, int64(0))

	// To forward a payment of 1000 sat, Alice is charging a fee of
	// 1 sat + 10% = 101 sat.
	const expectedFeeAlice = 5 * 101

	// Dave needs to pay what Alice pays plus Alice's fee.
	expectedAmountPaidDtoA := expectedAmountPaidAtoB + expectedFeeAlice

	assertAmountPaid(t, "Dave(local) => Alice(remote)", net.Alice,
		daveFundPoint, int64(0), expectedAmountPaidDtoA)
	assertAmountPaid(t, "Dave(local) => Alice(remote)", dave,
		daveFundPoint, expectedAmountPaidDtoA, int64(0))

	// To forward a payment of 1101 sat, Dave is charging a fee of
	// 5 sat + 15% = 170.15 sat. This is rounded down in rpcserver to 170.
	const expectedFeeDave = 5 * 170

	// Carol needs to pay what Dave pays plus Dave's fee.
	expectedAmountPaidCtoD := expectedAmountPaidDtoA + expectedFeeDave

	assertAmountPaid(t, "Carol(local) => Dave(remote)", dave,
		carolFundPoint, int64(0), expectedAmountPaidCtoD)
	assertAmountPaid(t, "Carol(local) => Dave(remote)", carol,
		carolFundPoint, expectedAmountPaidCtoD, int64(0))

	// Now that we know all the balances have been settled out properly,
	// we'll ensure that our internal record keeping for completed circuits
	// was properly updated.

	// First, check that the FeeReport response shows the proper fees
	// accrued over each time range. Dave should've earned 170 satoshi for
	// each of the forwarded payments.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	feeReport, err := dave.FeeReport(ctxt, &lnrpc.FeeReportRequest{})
	if err != nil {
		t.Fatalf("unable to query for fee report: %v", err)
	}

	if feeReport.DayFeeSum != uint64(expectedFeeDave) {
		t.Fatalf("fee mismatch: expected %v, got %v", expectedFeeDave,
			feeReport.DayFeeSum)
	}
	if feeReport.WeekFeeSum != uint64(expectedFeeDave) {
		t.Fatalf("fee mismatch: expected %v, got %v", expectedFeeDave,
			feeReport.WeekFeeSum)
	}
	if feeReport.MonthFeeSum != uint64(expectedFeeDave) {
		t.Fatalf("fee mismatch: expected %v, got %v", expectedFeeDave,
			feeReport.MonthFeeSum)
	}

	// Next, ensure that if we issue the vanilla query for the forwarding
	// history, it returns 5 values, and each entry is formatted properly.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	fwdingHistory, err := dave.ForwardingHistory(
		ctxt, &lnrpc.ForwardingHistoryRequest{},
	)
	if err != nil {
		t.Fatalf("unable to query for fee report: %v", err)
	}
	if len(fwdingHistory.ForwardingEvents) != 5 {
		t.Fatalf("wrong number of forwarding event: expected %v, "+
			"got %v", 5, len(fwdingHistory.ForwardingEvents))
	}
	expectedForwardingFee := uint64(expectedFeeDave / numPayments)
	for _, event := range fwdingHistory.ForwardingEvents {
		// Each event should show a fee of 170 satoshi.
		if event.Fee != expectedForwardingFee {
			t.Fatalf("fee mismatch:  expected %v, got %v",
				expectedForwardingFee, event.Fee)
		}
	}

	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, dave, chanPointDave, false)
	ctxt, _ = context.WithTimeout(ctxb, channelCloseTimeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)
}
