package main

import (
	"bytes"
	"fmt"
	"golang.org/x/net/context"
	"sync"
	"time"
	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/roasbeef/btcd/rpctest"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcrpcclient"
	"github.com/roasbeef/btcutil"
	"testing"
)

// CT is needed for:
// - have uniform way of handling panic and fatal error from test cases.
// - have ability to properly wrap errors in order to see stack trace.
// - have nice and elegant way to handle lnd process errors in one
// select structure with test cases.
type CT struct {
	*testing.T

	// Channel for sending retransmitted panic errors and fatal error which
	// happens in test case.
	errChan chan error
}

func NewCT(t *testing.T) *CT {
	return &CT{t, nil}
}

func (ct *CT) Error(err error) {
	if ct.errChan != nil {
		ct.errChan <- fmt.Errorf(errors.Wrap(err, 1).ErrorStack())
		ct.FailNow()
	} else {
		ct.Fatal("can't sen error when test isn't running")
	}
}

// Errorf create and send the description about the error in the error channel
// and exit.
func (ct *CT) Errorf(format string, a ...interface{}) {
	if ct.errChan != nil {
		description := fmt.Sprintf(format, a...)
		ct.errChan <- fmt.Errorf(errors.Wrap(description, 1).ErrorStack())
		ct.FailNow()
	} else {
		ct.Fatal("can't sen error when test isn't running")
	}

}

// RunTest wraps test case function in goroutine and also redirects the panic
// error from test case into error channel.
func (ct *CT) RunTest(net *networkHarness, test testCase) chan error {
	// a channel to signal that test was exited with error
	ct.errChan = make(chan error)

	go func() {
		defer func() {
			if err := recover(); err != nil {
				// Retransmit test panic into main "process"
				ct.errChan <- fmt.Errorf(err.(string))
			}
			close(ct.errChan)
			ct.errChan = nil
		}()
		test(net, ct)
	}()

	return ct.errChan
}

func assertTxInBlock(ct *CT, block *btcutil.Block, txid *wire.ShaHash) {
	for _, tx := range block.Transactions() {
		if bytes.Equal(txid[:], tx.Sha()[:]) {
			return
		}
	}

	ct.Errorf("funding tx was not included in block")
}

// openChannelAndAssert attempts to open a channel with the specified
// parameters extended from Alice to Bob. Additionally, two items are asserted
// after the channel is considered open: the funding transaction should be
// found within a block, and that Alice can report the status of the new
// channel.
func openChannelAndAssert(ct *CT, net *networkHarness, ctx context.Context,
	alice, bob *lightningNode, amount btcutil.Amount) *lnrpc.ChannelPoint {

	chanOpenUpdate, err := net.OpenChannel(ctx, alice, bob, amount, 1)
	if err != nil {
		ct.Errorf("unable to open channel: %v", err)
	}

	// Mine a block, then wait for Alice's node to notify us that the
	// channel has been opened. The funding transaction should be found
	// within the newly mined block.
	blockHash, err := net.Miner.Node.Generate(1)
	if err != nil {
		ct.Errorf("unable to generate block: %v", err)
	}
	block, err := net.Miner.Node.GetBlock(blockHash[0])
	if err != nil {
		ct.Errorf("unable to get block: %v", err)
	}
	fundingChanPoint, err := net.WaitForChannelOpen(ctx, chanOpenUpdate)
	if err != nil {
		ct.Errorf("error while waiting for channel open: %v", err)
	}
	fundingTxID, err := wire.NewShaHash(fundingChanPoint.FundingTxid)
	if err != nil {
		ct.Errorf("unable to create sha hash: %v", err)
	}
	assertTxInBlock(ct, block, fundingTxID)

	// The channel should be listed in the peer information returned by
	// both peers.
	chanPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: fundingChanPoint.OutputIndex,
	}
	if err := net.AssertChannelExists(ctx, alice, &chanPoint); err != nil {
		ct.Errorf("unable to assert channel existence: %v", err)
	}

	return fundingChanPoint
}

// closeChannelAndAssert attempts to close a channel identified by the passed
// channel point owned by the passed lighting node. A fully blocking channel
// closure is attempted, therefore the passed context should be a child derived
// via timeout from a base parent. Additionally, once the channel has been
// detected as closed, an assertion checks that the transaction is found within
// a block.
func closeChannelAndAssert(ct *CT, net *networkHarness, ctx context.Context,
	node *lightningNode, fundingChanPoint *lnrpc.ChannelPoint) {

	closeUpdates, err := net.CloseChannel(ctx, node, fundingChanPoint, false)
	if err != nil {
		ct.Errorf("unable to close channel: %v", err)
	}

	// Finally, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block.
	blockHash, err := net.Miner.Node.Generate(1)
	if err != nil {
		ct.Errorf("unable to generate block: %v", err)
	}
	block, err := net.Miner.Node.GetBlock(blockHash[0])
	if err != nil {
		ct.Errorf("unable to get block: %v", err)
	}

	closingTxid, err := net.WaitForChannelClose(ctx, closeUpdates)
	if err != nil {
		ct.Errorf("error while waiting for channel close: %v", err)
	}

	assertTxInBlock(ct, block, closingTxid)
}

// testBasicChannelFunding performs a test exercising expected behavior from a
// basic funding workflow. The test creates a new channel between Alice and
// Bob, then immediately closes the channel after asserting some expected post
// conditions. Finally, the chain itself is checked to ensure the closing
// transaction was mined.
func testBasicChannelFunding(net *networkHarness, ct *CT) {
	timeout := time.Duration(time.Second * 5)
	ctxb := context.Background()

	chanAmt := btcutil.Amount(btcutil.SatoshiPerBitcoin / 2)

	// First establish a channel with a capacity of 0.5 BTC between Alice
	// and Bob. This function will block until the channel itself is fully
	// open or an error occurs in the funding process. A series of
	// assertions will be executed to ensure the funding process completed
	// successfully.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(ct, net, ctxt, net.Alice, net.Bob, chanAmt)

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ct, net, ctxt, net.Alice, chanPoint)
}

// testChannelBalance creates a new channel between Alice and  Bob, then
// checks channel balance to be equal amount specified while creation of channel.
func testChannelBalance(net *networkHarness, ct *CT) {
	timeout := time.Duration(time.Second * 5)

	// Open a channel with 0.5 BTC between Alice and Bob, ensuring the
	// channel has been opened properly.
	amount := btcutil.Amount(btcutil.SatoshiPerBitcoin / 2)
	ctx, _ := context.WithTimeout(context.Background(), timeout)

	// Creates a helper closure to be used below which asserts the proper
	// response to a channel balance RPC.
	checkChannelBalance := func(node lnrpc.LightningClient,
		amount btcutil.Amount) {

		response, err := node.ChannelBalance(ctx, &lnrpc.ChannelBalanceRequest{})
		if err != nil {
			ct.Errorf("unable to get channel balance: %v", err)
		}

		balance := btcutil.Amount(response.Balance)
		if balance != amount {
			ct.Errorf("channel balance wrong: %v != %v", balance,
				amount)
		}
	}

	chanPoint := openChannelAndAssert(ct, net, ctx, net.Alice, net.Bob,
		amount)

	// As this is a single funder channel, Alice's balance should be
	// exactly 0.5 BTC since now state transitions have taken place yet.
	checkChannelBalance(net.Alice, amount)

	// Since we only explicitly wait for Alice's channel open notification,
	// Bob might not yet have updated his internal state in response to
	// Alice's channel open proof. So we sleep here for a second to let Bob
	// catch up.
	// TODO(roasbeef): Bob should also watch for the channel on-chain after
	// the changes to restrict the number of pending channels are in.
	time.Sleep(time.Second)

	// Ensure Bob currently has no available balance within the channel.
	checkChannelBalance(net.Bob, 0)

	// Finally close the channel between Alice and Bob, asserting that the
	// channel has been properly closed on-chain.
	ctx, _ = context.WithTimeout(context.Background(), timeout)
	closeChannelAndAssert(ct, net, ctx, net.Alice, chanPoint)
}

// testChannelForceClosure performs a test to exercise the behavior of "force"
// closing a channel or unilaterally broadcasting the latest local commitment
// state on-chain. The test creates a new channel between Alice and Bob, then
// force closes the channel after some cursory assertions. Within the test, two
// transactions should be broadcast on-chain, the commitment transaction itself
// (which closes the channel), and the sweep transaction a few blocks later
// once the output(s) become mature.
//
// TODO(roabeef): also add an unsettled HTLC before force closing.
func testChannelForceClosure(net *networkHarness, ct *CT) {
	timeout := time.Duration(time.Second * 5)
	ctxb := context.Background()

	// First establish a channel ween with a capacity of 100k satoshis
	// between Alice and Bob.
	numFundingConfs := uint32(1)
	chanAmt := btcutil.Amount(10e4)
	chanOpenUpdate, err := net.OpenChannel(ctxb, net.Alice, net.Bob,
		chanAmt, numFundingConfs)
	if err != nil {
		ct.Errorf("unable to open channel: %v", err)
	}

	if _, err := net.Miner.Node.Generate(numFundingConfs); err != nil {
		ct.Errorf("unable to mine block: %v", err)
	}

	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint, err := net.WaitForChannelOpen(ctxt, chanOpenUpdate)
	if err != nil {
		ct.Errorf("error while waiting for channel to open: %v", err)
	}

	// Now that the channel is open, immediately execute a force closure of
	// the channel. This will also assert that the commitment transaction
	// was immediately broadcast in order to fulfill the force closure
	// request.
	closeUpdate, err := net.CloseChannel(ctxb, net.Alice, chanPoint, true)
	if err != nil {
		ct.Errorf("unable to execute force channel closure: %v", err)
	}

	// Mine a block which should confirm the commitment transaction
	// broadcast as a result of the force closure.
	if _, err := net.Miner.Node.Generate(1); err != nil {
		ct.Errorf("unable to generate block: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closingTxID, err := net.WaitForChannelClose(ctxt, closeUpdate)
	if err != nil {
		ct.Errorf("error while waiting for channel close: %v", err)
	}

	// Currently within the codebase, the default CSV is 4 relative blocks.
	// So generate exactly 4 new blocks.
	// TODO(roasbeef): should check default value in config here instead,
	// or make delay a param
	const defaultCSV = 4
	if _, err := net.Miner.Node.Generate(defaultCSV); err != nil {
		ct.Errorf("unable to mine blocks: %v", err)
	}

	// At this point, the sweeping transaction should now be broadcast. So
	// we fetch the node's mempool to ensure it has been properly
	// broadcast.
	var sweepingTXID *wire.ShaHash
	var mempool []*wire.ShaHash
mempoolPoll:
	for {
		select {
		case <-time.After(time.Second * 5):
			ct.Errorf("sweep tx not found in mempool")
		default:
			mempool, err = net.Miner.Node.GetRawMempool()
			if err != nil {
				ct.Errorf("unable to fetch node's mempool: %v", err)
			}
			if len(mempool) == 0 {
				continue
			}
			break mempoolPoll
		}
	}

	// There should be exactly one transaction within the mempool at this
	// point.
	// TODO(roasbeef): assertion may not necessarily hold with concurrent
	// test executions
	if len(mempool) != 1 {
		ct.Errorf("node's mempool is wrong size, expected 1 got %v",
			len(mempool))
	}
	sweepingTXID = mempool[0]

	// Fetch the sweep transaction, all input it's spending should be from
	// the commitment transaction which was broadcast on-chain.
	sweepTx, err := net.Miner.Node.GetRawTransaction(sweepingTXID)
	if err != nil {
		ct.Errorf("unable to fetch sweep tx: %v", err)
	}
	for _, txIn := range sweepTx.MsgTx().TxIn {
		if !closingTxID.IsEqual(&txIn.PreviousOutPoint.Hash) {
			ct.Errorf("sweep transaction not spending from commit "+
				"tx %v, instead spending %v",
				closingTxID, txIn.PreviousOutPoint)
		}
	}

	// Finally, we mine an additional block which should include the sweep
	// transaction as the input scripts and the sequence locks on the
	// inputs should be properly met.
	blockHash, err := net.Miner.Node.Generate(1)
	if err != nil {
		ct.Errorf("unable to generate block: %v", err)
	}
	block, err := net.Miner.Node.GetBlock(blockHash[0])
	if err != nil {
		ct.Errorf("unable to get block: %v", err)
	}

	assertTxInBlock(ct, block, sweepTx.Sha())
}

func testSingleHopInvoice(net *networkHarness, ct *CT) {
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 5)

	// Open a channel with 100k satoshis between Alice and Bob with Alice being
	// the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanAmt := btcutil.Amount(100000)
	chanPoint := openChannelAndAssert(ct, net, ctxt, net.Alice, net.Bob, chanAmt)

	// Now that the channel is open, create an invoice for Bob which
	// expects a payment of 1000 satoshis from Alice paid via a particular
	// pre-image.
	const paymentAmt = 1000
	preimage := bytes.Repeat([]byte("A"), 32)
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: preimage,
		Value:     paymentAmt,
	}
	invoiceResp, err := net.Bob.AddInvoice(ctxb, invoice)
	if err != nil {
		ct.Errorf("unable to add invoice: %v", err)
	}

	// With the invoice for Bob added, send a payment towards Alice paying
	// to the above generated invoice.
	sendStream, err := net.Alice.SendPayment(ctxb)
	if err != nil {
		ct.Errorf("unable to create alice payment stream: %v", err)
	}
	sendReq := &lnrpc.SendRequest{
		PaymentHash: invoiceResp.RHash,
		Dest:        net.Bob.PubKey[:],
		Amt:         paymentAmt,
	}
	if err := sendStream.Send(sendReq); err != nil {
		ct.Errorf("unable to send payment: %v", err)
	}
	if _, err := sendStream.Recv(); err != nil {
		ct.Errorf("error when attempting recv: %v", err)
	}

	// Bob's invoice should now be found and marked as settled.
	// TODO(roasbeef): remove sleep after hooking into the to-be-written
	// invoice settlement notification stream
	payHash := &lnrpc.PaymentHash{
		RHash: invoiceResp.RHash,
	}
	dbInvoice, err := net.Bob.LookupInvoice(ctxb, payHash)
	if err != nil {
		ct.Errorf("unable to lookup invoice: %v", err)
	}
	if !dbInvoice.Settled {
		ct.Errorf("bob's invoice should be marked as settled: %v",
			spew.Sdump(dbInvoice))
	}

	// The balances of Alice and Bob should be updated accordingly.
	aliceBalance, err := net.Alice.ChannelBalance(ctxb, &lnrpc.ChannelBalanceRequest{})
	if err != nil {
		ct.Errorf("unable to query for alice's balance: %v", err)
	}
	bobBalance, err := net.Bob.ChannelBalance(ctxb, &lnrpc.ChannelBalanceRequest{})
	if err != nil {
		ct.Errorf("unable to query for bob's balance: %v", err)
	}

	if aliceBalance.Balance != int64(chanAmt-paymentAmt) {
		ct.Errorf("Alice's balance is incorrect got %v, expected %v",
			aliceBalance, int64(chanAmt-paymentAmt))
	}
	if bobBalance.Balance != paymentAmt {
		ct.Errorf("Bob's balance is incorrect got %v, expected %v",
			bobBalance, paymentAmt)
	}

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ct, net, ctxt, net.Alice, chanPoint)
}

func testMultiHopPayments(net *networkHarness, ct *CT) {

	const chanAmt = btcutil.Amount(100000)
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 5)

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointAlice := openChannelAndAssert(ct, net, ctxt, net.Alice,
		net.Bob, chanAmt)

	aliceChanTXID, err := wire.NewShaHash(chanPointAlice.FundingTxid)
	if err != nil {
		ct.Errorf("unable to create sha hash: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// Create a new node (Carol), load her with some funds, then establish
	// a connection between Carol and Alice with a channel that has
	// identical capacity to the one created above.
	//
	// The network topology should now look like: Carol -> Alice -> Bob
	carol, err := net.NewNode(nil)
	if err != nil {
		ct.Errorf("unable to create new nodes: %v", err)
	}
	if err := net.ConnectNodes(ctxb, carol, net.Alice); err != nil {
		ct.Errorf("unable to connect carol to alice: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, carol)
	if err != nil {
		ct.Errorf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointCarol := openChannelAndAssert(ct, net, ctxt, carol,
		net.Alice, chanAmt)

	carolChanTXID, err := wire.NewShaHash(chanPointCarol.FundingTxid)
	if err != nil {
		ct.Errorf("unable to create sha hash: %v", err)
	}
	carolFundPoint := wire.OutPoint{
		Hash:  *carolChanTXID,
		Index: chanPointCarol.OutputIndex,
	}

	// Create 5 invoices for Bob, which expect a payment from Carol for 1k
	// satoshis with a different preimage each time.
	const numPayments = 5
	const paymentAmt = 1000
	rHashes := make([][]byte, numPayments)
	for i := 0; i < numPayments; i++ {
		preimage := bytes.Repeat([]byte{byte(i)}, 32)
		invoice := &lnrpc.Invoice{
			Memo:      "testing",
			RPreimage: preimage,
			Value:     paymentAmt,
		}
		resp, err := net.Bob.AddInvoice(ctxb, invoice)
		if err != nil {
			ct.Errorf("unable to add invoice: %v", err)
		}

		rHashes[i] = resp.RHash
	}

	// Carol's routing table should show a path from Carol -> Alice -> Bob,
	// with the two channels above recognized as the only links within the
	// network.
	time.Sleep(time.Second)
	req := &lnrpc.ShowRoutingTableRequest{}
	routingResp, err := carol.ShowRoutingTable(ctxb, req)
	if err != nil {
		ct.Errorf("unable to query for carol's routing table: %v", err)
	}
	if len(routingResp.Channels) != 2 {
		ct.Errorf("only two channels should be seen as active in the "+
			"network, instead %v are", len(routingResp.Channels))
	}
	for _, link := range routingResp.Channels {
		switch {
		case link.Outpoint == aliceFundPoint.String():
			switch {
			case link.Id1 == net.Alice.PubKeyStr &&
				link.Id2 == net.Bob.PubKeyStr:
				continue
			case link.Id1 == net.Bob.PubKeyStr &&
				link.Id2 == net.Alice.PubKeyStr:
				continue
			default:
				ct.Errorf("unkown link within routing "+
					"table: %v", spew.Sdump(link))
			}
		case link.Outpoint == carolFundPoint.String():
			switch {
			case link.Id1 == net.Alice.PubKeyStr &&
				link.Id2 == carol.PubKeyStr:
				continue
			case link.Id1 == carol.PubKeyStr &&
				link.Id2 == net.Alice.PubKeyStr:
				continue
			default:
				ct.Errorf("unkown link within routing "+
					"table: %v", spew.Sdump(link))
			}
		default:
			ct.Errorf("unkown channel %v found in routing table, "+
				"only %v and %v should exist", link.Outpoint,
				aliceFundPoint, carolFundPoint)
		}
	}

	// Using Carol as the source, pay to the 5 invoices from Bob created above.
	carolPayStream, err := carol.SendPayment(ctxb)
	if err != nil {
		ct.Errorf("unable to create payment stream for carol: %v", err)
	}

	// Concurrently pay off all 5 of Bob's invoices. Each of the goroutines
	// will unblock on the recv once the HTLC it sent has been fully
	// settled.
	var wg sync.WaitGroup
	for _, rHash := range rHashes {
		sendReq := &lnrpc.SendRequest{
			PaymentHash: rHash,
			Dest:        net.Bob.PubKey[:],
			Amt:         paymentAmt,
		}

		wg.Add(1)
		go func() {
			if err := carolPayStream.Send(sendReq); err != nil {
				ct.Errorf("unable to send payment: %v", err)
			}
			if _, err := carolPayStream.Recv(); err != nil {
				ct.Errorf("unable to recv pay resp: %v", err)
			}
			wg.Done()
		}()
	}

	finClear := make(chan struct{})
	go func() {
		wg.Wait()
		close(finClear)
	}()

	select {
	case <-time.After(time.Second * 10):
		ct.Errorf("HLTC's not cleared after 10 seconds")
	case <-finClear:
	}

	assertAsymmetricBalance := func(node *lightningNode,
		chanPoint *wire.OutPoint, localBalance,
		remoteBalance int64) {

		listReq := &lnrpc.ListChannelsRequest{}
		resp, err := node.ListChannels(ctxb, listReq)
		if err != nil {
			ct.Errorf("unable to for node's channels: %v", err)
		}
		for _, channel := range resp.Channels {
			if channel.ChannelPoint != chanPoint.String() {
				continue
			}

			if channel.LocalBalance != localBalance ||
				channel.RemoteBalance != remoteBalance {
				ct.Errorf("incorrect balances: %v",
					spew.Sdump(channel))
			}
			return
		}
		ct.Errorf("channel not found")
	}

	// At this point all the channels within our proto network should be
	// shifted by 5k satoshis in the direction of Bob, the sink within the
	// payment flow generated above.
	// TODO(roasbeef): remove sleep after invoice notification hooks are in
	// place
	time.Sleep(time.Second * 3)
	const sourceBal = int64(95000)
	const sinkBal = int64(5000)
	assertAsymmetricBalance(carol, &carolFundPoint, sourceBal, sinkBal)
	assertAsymmetricBalance(net.Alice, &carolFundPoint, sinkBal, sourceBal)
	assertAsymmetricBalance(net.Alice, &aliceFundPoint, sourceBal, sinkBal)
	assertAsymmetricBalance(net.Bob, &aliceFundPoint, sinkBal, sourceBal)

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ct, net, ctxt, net.Alice, chanPointAlice)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ct, net, ctxt, carol, chanPointCarol)
}

func testInvoiceSubscriptions(net *networkHarness, ct *CT) {
	const chanAmt = btcutil.Amount(500000)
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 5)

	// Open a channel with 500k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(ct, net, ctxt, net.Alice, net.Bob,
		chanAmt)

	// Next create a new invoice for Bob requesting 1k satoshis.
	const paymentAmt = 1000
	preimage := bytes.Repeat([]byte{byte(90)}, 32)
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: preimage,
		Value:     paymentAmt,
	}
	invoiceResp, err := net.Bob.AddInvoice(ctxb, invoice)
	if err != nil {
		ct.Errorf("unable to add invoice: %v", err)
	}

	// Create a new invoice subscription client for Bob, the notification
	// should be dispatched shortly below.
	req := &lnrpc.InvoiceSubscription{}
	bobInvoiceSubscription, err := net.Bob.SubscribeInvoices(ctxb, req)
	if err != nil {
		ct.Errorf("unable to subscribe to bob's invoice updates: %v", err)
	}

	updateSent := make(chan struct{})
	go func() {
		invoiceUpdate, err := bobInvoiceSubscription.Recv()
		if err != nil {
			ct.Errorf("unable to recv invoice update: %v", err)
		}

		// The invoice update should exactly match the invoice created
		// above, but should now be settled.
		if !invoiceUpdate.Settled {
			ct.Errorf("invoice not settled but shoudl be")
		}
		if !bytes.Equal(invoiceUpdate.RPreimage, invoice.RPreimage) {
			ct.Errorf("payment preimages don't match: expected %v, got %v",
				invoice.RPreimage, invoiceUpdate.RPreimage)
		}

		close(updateSent)
	}()

	// With the assertion above set up, send a payment from Alice to Bob
	// which should finalize and settle the invoice.
	sendStream, err := net.Alice.SendPayment(ctxb)
	if err != nil {
		ct.Errorf("unable to create alice payment stream: %v", err)
	}
	sendReq := &lnrpc.SendRequest{
		PaymentHash: invoiceResp.RHash,
		Dest:        net.Bob.PubKey[:],
		Amt:         paymentAmt,
	}
	if err := sendStream.Send(sendReq); err != nil {
		ct.Errorf("unable to send payment: %v", err)
	}
	if _, err := sendStream.Recv(); err != nil {
		ct.Errorf("error when attempting recv: %v", err)
	}

	select {
	case <-time.After(time.Second * 5):
		ct.Errorf("update not sent after 5 seconds")
	case <-updateSent: // Fall through on success
	}

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ct, net, ctxt, net.Alice, chanPoint)
}

// testBasicChannelCreation test multiple channel opening and closing.
func testBasicChannelCreation(net *networkHarness, ct *CT) {
	timeout := time.Duration(time.Second * 5)
	ctx, _ := context.WithTimeout(context.Background(), timeout)

	amount := btcutil.Amount(btcutil.SatoshiPerBitcoin)

	num := 2

	// Open the channel between Alice and Bob, asserting that the
	// channel has been properly open on-chain.
	chanPoints := make([]*lnrpc.ChannelPoint, num)
	for i := 0; i < num; i++ {
		chanPoints[i] = openChannelAndAssert(ct, net, ctx, net.Alice,
			net.Bob, amount)
	}

	// Close the channel between Alice and Bob, asserting that the
	// channel has been properly closed on-chain.
	for _, chanPoint := range chanPoints {
		closeChannelAndAssert(ct, net, ctx, net.Alice, chanPoint)
	}
}

type testCase func(net *networkHarness, t *testing.T)

var testCases = map[string]testCase{
	"basic funding flow":          testBasicChannelFunding,
	"channel force closure":       testChannelForceClosure,
	"channel balance":             testChannelBalance,
	"single hop invoice":          testSingleHopInvoice,
	"multi-hop payments":          testMultiHopPayments,
	"invoice update subscription": testInvoiceSubscriptions,
	"multiple channel creation": 	testBasicChannelCreation,
}

// TestLightningNetworkDaemon performs a series of integration tests amongst a
// programmatically driven network of lnd nodes.
func TestLightningNetworkDaemon(t *testing.T) {
	ct := NewCT(t)

	// First create the network harness to gain access to its
	// 'OnTxAccepted' call back.
	lndHarness, err := newNetworkHarness()
	if err != nil {
		ct.Fatalf("unable to create lightning network harness: %v", err)
	}
	defer lndHarness.TearDownAll()

	handlers := &btcrpcclient.NotificationHandlers{
		OnTxAccepted: lndHarness.OnTxAccepted,
	}

	// First create an instance of the btcd's rpctest.Harness. This will be
	// used to fund the wallets of the nodes within the test network and to
	// drive blockchain related events within the network.
	btcdHarness, err := rpctest.New(harnessNetParams, handlers, nil)
	if err != nil {
		ct.Fatalf("unable to create mining node: %v", err)
	}
	defer btcdHarness.TearDown()
	if err := btcdHarness.SetUp(true, 50); err != nil {
		ct.Fatalf("unable to set up mining node: %v", err)
	}
	if err := btcdHarness.Node.NotifyNewTransactions(false); err != nil {
		ct.Fatalf("unable to request transaction notifications: %v", err)
	}

	// With the btcd harness created, we can now complete the
	// initialization of the network. args - list of lnd arguments,
	// example: "--debuglevel=debug"
	// TODO(roasbeef): create master balanced channel with all the monies?
	if err := lndHarness.InitializeSeedNodes(btcdHarness, nil); err != nil {
		ct.Fatalf("unable to initialize seed nodes: %v", err)
	}
	if err = lndHarness.SetUp(); err != nil {
		ct.Fatalf("unable to set up test lightning network: %v", err)
	}

	ct.Logf("Running %v integration tests", len(testCases))

	for name, test := range testCases {
		errChan := ct.RunTest(lndHarness, test)

		select {
		// Receive both types of err - panic and fatal from
		// one channel and raise the fatal in main goroutine.
		case err := <-errChan:
			if err != nil {
				ct.Fatalf("Fail: (%v): exited with error: \n%v",
					name, err)
			}
			ct.Logf("Successed: (%v)", name)

		// In this case lightning node process finished with error
		// status. It might be because of wrong flag, or it might
		// be because of nil pointer access. Who knows!? Who knows...
		// TODO(andrew.shvv) When two nodes are closing simultanisly
		// it leads to panic - 'sending to the closed channel'. Fix it?
		case err := <-lndHarness.lndErrorChan:
			ct.Fatalf("Fail:  (%v): lnd finished with error "+
				"(stderr): \n%v", name, err)
		}
	}
}
