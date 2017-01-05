package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"sync/atomic"

	"encoding/hex"
	"reflect"

	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/rpctest"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcrpcclient"
	"github.com/roasbeef/btcutil"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

// harnessTest wraps a regular testing.T providing enhanced error detection
// and propagation. All error will be augmented with a full stack-trace in
// order to aide in debugging. Additionally, any panics caused by active
// test cases will also be handled and represented as fatals.
type harnessTest struct {
	t *testing.T

	// testCase is populated during test execution and represents the
	// current test case.
	testCase *testCase
}

// newHarnessTest creates a new instance of a harnessTest from a regular
// testing.T instance.
func newHarnessTest(t *testing.T) *harnessTest {
	return &harnessTest{t, nil}
}

// Fatalf causes the current active test-case to fail with a fatal error. All
// integration tests should mark test failures soley with this method due to
// the error stack traces it produces.
func (h *harnessTest) Fatalf(format string, a ...interface{}) {
	stacktrace := errors.Wrap(fmt.Sprintf(format, a...), 1).ErrorStack()

	if h.testCase != nil {
		h.t.Fatalf("Failed: (%v): exited with error: \n"+
			"%v", h.testCase.name, stacktrace)
	} else {
		h.t.Fatalf("Error outside of test: %v", stacktrace)
	}
}

// RunTestCase executes a harness test-case. Any errors or panics will be
// represented as fatal.
func (h *harnessTest) RunTestCase(testCase *testCase, net *networkHarness) {
	h.testCase = testCase
	defer func() {
		h.testCase = nil
	}()

	defer func() {
		if err := recover(); err != nil {
			description := errors.Wrap(err, 2).ErrorStack()
			h.t.Fatalf("Failed: (%v) paniced with: \n%v",
				h.testCase.name, description)
		}
	}()

	testCase.test(net, h)
	h.t.Logf("Passed: (%v)", h.testCase.name)
	return
}

func (h *harnessTest) Logf(format string, args ...interface{}) {
	h.t.Logf(format, args...)
}

func (h *harnessTest) Log(args ...interface{}) {
	h.t.Log(args...)
}

func assertTxInBlock(t *harnessTest, block *wire.MsgBlock, txid *chainhash.Hash) {
	for _, tx := range block.Transactions {
		sha := tx.TxHash()
		if bytes.Equal(txid[:], sha[:]) {
			return
		}
	}

	t.Fatalf("funding tx was not included in block")
}

// mineBlocks mine 'num' of blocks and check that blocks are present in
// node blockchain.
func mineBlocks(t *harnessTest, net *networkHarness, num uint32) []*wire.MsgBlock {
	blocks := make([]*wire.MsgBlock, num)

	blockHashes, err := net.Miner.Node.Generate(num)
	if err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	for i, blockHash := range blockHashes {
		block, err := net.Miner.Node.GetBlock(blockHash)
		if err != nil {
			t.Fatalf("unable to get block: %v", err)
		}

		blocks[i] = block
	}

	return blocks
}

// openChannelAndAssert attempts to open a channel with the specified
// parameters extended from Alice to Bob. Additionally, two items are asserted
// after the channel is considered open: the funding transaction should be
// found within a block, and that Alice can report the status of the new
// channel.
func openChannelAndAssert(t *harnessTest, net *networkHarness, ctx context.Context,
	alice, bob *lightningNode, amount btcutil.Amount) *lnrpc.ChannelPoint {

	chanOpenUpdate, err := net.OpenChannel(ctx, alice, bob, amount, 1)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// Mine a block, then wait for Alice's node to notify us that the
	// channel has been opened. The funding transaction should be found
	// within the newly mined block.
	block := mineBlocks(t, net, 1)[0]

	fundingChanPoint, err := net.WaitForChannelOpen(ctx, chanOpenUpdate)
	if err != nil {
		t.Fatalf("error while waiting for channel open: %v", err)
	}
	fundingTxID, err := chainhash.NewHash(fundingChanPoint.FundingTxid)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	assertTxInBlock(t, block, fundingTxID)

	// The channel should be listed in the peer information returned by
	// both peers.
	chanPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: fundingChanPoint.OutputIndex,
	}
	if err := net.AssertChannelExists(ctx, alice, &chanPoint); err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}

	return fundingChanPoint
}

// closeChannelAndAssert attempts to close a channel identified by the passed
// channel point owned by the passed lighting node. A fully blocking channel
// closure is attempted, therefore the passed context should be a child derived
// via timeout from a base parent. Additionally, once the channel has been
// detected as closed, an assertion checks that the transaction is found within
// a block.
func closeChannelAndAssert(t *harnessTest, net *networkHarness, ctx context.Context,
	node *lightningNode, fundingChanPoint *lnrpc.ChannelPoint, force bool) *chainhash.Hash {

	closeUpdates, _, err := net.CloseChannel(ctx, node, fundingChanPoint, force)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	// Finally, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block.
	block := mineBlocks(t, net, 1)[0]

	closingTxid, err := net.WaitForChannelClose(ctx, closeUpdates)
	if err != nil {
		t.Fatalf("error while waiting for channel close: %v", err)
	}

	assertTxInBlock(t, block, closingTxid)

	return closingTxid
}

// testBasicChannelFunding performs a test exercising expected behavior from a
// basic funding workflow. The test creates a new channel between Alice and
// Bob, then immediately closes the channel after asserting some expected post
// conditions. Finally, the chain itself is checked to ensure the closing
// transaction was mined.
func testBasicChannelFunding(net *networkHarness, t *harnessTest) {
	timeout := time.Duration(time.Second * 5)
	ctxb := context.Background()

	chanAmt := btcutil.Amount(btcutil.SatoshiPerBitcoin / 2)

	// First establish a channel with a capacity of 0.5 BTC between Alice
	// and Bob. This function will block until the channel itself is fully
	// open or an error occurs in the funding process. A series of
	// assertions will be executed to ensure the funding process completed
	// successfully.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(t, net, ctxt, net.Alice, net.Bob, chanAmt)

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(t, net, ctxt, net.Alice, chanPoint, false)
}

// testChannelBalance creates a new channel between Alice and  Bob, then
// checks channel balance to be equal amount specified while creation of channel.
func testChannelBalance(net *networkHarness, t *harnessTest) {
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
			t.Fatalf("unable to get channel balance: %v", err)
		}

		balance := btcutil.Amount(response.Balance)
		if balance != amount {
			t.Fatalf("channel balance wrong: %v != %v", balance,
				amount)
		}
	}

	chanPoint := openChannelAndAssert(t, net, ctx, net.Alice, net.Bob,
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
	closeChannelAndAssert(t, net, ctx, net.Alice, chanPoint, false)
}

// testChannelForceClosure performs a test to exercise the behavior of "force"
// closing a channel or unilaterally broadcasting the latest local commitment
// state on-chain. The test creates a new channel between Alice and Bob, then
// force closes the channel after some cursory assertions. Within the test, two
// transactions should be broadcast on-chain, the commitment transaction itself
// (which closes the channel), and the sweep transaction a few blocks later
// once the output(s) become mature. This test also includes several restarts
// to ensure that the transaction output states are persisted throughout
// the forced closure process.
//
// TODO(roasbeef): also add an unsettled HTLC before force closing.
func testChannelForceClosure(net *networkHarness, t *harnessTest) {
	timeout := time.Duration(time.Second * 10)
	ctxb := context.Background()

	// First establish a channel ween with a capacity of 100k satoshis
	// between Alice and Bob.
	numFundingConfs := uint32(1)
	chanAmt := btcutil.Amount(10e4)
	chanOpenUpdate, err := net.OpenChannel(ctxb, net.Alice, net.Bob,
		chanAmt, numFundingConfs)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	if _, err := net.Miner.Node.Generate(numFundingConfs); err != nil {
		t.Fatalf("unable to mine block: %v", err)
	}

	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint, err := net.WaitForChannelOpen(ctxt, chanOpenUpdate)
	if err != nil {
		t.Fatalf("error while waiting for channel to open: %v", err)
	}

	// Now that the channel is open, immediately execute a force closure of
	// the channel. This will also assert that the commitment transaction
	// was immediately broadcast in order to fulfill the force closure
	// request.
	_, closingTxID, err := net.CloseChannel(ctxb, net.Alice, chanPoint, true)
	if err != nil {
		t.Fatalf("unable to execute force channel closure: %v", err)
	}

	// The several restarts in this test are intended to ensure that when a
	// channel is force-closed, the UTXO nursery has persisted the state of
	// the channel in the closure process and will recover the correct state
	// when the system comes back on line. This restart tests state
	// persistence at the beginning of the process, when the commitment
	// transaction has been broadcast but not yet confirmed in a block.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}
	// Mine a block which should confirm the commitment transaction
	// broadcast as a result of the force closure.
	if _, err := net.Miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// The following sleep provides time for the UTXO nursery to move the
	// output from the preschool to the kindergarten database buckets
	// prior to RestartNode() being triggered. Without this sleep, the
	// database update may fail, causing the UTXO nursery to retry the move
	// operation upon restart. This will change the blockheights from what
	// is expected by the test.
	// TODO(bvu): refactor out this sleep.
	duration := time.Millisecond * 300
	time.Sleep(duration)

	// The following restart is intended to ensure that outputs from the
	// force close commitment transaction have been persisted once the
	// transaction has been confirmed, but before the outputs are spendable
	// (the "kindergarten" bucket.)
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Currently within the codebase, the default CSV is 4 relative blocks.
	// For the persistence test, we generate three blocks, then trigger
	// a restart and then generate the final block that should trigger
	// the creation of the sweep transaction.
	// TODO(roasbeef): should check default value in config here instead,
	// or make delay a param
	const defaultCSV = 4
	if _, err := net.Miner.Node.Generate(defaultCSV - 1); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	// The following restart checks to ensure that outputs in the kindergarten
	// bucket are persisted while waiting for the required number of
	// confirmations to be reported.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	if _, err := net.Miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	// At this point, the sweeping transaction should now be broadcast. So
	// we fetch the node's mempool to ensure it has been properly
	// broadcast.
	var sweepingTXID *chainhash.Hash
	var mempool []*chainhash.Hash
	mempoolTimeout := time.After(3 * time.Second)
	checkMempoolTick := time.Tick(100 * time.Millisecond)
mempoolPoll:
	for {
		select {
		case <-mempoolTimeout:
			t.Fatalf("sweep tx not found in mempool")
		case <-checkMempoolTick:
			mempool, err = net.Miner.Node.GetRawMempool()
			if err != nil {
				t.Fatalf("unable to fetch node's mempool: %v", err)
			}
			if len(mempool) != 0 {
				break mempoolPoll
			}
		}
	}

	// There should be exactly one transaction within the mempool at this
	// point.
	// TODO(roasbeef): assertion may not necessarily hold with concurrent
	// test executions
	if len(mempool) != 1 {
		t.Fatalf("node's mempool is wrong size, expected 1 got %v",
			len(mempool))
	}
	sweepingTXID = mempool[0]

	// Fetch the sweep transaction, all input it's spending should be from
	// the commitment transaction which was broadcast on-chain.
	sweepTx, err := net.Miner.Node.GetRawTransaction(sweepingTXID)
	if err != nil {
		t.Fatalf("unable to fetch sweep tx: %v", err)
	}
	for _, txIn := range sweepTx.MsgTx().TxIn {
		if !closingTxID.IsEqual(&txIn.PreviousOutPoint.Hash) {
			t.Fatalf("sweep transaction not spending from commit "+
				"tx %v, instead spending %v",
				closingTxID, txIn.PreviousOutPoint)
		}
	}

	// Finally, we mine an additional block which should include the sweep
	// transaction as the input scripts and the sequence locks on the
	// inputs should be properly met.
	blockHash, err := net.Miner.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	block, err := net.Miner.Node.GetBlock(blockHash[0])
	if err != nil {
		t.Fatalf("unable to get block: %v", err)
	}

	assertTxInBlock(t, block, sweepTx.Hash())
}

func testSingleHopInvoice(net *networkHarness, t *harnessTest) {
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 5)

	// Open a channel with 100k satoshis between Alice and Bob with Alice being
	// the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanAmt := btcutil.Amount(100000)
	chanPoint := openChannelAndAssert(t, net, ctxt, net.Alice, net.Bob, chanAmt)

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
		t.Fatalf("unable to add invoice: %v", err)
	}

	// With the invoice for Bob added, send a payment towards Alice paying
	// to the above generated invoice.
	sendStream, err := net.Alice.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create alice payment stream: %v", err)
	}
	sendReq := &lnrpc.SendRequest{
		PaymentHash: invoiceResp.RHash,
		Dest:        net.Bob.PubKey[:],
		Amt:         paymentAmt,
	}
	if err := sendStream.Send(sendReq); err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}
	if _, err := sendStream.Recv(); err != nil {
		t.Fatalf("error when attempting recv: %v", err)
	}

	// Bob's invoice should now be found and marked as settled.
	// TODO(roasbeef): remove sleep after hooking into the to-be-written
	// invoice settlement notification stream
	payHash := &lnrpc.PaymentHash{
		RHash: invoiceResp.RHash,
	}
	dbInvoice, err := net.Bob.LookupInvoice(ctxb, payHash)
	if err != nil {
		t.Fatalf("unable to lookup invoice: %v", err)
	}
	if !dbInvoice.Settled {
		t.Fatalf("bob's invoice should be marked as settled: %v",
			spew.Sdump(dbInvoice))
	}

	// The balances of Alice and Bob should be updated accordingly.
	aliceBalance, err := net.Alice.ChannelBalance(ctxb, &lnrpc.ChannelBalanceRequest{})
	if err != nil {
		t.Fatalf("unable to query for alice's balance: %v", err)
	}
	bobBalance, err := net.Bob.ChannelBalance(ctxb, &lnrpc.ChannelBalanceRequest{})
	if err != nil {
		t.Fatalf("unable to query for bob's balance: %v", err)
	}

	if aliceBalance.Balance != int64(chanAmt-paymentAmt) {
		t.Fatalf("Alice's balance is incorrect got %v, expected %v",
			aliceBalance, int64(chanAmt-paymentAmt))
	}
	if bobBalance.Balance != paymentAmt {
		t.Fatalf("Bob's balance is incorrect got %v, expected %v",
			bobBalance, paymentAmt)
	}

	// Both channels should also have properly accunted from the amount
	// that has been sent/received over the channel.
	listReq := &lnrpc.ListChannelsRequest{}
	aliceListChannels, err := net.Alice.ListChannels(ctxb, listReq)
	if err != nil {
		t.Fatalf("unable to query for alice's channel list: %v", err)
	}
	aliceSatoshisSent := aliceListChannels.Channels[0].TotalSatoshisSent
	if aliceSatoshisSent != paymentAmt {
		t.Fatalf("Alice's satoshis sent is incorrect got %v, expected %v",
			aliceSatoshisSent, paymentAmt)
	}

	bobListChannels, err := net.Bob.ListChannels(ctxb, listReq)
	if err != nil {
		t.Fatalf("unable to query for bob's channel list: %v", err)
	}
	bobSatoshisReceived := bobListChannels.Channels[0].TotalSatoshisReceived
	if bobSatoshisReceived != paymentAmt {
		t.Fatalf("Bob's satoshis received is incorrect got %v, expected %v",
			bobSatoshisReceived, paymentAmt)
	}

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(t, net, ctxt, net.Alice, chanPoint, false)
}

func testListPayments(net *networkHarness, t *harnessTest) {
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 5)

	// First start by deleting all payments that Alice knows of. This will
	// allow us to execute the test with a clean state for Alice.
	delPaymentsReq := &lnrpc.DeleteAllPaymentsRequest{}
	if _, err := net.Alice.DeleteAllPayments(ctxb, delPaymentsReq); err != nil {
		t.Fatalf("unable to delete payments: %v", err)
	}

	// Check that there are no payments before test.
	reqInit := &lnrpc.ListPaymentsRequest{}
	paymentsRespInit, err := net.Alice.ListPayments(ctxb, reqInit)
	if err != nil {
		t.Fatalf("error when obtaining Alice payments: %v", err)
	}
	if len(paymentsRespInit.Payments) != 0 {
		t.Fatalf("incorrect number of payments, got %v, want %v",
			len(paymentsRespInit.Payments), 0)
	}

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	chanAmt := btcutil.Amount(100000)
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(t, net, ctxt, net.Alice, net.Bob,
		chanAmt)

	// Now that the channel is open, create an invoice for Bob which
	// expects a payment of 1000 satoshis from Alice paid via a particular
	// pre-image.
	const paymentAmt = 1000
	preimage := bytes.Repeat([]byte("B"), 32)
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: preimage,
		Value:     paymentAmt,
	}
	addInvoiceCtxt, _ := context.WithTimeout(ctxb, timeout)
	invoiceResp, err := net.Bob.AddInvoice(addInvoiceCtxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// With the invoice for Bob added, send a payment towards Alice paying
	// to the above generated invoice.
	sendStream, err := net.Alice.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create alice payment stream: %v", err)
	}
	sendReq := &lnrpc.SendRequest{
		PaymentHash: invoiceResp.RHash,
		Dest:        net.Bob.PubKey[:],
		Amt:         paymentAmt,
	}
	if err := sendStream.Send(sendReq); err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}
	if _, err := sendStream.Recv(); err != nil {
		t.Fatalf("error when attempting recv: %v", err)
	}

	// Grab Alice's list of payments, she should show the existence of
	// exactly one payment.
	req := &lnrpc.ListPaymentsRequest{}
	paymentsResp, err := net.Alice.ListPayments(ctxb, req)
	if err != nil {
		t.Fatalf("error when obtaining Alice payments: %v", err)
	}
	if len(paymentsResp.Payments) != 1 {
		t.Fatalf("incorrect number of payments, got %v, want %v",
			len(paymentsResp.Payments), 1)
	}
	p := paymentsResp.Payments[0]

	// Ensure that the stored path shows a direct payment to Bob with no
	// other nodes in-between.
	expectedPath := []string{
		net.Bob.PubKeyStr,
	}
	if !reflect.DeepEqual(p.Path, expectedPath) {
		t.Fatalf("incorrect path, got %v, want %v",
			p.Path, expectedPath)
	}

	// The payment amount should also match our previous payment directly.
	if p.Value != paymentAmt {
		t.Fatalf("incorrect amount, got %v, want %v",
			p.Value, paymentAmt)
	}

	// The payment hash (or r-hash) should have been stored correctly.
	correctRHash := hex.EncodeToString(invoiceResp.RHash)
	if !reflect.DeepEqual(p.PaymentHash, correctRHash) {
		t.Fatalf("incorrect RHash, got %v, want %v",
			p.PaymentHash, correctRHash)
	}

	// Finally, as we made a single-hop direct payment, there should have
	// been no fee applied.
	if p.Fee != 0 {
		t.Fatalf("incorrect Fee, got %v, want %v", p.Fee, 0)
	}

	// Delete all payments from Alice. DB should have no payments.
	delReq := &lnrpc.DeleteAllPaymentsRequest{}
	_, err = net.Alice.DeleteAllPayments(ctxb, delReq)
	if err != nil {
		t.Fatalf("Can't delete payments at the end: %v", err)
	}

	// Check that there are no payments before test.
	listReq := &lnrpc.ListPaymentsRequest{}
	paymentsResp, err = net.Alice.ListPayments(ctxb, listReq)
	if err != nil {
		t.Fatalf("error when obtaining Alice payments: %v", err)
	}
	if len(paymentsResp.Payments) != 0 {
		t.Fatalf("incorrect number of payments, got %v, want %v",
			len(paymentsRespInit.Payments), 0)
	}

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(t, net, ctxt, net.Alice, chanPoint, false)
}

func testMultiHopPayments(net *networkHarness, t *harnessTest) {
	const chanAmt = btcutil.Amount(100000)
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 5)

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointAlice := openChannelAndAssert(t, net, ctxt, net.Alice,
		net.Bob, chanAmt)

	aliceChanTXID, err := chainhash.NewHash(chanPointAlice.FundingTxid)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
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
		t.Fatalf("unable to create new nodes: %v", err)
	}
	if err := net.ConnectNodes(ctxb, carol, net.Alice); err != nil {
		t.Fatalf("unable to connect carol to alice: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointCarol := openChannelAndAssert(t, net, ctxt, carol,
		net.Alice, chanAmt)

	carolChanTXID, err := chainhash.NewHash(chanPointCarol.FundingTxid)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
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
			t.Fatalf("unable to add invoice: %v", err)
		}

		rHashes[i] = resp.RHash
	}

	// Carol's routing table should show a path from Carol -> Alice -> Bob,
	// with the two channels above recognized as the only links within the
	// network.
	time.Sleep(time.Second)
	req := &lnrpc.ChannelGraphRequest{}
	chanGraph, err := carol.DescribeGraph(ctxb, req)
	if err != nil {
		t.Fatalf("unable to query for carol's routing table: %v", err)
	}
	if len(chanGraph.Edges) != 2 {
		t.Fatalf("only two channels should be seen as active in the "+
			"network, instead %v are", len(chanGraph.Edges))
	}
	for _, link := range chanGraph.Edges {
		switch {
		case link.ChanPoint == aliceFundPoint.String():
			switch {
			case link.Node1Pub == net.Alice.PubKeyStr &&
				link.Node2Pub == net.Bob.PubKeyStr:
				continue
			case link.Node1Pub == net.Bob.PubKeyStr &&
				link.Node2Pub == net.Alice.PubKeyStr:
				continue
			default:
				t.Fatalf("unknown link within routing "+
					"table: %v", spew.Sdump(link))
			}
		case link.ChanPoint == carolFundPoint.String():
			switch {
			case link.Node1Pub == net.Alice.PubKeyStr &&
				link.Node2Pub == carol.PubKeyStr:
				continue
			case link.Node1Pub == carol.PubKeyStr &&
				link.Node2Pub == net.Alice.PubKeyStr:
				continue
			default:
				t.Fatalf("unknown link within routing "+
					"table: %v", spew.Sdump(link))
			}
		default:
			t.Fatalf("unknown channel %v found in routing table, "+
				"only %v and %v should exist", spew.Sdump(link),
				aliceFundPoint, carolFundPoint)
		}
	}

	// Using Carol as the source, pay to the 5 invoices from Bob created above.
	carolPayStream, err := carol.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create payment stream for carol: %v", err)
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
				t.Fatalf("unable to send payment: %v", err)
			}
			if _, err := carolPayStream.Recv(); err != nil {
				t.Fatalf("unable to recv pay resp: %v", err)
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
		t.Fatalf("HTLC's not cleared after 10 seconds")
	case <-finClear:
	}

	assertAsymmetricBalance := func(node *lightningNode,
		chanPoint wire.OutPoint, localBalance,
		remoteBalance int64) {

		channelName := ""
		switch chanPoint {
		case carolFundPoint:
			channelName = "Carol(local) => Alice(remote)"
		case aliceFundPoint:
			channelName = "Alice(local) => Bob(remote)"
		}

		checkBalance := func() error {
			listReq := &lnrpc.ListChannelsRequest{}
			resp, err := node.ListChannels(ctxb, listReq)
			if err != nil {
				return fmt.Errorf("unable to for node's "+
					"channels: %v", err)
			}
			for _, channel := range resp.Channels {
				if channel.ChannelPoint != chanPoint.String() {
					continue
				}

				if channel.LocalBalance != localBalance {
					return fmt.Errorf("%v: incorrect local "+
						"balances: %v != %v", channelName,
						channel.LocalBalance, localBalance)
				}

				if channel.RemoteBalance != remoteBalance {
					return fmt.Errorf("%v: incorrect remote "+
						"balances: %v != %v", channelName,
						channel.RemoteBalance, remoteBalance)
				}

				return nil
			}
			return fmt.Errorf("channel not found")
		}

		// As far as HTLC inclusion in commitment transaction might be
		// postponed we will try to check the balance couple of
		// times, and then if after some period of time we receive wrong
		// balance return the error.
		// TODO(roasbeef): remove sleep after invoice notification hooks
		// are in place
		var timeover uint32
		go func() {
			<-time.After(time.Second * 20)
			atomic.StoreUint32(&timeover, 1)
		}()

		for {
			isTimeover := atomic.LoadUint32(&timeover) == 1
			if err := checkBalance(); err != nil {
				if isTimeover {
					t.Fatalf("Check balance failed: %v", err)
				}
			} else {
				break
			}
		}
	}

	// At this point all the channels within our proto network should be
	// shifted by 5k satoshis in the direction of Bob, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Carol->Alice->Bob, order is Bob,Alice,Carol.
	const sourceBal = int64(95000)
	const sinkBal = int64(5000)
	assertAsymmetricBalance(net.Bob, aliceFundPoint, sinkBal, sourceBal)
	assertAsymmetricBalance(net.Alice, aliceFundPoint, sourceBal, sinkBal)
	assertAsymmetricBalance(net.Alice, carolFundPoint, sinkBal, sourceBal)
	assertAsymmetricBalance(carol, carolFundPoint, sourceBal, sinkBal)

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(t, net, ctxt, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(t, net, ctxt, carol, chanPointCarol, false)
}

func testInvoiceSubscriptions(net *networkHarness, t *harnessTest) {
	const chanAmt = btcutil.Amount(500000)
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 5)

	// Open a channel with 500k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(t, net, ctxt, net.Alice, net.Bob,
		chanAmt)

	// Next create a new invoice for Bob requesting 1k satoshis.
	// TODO(roasbeef): make global list of invoices for each node to re-use
	// and avoid collisions
	const paymentAmt = 1000
	preimage := bytes.Repeat([]byte{byte(90)}, 32)
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: preimage,
		Value:     paymentAmt,
	}
	invoiceResp, err := net.Bob.AddInvoice(ctxb, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Create a new invoice subscription client for Bob, the notification
	// should be dispatched shortly below.
	req := &lnrpc.InvoiceSubscription{}
	bobInvoiceSubscription, err := net.Bob.SubscribeInvoices(ctxb, req)
	if err != nil {
		t.Fatalf("unable to subscribe to bob's invoice updates: %v", err)
	}

	updateSent := make(chan struct{})
	go func() {
		invoiceUpdate, err := bobInvoiceSubscription.Recv()
		if err != nil {
			t.Fatalf("unable to recv invoice update: %v", err)
		}

		// The invoice update should exactly match the invoice created
		// above, but should now be settled.
		if !invoiceUpdate.Settled {
			t.Fatalf("invoice not settled but shoudl be")
		}
		if !bytes.Equal(invoiceUpdate.RPreimage, invoice.RPreimage) {
			t.Fatalf("payment preimages don't match: expected %v, got %v",
				invoice.RPreimage, invoiceUpdate.RPreimage)
		}

		close(updateSent)
	}()

	// With the assertion above set up, send a payment from Alice to Bob
	// which should finalize and settle the invoice.
	sendStream, err := net.Alice.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create alice payment stream: %v", err)
	}
	sendReq := &lnrpc.SendRequest{
		PaymentHash: invoiceResp.RHash,
		Dest:        net.Bob.PubKey[:],
		Amt:         paymentAmt,
	}
	if err := sendStream.Send(sendReq); err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}
	if _, err := sendStream.Recv(); err != nil {
		t.Fatalf("error when attempting recv: %v", err)
	}

	select {
	case <-time.After(time.Second * 5):
		t.Fatalf("update not sent after 5 seconds")
	case <-updateSent: // Fall through on success
	}

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(t, net, ctxt, net.Alice, chanPoint, false)
}

// testBasicChannelCreation test multiple channel opening and closing.
func testBasicChannelCreation(net *networkHarness, t *harnessTest) {
	const (
		numChannels = 2
		timeout     = time.Duration(time.Second * 5)
		amount      = btcutil.Amount(btcutil.SatoshiPerBitcoin)
	)

	// Open the channel between Alice and Bob, asserting that the
	// channel has been properly open on-chain.
	chanPoints := make([]*lnrpc.ChannelPoint, numChannels)
	for i := 0; i < numChannels; i++ {
		ctx, _ := context.WithTimeout(context.Background(), timeout)
		chanPoints[i] = openChannelAndAssert(t, net, ctx, net.Alice,
			net.Bob, amount)
	}

	// Close the channel between Alice and Bob, asserting that the
	// channel has been properly closed on-chain.
	for _, chanPoint := range chanPoints {
		ctx, _ := context.WithTimeout(context.Background(), timeout)
		closeChannelAndAssert(t, net, ctx, net.Alice, chanPoint, false)
	}
}

// testMaxPendingChannels checks that error is returned from remote peer if
// max pending channel number was exceeded and that '--maxpendingchannels' flag
// exists and works properly.
func testMaxPendingChannels(net *networkHarness, t *harnessTest) {
	maxPendingChannels := defaultMaxPendingChannels + 1
	amount := btcutil.Amount(btcutil.SatoshiPerBitcoin)

	timeout := time.Duration(time.Second * 10)
	ctx, _ := context.WithTimeout(context.Background(), timeout)

	// Create a new node (Carol) with greater number of max pending
	// channels.
	args := []string{
		fmt.Sprintf("--maxpendingchannels=%v", maxPendingChannels),
	}
	carol, err := net.NewNode(args)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}

	ctx, _ = context.WithTimeout(context.Background(), timeout)
	if err := net.ConnectNodes(ctx, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect carol to alice: %v", err)
	}

	ctx, _ = context.WithTimeout(context.Background(), timeout)
	carolBalance := btcutil.Amount(maxPendingChannels) * amount
	if err := net.SendCoins(ctx, carolBalance, carol); err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}

	// Send open channel requests without generating new blocks thereby
	// increasing pool of pending channels. Then check that we can't
	// open the channel if the number of pending channels exceed
	// max value.
	openStreams := make([]lnrpc.Lightning_OpenChannelClient, maxPendingChannels)
	for i := 0; i < maxPendingChannels; i++ {
		ctx, _ = context.WithTimeout(context.Background(), timeout)
		stream, err := net.OpenChannel(ctx, net.Alice, carol, amount, 1)
		if err != nil {
			t.Fatalf("unable to open channel: %v", err)
		}
		openStreams[i] = stream
	}

	// Carol exhausted available amount of pending channels, next open
	// channel request should cause ErrorGeneric to be sent back to Alice.
	ctx, _ = context.WithTimeout(context.Background(), timeout)
	_, err = net.OpenChannel(ctx, net.Alice, carol, amount, 1)
	if err == nil {
		t.Fatalf("error wasn't received")
	} else if grpc.Code(err) != lnwire.ErrorMaxPendingChannels.ToGrpcCode() {
		t.Fatalf("not expected error was received: %v", err)
	}

	// For now our channels are in pending state, in order to not
	// interfere with other tests we should clean up - complete opening
	// of the channel and then close it.

	// Mine a block, then wait for node's to notify us that the channel
	// has been opened. The funding transactions should be found within the
	// newly mined block.
	block := mineBlocks(t, net, 1)[0]

	chanPoints := make([]*lnrpc.ChannelPoint, maxPendingChannels)
	for i, stream := range openStreams {
		ctxt, _ := context.WithTimeout(context.Background(), timeout)
		fundingChanPoint, err := net.WaitForChannelOpen(ctxt, stream)
		if err != nil {
			t.Fatalf("error while waiting for channel open: %v", err)
		}

		fundingTxID, err := chainhash.NewHash(fundingChanPoint.FundingTxid)
		if err != nil {
			t.Fatalf("unable to create sha hash: %v", err)
		}

		assertTxInBlock(t, block, fundingTxID)

		// The channel should be listed in the peer information
		// returned by both peers.
		chanPoint := wire.OutPoint{
			Hash:  *fundingTxID,
			Index: fundingChanPoint.OutputIndex,
		}
		time.Sleep(time.Millisecond * 500)
		if err := net.AssertChannelExists(ctx, net.Alice, &chanPoint); err != nil {
			t.Fatalf("unable to assert channel existence: %v", err)
		}

		chanPoints[i] = fundingChanPoint
	}

	// Finally close the channel between Alice and Carol, asserting that
	// the channel has been properly closed on-chain.
	for _, chanPoint := range chanPoints {
		ctxt, _ := context.WithTimeout(context.Background(), timeout)
		closeChannelAndAssert(t, net, ctxt, net.Alice, chanPoint, false)
	}
}

func copyFile(dest, src string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()

	d, err := os.Create(dest)
	if err != nil {
		return err
	}

	if _, err := io.Copy(d, s); err != nil {
		d.Close()
		return err
	}

	return d.Close()

}

func testRevokedCloseRetribution(net *networkHarness, t *harnessTest) {
	ctxb := context.Background()
	const (
		timeout     = time.Duration(time.Second * 5)
		chanAmt     = btcutil.Amount(btcutil.SatoshiPerBitcoin / 2)
		paymentAmt  = 10000
		numInvoices = 6
	)

	// In order to test Alice's response to an uncooperative channel
	// closure by Bob, we'll first open up a channel between them with a
	// 0.5 BTC value.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(t, net, ctxt, net.Alice, net.Bob, chanAmt)

	// With the channel open, we'll create a few invoices for Bob that
	// Alice will pay to in order to advance the state of the channel.
	bobPaymentHashes := make([][]byte, numInvoices)
	for i := 0; i < numInvoices; i++ {
		preimage := bytes.Repeat([]byte{byte(255 - i)}, 32)
		invoice := &lnrpc.Invoice{
			Memo:      "testing",
			RPreimage: preimage,
			Value:     paymentAmt,
		}
		resp, err := net.Bob.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		bobPaymentHashes[i] = resp.RHash
	}

	// As we'll be querying the state of bob's channels frequently we'll
	// create a closure helper function for the purpose.
	getBobChanInfo := func() (*lnrpc.ActiveChannel, error) {
		req := &lnrpc.ListChannelsRequest{}
		bobChannelInfo, err := net.Bob.ListChannels(ctxb, req)
		if err != nil {
			return nil, err
		}
		if len(bobChannelInfo.Channels) != 1 {
			t.Fatalf("bob should only have a single channel, instead he has %v",
				len(bobChannelInfo.Channels))
		}

		return bobChannelInfo.Channels[0], nil
	}

	// Open up a payment stream to Alice that we'll use to send payment to
	// Bob. We also create a small helper function to send payments to Bob,
	// consuming the payment hashes we generated above.
	alicePayStream, err := net.Alice.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}
	sendPayments := func(start, stop int) error {
		for i := start; i < stop; i++ {
			sendReq := &lnrpc.SendRequest{
				PaymentHash: bobPaymentHashes[i],
				Dest:        net.Bob.PubKey[:],
				Amt:         paymentAmt,
			}
			if err := alicePayStream.Send(sendReq); err != nil {
				return err
			}
			if _, err := alicePayStream.Recv(); err != nil {
				return err
			}
		}
		return nil
	}

	// Send payments from Alice to Bob using 3 of Bob's payment hashes
	// generated above.
	if err := sendPayments(0, numInvoices/2); err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// Next query for Bob's channel state, as we sent 3 payments of 10k
	// satoshis each, Bob should now see his balance as being 30k satoshis.
	bobChan, err := getBobChanInfo()
	if err != nil {
		t.Fatalf("unable to get bob's channel info: %v", err)
	}
	if bobChan.LocalBalance != 30000 {
		t.Fatalf("bob's balance is incorrect, got %v, expected %v",
			bobChan.LocalBalance, 30000)
	}

	// Grab Bob's current commitment height (update number), we'll later
	// revert him to this state after additional updates to force him to
	// broadcast this soon to be revoked state.
	bobStateNumPreCopy := bobChan.NumUpdates

	// Create a temporary file to house Bob's database state at this
	// particular point in history.
	bobTempDbPath, err := ioutil.TempDir("", "bob-past-state")
	if err != nil {
		t.Fatalf("unable to create temp db folder: %v", err)
	}
	bobTempDbFile := filepath.Join(bobTempDbPath, "channel.db")
	defer os.Remove(bobTempDbPath)

	// With the temporary file created, copy Bob's current state into the
	// temporary file we created above. Later after more updates, we'll
	// restore this state.
	bobDbPath := filepath.Join(net.Bob.cfg.DataDir, "simnet/channel.db")
	if err := copyFile(bobTempDbFile, bobDbPath); err != nil {
		t.Fatalf("unable to copy database files: %v", err)
	}

	// Finally, send payments from Alice to Bob, consuming Bob's remaining
	// payment hashes.
	if err := sendPayments(numInvoices/2, numInvoices); err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	bobChan, err = getBobChanInfo()
	if err != nil {
		t.Fatalf("unable to get bob chan info: %v", err)
	}

	// Now we shutdown Bob, copying over the his temporary database state
	// which has the *prior* channel state over his current most up to date
	// state. With this, we essentially force Bob to travel back in time
	// within the channel's history.
	if err = net.RestartNode(net.Bob, func() error {
		return os.Rename(bobTempDbFile, bobDbPath)
	}); err != nil {
		t.Fatalf("unable to restart node: %v", err)
	}

	// Now query for Bob's channel state, it should show that he's at a
	// state number in the past, not the *latest* state.
	bobChan, err = getBobChanInfo()
	if err != nil {
		t.Fatalf("unable to get bob chan info: %v", err)
	}
	if bobChan.NumUpdates != bobStateNumPreCopy {
		t.Fatalf("db copy failed: %v", bobChan.NumUpdates)
	}

	// Now force Bob to execute a *force* channel closure by unilaterally
	// broadcasting his current channel state. This is actually the
	// commitment transaction of a prior *revoked* state, so he'll soon
	// feel the wrath of Alice's retribution.
	breachTXID := closeChannelAndAssert(t, net, ctxb, net.Bob, chanPoint,
		true)

	// Query the mempool for Alice's justice transaction, this should be
	// broadcast as Bob's contract breaching transaction gets confirmed
	// above.
	var justiceTXID *chainhash.Hash
	breakTimeout := time.After(time.Second * 5)
poll:
	for {
		select {
		case <-breakTimeout:
			t.Fatalf("justice tx not found in mempool")
		default:
		}

		mempool, err := net.Miner.Node.GetRawMempool()
		if err != nil {
			t.Fatalf("unable to get mempool: %v", err)
		}

		if len(mempool) == 0 {
			continue
		}

		justiceTXID = mempool[0]
		break poll
	}

	// Query for the mempool transaction found above. Then assert that all
	// the inputs of this transaction are spending outputs generated by
	// Bob's breach transaction above.
	justiceTx, err := net.Miner.Node.GetRawTransaction(justiceTXID)
	if err != nil {
		t.Fatalf("unable to query for justice tx: %v", err)
	}
	for _, txIn := range justiceTx.MsgTx().TxIn {
		if !bytes.Equal(txIn.PreviousOutPoint.Hash[:], breachTXID[:]) {
			t.Fatalf("justice tx not spending commitment utxo "+
				"instead is: %v", txIn.PreviousOutPoint)
		}
	}

	// Now mine a block, this transaction should include Alice's justice
	// transaction which was just accepted into the mempool.
	block := mineBlocks(t, net, 1)[0]

	// The block should have exactly *two* transactions, one of which is
	// the justice transaction.
	if len(block.Transactions) != 2 {
		t.Fatalf("transaction wasn't mined")
	}
	justiceSha := block.Transactions[1].TxHash()
	if !bytes.Equal(justiceTx.Hash()[:], justiceSha[:]) {
		t.Fatalf("justice tx wasn't mined")
	}

	// Finally, obtain Alice's channel state, she shouldn't report any
	// channel as she just successfully brought Bob to justice by sweeping
	// all the channel funds.
	time.Sleep(time.Second * 2)
	req := &lnrpc.ListChannelsRequest{}
	aliceChanInfo, err := net.Alice.ListChannels(ctxb, req)
	if err != nil {
		t.Fatalf("unable to query for alice's channels: %v", err)
	}
	if len(aliceChanInfo.Channels) != 0 {
		t.Fatalf("alice shouldn't have a channel: %v",
			spew.Sdump(aliceChanInfo.Channels))
	}
}

type testCase struct {
	name string
	test func(net *networkHarness, t *harnessTest)
}

var testsCases = []*testCase{
	{
		name: "basic funding flow",
		test: testBasicChannelFunding,
	},
	{
		name: "channel force closure",
		test: testChannelForceClosure,
	},
	{
		name: "channel balance",
		test: testChannelBalance,
	},
	{
		name: "single hop invoice",
		test: testSingleHopInvoice,
	},
	{
		name: "list outgoing payments",
		test: testListPayments,
	},
	{
		name: "max pending channel",
		test: testMaxPendingChannels,
	},
	{
		name: "multi-hop payments",
		test: testMultiHopPayments,
	},
	{
		name: "multiple channel creation",
		test: testBasicChannelCreation,
	},
	{
		name: "invoice update subscription",
		test: testInvoiceSubscriptions,
	},
	{
		// TODO(roasbeef): test always needs to be last as Bob's state
		// is borked since we trick him into attempting to cheat Alice?
		name: "revoked uncooperative close retribution",
		test: testRevokedCloseRetribution,
	},
}

// TestLightningNetworkDaemon performs a series of integration tests amongst a
// programmatically driven network of lnd nodes.
func TestLightningNetworkDaemon(t *testing.T) {
	ht := newHarnessTest(t)

	// First create the network harness to gain access to its
	// 'OnTxAccepted' call back.
	lndHarness, err := newNetworkHarness()
	if err != nil {
		ht.Fatalf("unable to create lightning network harness: %v", err)
	}
	defer lndHarness.TearDownAll()

	handlers := &btcrpcclient.NotificationHandlers{
		OnTxAccepted: lndHarness.OnTxAccepted,
	}

	// Spawn a new goroutine to watch for any fatal errors that any of the
	// running lnd processes encounter. If an error occurs, then the test
	// fails immediately with a fatal error, as far as fatal is happening
	// inside goroutine main goroutine would not be finished at the same
	// time as we receive fatal error from lnd process.
	testsFin := make(chan struct{})
	go func() {
		select {
		case err := <-lndHarness.ProcessErrors():
			ht.Fatalf("lnd finished with error (stderr): "+
				"\n%v", err)
		case <-testsFin:
			return
		}
	}()

	// First create an instance of the btcd's rpctest.Harness. This will be
	// used to fund the wallets of the nodes within the test network and to
	// drive blockchain related events within the network.
	btcdHarness, err := rpctest.New(harnessNetParams, handlers, nil)
	if err != nil {
		ht.Fatalf("unable to create mining node: %v", err)
	}
	defer btcdHarness.TearDown()
	if err := btcdHarness.SetUp(true, 50); err != nil {
		ht.Fatalf("unable to set up mining node: %v", err)
	}
	if err := btcdHarness.Node.NotifyNewTransactions(false); err != nil {
		ht.Fatalf("unable to request transaction notifications: %v", err)
	}

	// Next mine enough blocks in order for segwit and the CSV package
	// soft-fork to activate on SimNet.
	numBlocks := chaincfg.SimNetParams.MinerConfirmationWindow * 2
	if _, err := btcdHarness.Node.Generate(numBlocks); err != nil {
		ht.Fatalf("unable to generate blocks: %v", err)
	}

	// With the btcd harness created, we can now complete the
	// initialization of the network. args - list of lnd arguments,
	// example: "--debuglevel=debug"
	// TODO(roasbeef): create master balanced channel with all the monies?
	if err := lndHarness.InitializeSeedNodes(btcdHarness, nil); err != nil {
		ht.Fatalf("unable to initialize seed nodes: %v", err)
	}
	if err = lndHarness.SetUp(); err != nil {
		ht.Fatalf("unable to set up test lightning network: %v", err)
	}

	t.Logf("Running %v integration tests", len(testsCases))
	for _, testCase := range testsCases {
		ht.RunTestCase(testCase, lndHarness)
	}

	close(testsFin)
}
