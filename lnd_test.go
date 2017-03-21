package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
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
// order to aid in debugging. Additionally, any panics caused by active
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

// Fatalf causes the current active test case to fail with a fatal error. All
// integration tests should mark test failures solely with this method due to
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

// RunTestCase executes a harness test case. Any errors or panics will be
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
func openChannelAndAssert(ctx context.Context, t *harnessTest, net *networkHarness,
	alice, bob *lightningNode, fundingAmt btcutil.Amount,
	pushAmt btcutil.Amount) *lnrpc.ChannelPoint {

	chanOpenUpdate, err := net.OpenChannel(ctx, alice, bob, fundingAmt,
		pushAmt, 1)
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
func closeChannelAndAssert(ctx context.Context, t *harnessTest, net *networkHarness,
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

// numChannelsPending sends an RPC request to a node to get a count of the
// node's channels that are currently in a pending state (with a broadcast,
// but not confirmed funding transaction).
func numChannelsPending(ctxt context.Context, node *lightningNode) (int, error) {
	pendingChansRequest := &lnrpc.PendingChannelRequest{
		Status: lnrpc.ChannelStatus_OPENING,
	}
	resp, err := node.PendingChannels(ctxt, pendingChansRequest)
	if err != nil {
		return 0, err
	}
	return len(resp.PendingChannels), nil
}

// assertNumChannelsPending asserts that a pair of nodes have the expected
// number of pending channels between them.
func assertNumChannelsPending(ctxt context.Context, t *harnessTest,
	alice, bob *lightningNode, expected int) {
	aliceNumChans, err := numChannelsPending(ctxt, alice)
	if err != nil {
		t.Fatalf("error fetching alice's node (%v) pending channels %v",
			alice.nodeID, err)
	}
	bobNumChans, err := numChannelsPending(ctxt, bob)
	if err != nil {
		t.Fatalf("error fetching bob's node (%v) pending channels %v",
			bob.nodeID, err)
	}
	if aliceNumChans != expected {
		t.Fatalf("number of pending channels for alice incorrect. "+
			"expected %v, got %v", expected, aliceNumChans)
	}
	if bobNumChans != expected {
		t.Fatalf("number of pending channels for bob incorrect. "+
			"expected %v, got %v",
			expected, bobNumChans)
	}
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
	pushAmt := btcutil.Amount(100000)

	// First establish a channel with a capacity of 0.5 BTC between Alice
	// and Bob with Alice pushing 100k satoshis to Bob's side during
	// funding. This function will block until the channel itself is fully
	// open or an error occurs in the funding process. A series of
	// assertions will be executed to ensure the funding process completed
	// successfully.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(ctxt, t, net, net.Alice, net.Bob,
		chanAmt, pushAmt)

	ctxt, _ = context.WithTimeout(ctxb, time.Second*15)
	err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}

	// With then channel open, ensure that the amount specified above has
	// properly been pushed to Bob.
	balReq := &lnrpc.ChannelBalanceRequest{}
	aliceBal, err := net.Alice.ChannelBalance(ctxb, balReq)
	if err != nil {
		t.Fatalf("unable to get alice's balance: %v", err)
	}
	bobBal, err := net.Bob.ChannelBalance(ctxb, balReq)
	if err != nil {
		t.Fatalf("unable to get bobs's balance: %v", err)
	}
	if aliceBal.Balance != int64(chanAmt-pushAmt) {
		t.Fatalf("alice's balance is incorrect: expected %v got %v",
			chanAmt-pushAmt, aliceBal)
	}
	if bobBal.Balance != int64(pushAmt) {
		t.Fatalf("bob's balance is incorrect: expected %v got %v",
			pushAmt, bobBal.Balance)
	}

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

// testFundingPersistence is intended to ensure that the Funding Manager
// persists the state of new channels prior to broadcasting the channel's
// funding transaction. This ensures that the daemon maintains an up-to-date
// representation of channels if the system is restarted or disconnected.
// testFundingPersistence mirrors testBasicChannelFunding, but adds restarts
// and checks for the state of channels with unconfirmed funding transactions.
func testChannelFundingPersistence(net *networkHarness, t *harnessTest) {
	ctxb := context.Background()

	chanAmt := btcutil.Amount(btcutil.SatoshiPerBitcoin / 2)
	pushAmt := btcutil.Amount(0)

	timeout := time.Duration(time.Second * 10)
	ctxt, _ := context.WithTimeout(ctxb, timeout)

	// Create a new channel that requires 5 confs before it's considered
	// open, then broadcast the funding transaction
	const numConfs = 5
	pendingUpdate, err := net.OpenPendingChannel(ctxt, net.Alice, net.Bob,
		chanAmt, pushAmt, numConfs)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// At this point, the channel's funding transaction will have
	// been broadcast, but not confirmed. Alice and Bob's nodes
	// should reflect this when queried via RPC.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	assertNumChannelsPending(ctxt, t, net.Alice, net.Bob, 1)

	// Restart both nodes to test that the appropriate state has been
	// persisted and that both nodes recover gracefully.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}
	if err := net.RestartNode(net.Bob, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	fundingTxID, err := chainhash.NewHash(pendingUpdate.Txid)
	if err != nil {
		t.Fatalf("unable to convert funding txid into chainhash.Hash:"+
			" %v", err)
	}

	// Mine a block, then wait for Alice's node to notify us that the
	// channel has been opened. The funding transaction should be found
	// within the newly mined block.
	block := mineBlocks(t, net, 1)[0]
	assertTxInBlock(t, block, fundingTxID)

	// Restart both nodes to test that the appropriate state has been
	// persisted and that both nodes recover gracefully.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}
	if err := net.RestartNode(net.Bob, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// The following block ensures that after both nodes have restarted,
	// they have reconnected before the execution of the next test.
	peersTimeout := time.After(5 * time.Second)
	checkPeersTick := time.NewTicker(100 * time.Millisecond)
	defer checkPeersTick.Stop()
peersPoll:
	for {
		select {
		case <-peersTimeout:
			t.Fatalf("peers unable to reconnect after restart")
		case <-checkPeersTick.C:
			peers, err := net.Bob.ListPeers(ctxb,
				&lnrpc.ListPeersRequest{})
			if err != nil {
				t.Fatalf("ListPeers error: %v\n", err)
			}
			if len(peers.Peers) > 0 {
				break peersPoll
			}
		}
	}

	// Next, mine enough blocks s.t the channel will open with a single
	// additional block mined.
	if _, err := net.Miner.Node.Generate(3); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	// Both nodes should still show a single channel as pending.
	time.Sleep(time.Millisecond * 300)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	assertNumChannelsPending(ctxt, t, net.Alice, net.Bob, 1)

	// Finally, mine the last block which should mark the channel as open.
	if _, err := net.Miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	// At this point, the channel should be fully opened and there should
	// be no pending channels remaining for either node.
	time.Sleep(time.Millisecond * 300)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	assertNumChannelsPending(ctxt, t, net.Alice, net.Bob, 0)

	// The channel should be listed in the peer information returned by
	// both peers.
	outPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: pendingUpdate.OutputIndex,
	}

	// Check both nodes to ensure that the channel is ready for operation.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.AssertChannelExists(ctxt, net.Alice, &outPoint); err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.AssertChannelExists(ctxt, net.Bob, &outPoint); err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: pendingUpdate.Txid,
		OutputIndex: pendingUpdate.OutputIndex,
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, true)
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

	chanPoint := openChannelAndAssert(ctx, t, net, net.Alice, net.Bob,
		amount, 0)

	// Wait for both Alice and Bob to recognize this new channel.
	ctxt, _ := context.WithTimeout(context.Background(), timeout)
	err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
	}
	ctxt, _ = context.WithTimeout(context.Background(), timeout)
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't advertise channel before "+
			"timeout: %v", err)
	}

	// As this is a single funder channel, Alice's balance should be
	// exactly 0.5 BTC since now state transitions have taken place yet.
	checkChannelBalance(net.Alice, amount)

	// Ensure Bob currently has no available balance within the channel.
	checkChannelBalance(net.Bob, 0)

	// Finally close the channel between Alice and Bob, asserting that the
	// channel has been properly closed on-chain.
	ctx, _ = context.WithTimeout(context.Background(), timeout)
	closeChannelAndAssert(ctx, t, net, net.Alice, chanPoint, false)
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

	// Before we start, obtain Bob's current wallet balance, we'll check to
	// ensure that at the end of the force closure by Alice, Bob recognizes
	// his new on-chain output.
	bobBalReq := &lnrpc.WalletBalanceRequest{}
	bobBalResp, err := net.Bob.WalletBalance(ctxb, bobBalReq)
	if err != nil {
		t.Fatalf("unable to get bob's balance: %v", err)
	}
	bobStartingBalance := btcutil.Amount(bobBalResp.Balance * 1e8)

	// First establish a channel with a capacity of 100k satoshis between
	// Alice and Bob. We also push 50k satoshis of the initial amount
	// towards Bob.
	numFundingConfs := uint32(1)
	chanAmt := btcutil.Amount(10e4)
	pushAmt := btcutil.Amount(5e4)
	chanOpenUpdate, err := net.OpenChannel(ctxb, net.Alice, net.Bob,
		chanAmt, pushAmt, numFundingConfs)
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
	checkMempoolTick := time.NewTicker(100 * time.Millisecond)
	defer checkMempoolTick.Stop()
mempoolPoll:
	for {
		select {
		case <-mempoolTimeout:
			t.Fatalf("sweep tx not found in mempool")
		case <-checkMempoolTick.C:
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

	// At this point, Bob should now be aware of his new immediately
	// spendable on-chain balance, as it was Alice who broadcast the
	// commitment transaction.
	bobBalResp, err = net.Bob.WalletBalance(ctxb, bobBalReq)
	if err != nil {
		t.Fatalf("unable to get bob's balance: %v", err)
	}
	bobExpectedBalance := bobStartingBalance + pushAmt
	if btcutil.Amount(bobBalResp.Balance*1e8) < bobExpectedBalance {
		t.Fatalf("bob's balance is incorrect: expected %v got %v",
			bobExpectedBalance, btcutil.Amount(bobBalResp.Balance*1e8))
	}
}

func testSingleHopInvoice(net *networkHarness, t *harnessTest) {
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 5)

	// Open a channel with 100k satoshis between Alice and Bob with Alice being
	// the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanAmt := btcutil.Amount(100000)
	chanPoint := openChannelAndAssert(ctxt, t, net, net.Alice, net.Bob,
		chanAmt, 0)

	assertPaymentBalance := func(amt btcutil.Amount) {
		balReq := &lnrpc.ChannelBalanceRequest{}

		// The balances of Alice and Bob should be updated accordingly.
		aliceBalance, err := net.Alice.ChannelBalance(ctxb, balReq)
		if err != nil {
			t.Fatalf("unable to query for alice's balance: %v", err)
		}
		bobBalance, err := net.Bob.ChannelBalance(ctxb, balReq)
		if err != nil {
			t.Fatalf("unable to query for bob's balance: %v", err)
		}
		if aliceBalance.Balance != int64(chanAmt-amt) {
			t.Fatalf("Alice's balance is incorrect got %v, expected %v",
				aliceBalance, int64(chanAmt-amt))
		}
		if bobBalance.Balance != int64(amt) {
			t.Fatalf("Bob's balance is incorrect got %v, expected %v",
				bobBalance, amt)
		}

		// Both channels should also have properly accunted from the amount
		// that has been sent/received over the channel.
		listReq := &lnrpc.ListChannelsRequest{}
		aliceListChannels, err := net.Alice.ListChannels(ctxb, listReq)
		if err != nil {
			t.Fatalf("unable to query for alice's channel list: %v", err)
		}
		aliceSatoshisSent := aliceListChannels.Channels[0].TotalSatoshisSent
		if aliceSatoshisSent != int64(amt) {
			t.Fatalf("Alice's satoshis sent is incorrect got %v, expected %v",
				aliceSatoshisSent, amt)
		}

		bobListChannels, err := net.Bob.ListChannels(ctxb, listReq)
		if err != nil {
			t.Fatalf("unable to query for bob's channel list: %v", err)
		}
		bobSatoshisReceived := bobListChannels.Channels[0].TotalSatoshisReceived
		if bobSatoshisReceived != int64(amt) {
			t.Fatalf("Bob's satoshis received is incorrect got %v, expected %v",
				bobSatoshisReceived, amt)
		}
	}

	// Now that the channel is open, create an invoice for Bob which
	// expects a payment of 1000 satoshis from Alice paid via a particular
	// preimage.
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

	// Wait for Alice to recognize and advertise the new channel generated
	// above.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
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

	// Ensure we obtain the proper preimage in the response.
	resp, err := sendStream.Recv()
	if err != nil {
		t.Fatalf("error when attempting recv: %v", err)
	}
	if !bytes.Equal(preimage, resp.PaymentPreimage) {
		t.Fatalf("preimage mismatch: expected %v, got %v", preimage,
			resp.PaymentPreimage)
	}

	// Bob's invoice should now be found and marked as settled.
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

	// With the payment completed all balance related stats should be
	// properly updated.
	assertPaymentBalance(paymentAmt)

	// Create another invoice for Bob, this time leaving off the preimage
	// to one will be randomly generated. We'll test the proper
	// encoding/decoding of the zpay32 payment requests.
	invoice = &lnrpc.Invoice{
		Memo:  "test3",
		Value: paymentAmt,
	}
	invoiceResp, err = net.Bob.AddInvoice(ctxb, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Next send another payment, but this time using a zpay32 encoded
	// invoice rather than manually specifying the payment details.
	if err := sendStream.Send(&lnrpc.SendRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
	}); err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}
	if _, err := sendStream.Recv(); err != nil {
		t.Fatalf("error when attempting recv: %v", err)
	}

	// The second payment should also have succeeded, with the balances
	// being update accordingly.
	assertPaymentBalance(paymentAmt * 2)

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
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
	chanPoint := openChannelAndAssert(ctxt, t, net, net.Alice, net.Bob,
		chanAmt, 0)

	// Now that the channel is open, create an invoice for Bob which
	// expects a payment of 1000 satoshis from Alice paid via a particular
	// preimage.
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

	// Wait for Alice to recognize and advertise the new channel generated
	// above.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
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
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

func testMultiHopPayments(net *networkHarness, t *harnessTest) {
	const chanAmt = btcutil.Amount(100000)
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 5)

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointAlice := openChannelAndAssert(ctxt, t, net, net.Alice,
		net.Bob, chanAmt, 0)

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
	chanPointCarol := openChannelAndAssert(ctxt, t, net, carol,
		net.Alice, chanAmt, 0)

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
		invoice := &lnrpc.Invoice{
			Memo:  "testing",
			Value: paymentAmt,
		}
		resp, err := net.Bob.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		rHashes[i] = resp.RHash
	}

	// Wait for carol to recognize both the Channel from herself to Carol,
	// and also the channel from Alice to Bob.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointCarol)
	if err != nil {
		t.Fatalf("carol didn't advertise her channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointAlice)
	if err != nil {
		t.Fatalf("carol didn't see the alice->bob channel before timeout: %v", err)
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

		if err := carolPayStream.Send(sendReq); err != nil {
			t.Fatalf("unable to send payment: %v", err)
		}
		if _, err := carolPayStream.Recv(); err != nil {
			t.Fatalf("unable to recv pay resp: %v", err)
		}
	}

	finClear := make(chan struct{})
	go func() {
		wg.Wait()
		close(finClear)
	}()

	select {
	case <-time.After(time.Second * 10):
		t.Fatalf("HTLCs not cleared after 10 seconds")
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
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)

	// Finally, shutdown the node we created for the duration of the tests,
	// only leaving the two seed nodes (Alice and Bob) within our test
	// network.
	if err := carol.Shutdown(); err != nil {
		t.Fatalf("unable to shutdown carol: %v", err)
	}
}

func testInvoiceSubscriptions(net *networkHarness, t *harnessTest) {
	const chanAmt = btcutil.Amount(500000)
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 5)

	// Open a channel with 500k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(ctxt, t, net, net.Alice, net.Bob,
		chanAmt, 0)

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

	// Wait for the channel to be recognized by both Alice and Bob before
	// continuing the rest of the test.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		// TODO(roasbeef): will need to make num blocks to advertise a
		// node param
		t.Fatalf("channel not seen by alice before timeout: %v", err)
	}

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
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
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
		chanPoints[i] = openChannelAndAssert(ctx, t, net, net.Alice,
			net.Bob, amount, 0)
	}

	// Close the channel between Alice and Bob, asserting that the
	// channel has been properly closed on-chain.
	for _, chanPoint := range chanPoints {
		ctx, _ := context.WithTimeout(context.Background(), timeout)
		closeChannelAndAssert(ctx, t, net, net.Alice, chanPoint, false)
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
	// increasing pool of pending channels. Then check that we can't open
	// the channel if the number of pending channels exceed max value.
	openStreams := make([]lnrpc.Lightning_OpenChannelClient, maxPendingChannels)
	for i := 0; i < maxPendingChannels; i++ {
		ctx, _ = context.WithTimeout(context.Background(), timeout)
		stream, err := net.OpenChannel(ctx, net.Alice, carol, amount,
			0, 1)
		if err != nil {
			t.Fatalf("unable to open channel: %v", err)
		}
		openStreams[i] = stream
	}

	// Carol exhausted available amount of pending channels, next open
	// channel request should cause ErrorGeneric to be sent back to Alice.
	ctx, _ = context.WithTimeout(context.Background(), timeout)
	_, err = net.OpenChannel(ctx, net.Alice, carol, amount, 0, 1)
	if err == nil {
		t.Fatalf("error wasn't received")
	} else if grpc.Code(err) != lnwire.ErrMaxPendingChannels.ToGrpcCode() {
		t.Fatalf("not expected error was received: %v", err)
	}

	// For now our channels are in pending state, in order to not interfere
	// with other tests we should clean up - complete opening of the
	// channel and then close it.

	// Mine a block, then wait for node's to notify us that the channel has
	// been opened. The funding transactions should be found within the
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

		// Ensure that the funding transaction enters a block, and is
		// properly advertised by Alice.
		assertTxInBlock(t, block, fundingTxID)
		ctxt, _ = context.WithTimeout(context.Background(), timeout)
		err = net.Alice.WaitForNetworkChannelOpen(ctxt, fundingChanPoint)
		if err != nil {
			t.Fatalf("channel not seen on network before "+
				"timeout: %v", err)
		}

		// The channel should be listed in the peer information
		// returned by both peers.
		chanPoint := wire.OutPoint{
			Hash:  *fundingTxID,
			Index: fundingChanPoint.OutputIndex,
		}
		if err := net.AssertChannelExists(ctx, net.Alice, &chanPoint); err != nil {
			t.Fatalf("unable to assert channel existence: %v", err)
		}

		chanPoints[i] = fundingChanPoint
	}

	// Next, close the channel between Alice and Carol, asserting that the
	// channel has been properly closed on-chain.
	for _, chanPoint := range chanPoints {
		ctxt, _ := context.WithTimeout(context.Background(), timeout)
		closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
	}

	// Finally, shutdown the node we created for the duration of the tests,
	// only leaving the two seed nodes (Alice and Bob) within our test
	// network.
	if err := carol.Shutdown(); err != nil {
		t.Fatalf("unable to shutdown carol: %v", err)
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
	chanPoint := openChannelAndAssert(ctxt, t, net, net.Alice, net.Bob,
		chanAmt, 0)

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

	// Wait for Alice to receive the channel edge from the funding manager.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't see the alice->bob channel before "+
			"timeout: %v", err)
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
	breachTXID := closeChannelAndAssert(ctxb, t, net, net.Bob, chanPoint,
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

func testHtlcErrorPropagation(net *networkHarness, t *harnessTest) {
	// In this test we wish to exercise the daemon's correct parsing,
	// handling, and propagation of errors that occur while processing a
	// multi-hop payment.
	timeout := time.Duration(time.Second * 5)
	ctxb := context.Background()

	const chanAmt = btcutil.Amount(btcutil.SatoshiPerBitcoin / 2)

	// First establish a channel with a capacity of 0.5 BTC between Alice
	// and Bob.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointAlice := openChannelAndAssert(ctxt, t, net, net.Alice, net.Bob,
		chanAmt, 0)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPointAlice); err != nil {
		t.Fatalf("channel not seen by alice before timeout: %v", err)
	}

	assertBaseBalance := func() {
		balReq := &lnrpc.ChannelBalanceRequest{}
		aliceBal, err := net.Alice.ChannelBalance(ctxb, balReq)
		if err != nil {
			t.Fatalf("unable to get channel balance: %v", err)
		}
		bobBal, err := net.Bob.ChannelBalance(ctxb, balReq)
		if err != nil {
			t.Fatalf("unable to get channel balance: %v", err)
		}
		if aliceBal.Balance != int64(chanAmt) {
			t.Fatalf("alice has an incorrect balance: expected %v got %v",
				int64(chanAmt), aliceBal)
		}
		if bobBal.Balance != int64(chanAmt) {
			t.Fatalf("bob has an incorrect balance: expected %v got %v",
				int64(chanAmt), bobBal)
		}
	}

	// Since we'd like to test some multi-hop failure scenarios, we'll
	// introduce another node into our test network: Carol.
	carol, err := net.NewNode(nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}

	// Next, we'll create a connection from Bob to Carol, and open a
	// channel between them so we have the topology: Alice -> Bob -> Carol.
	// The channel created will be of lower capacity that the one created
	// above.
	if err := net.ConnectNodes(ctxb, net.Bob, carol); err != nil {
		t.Fatalf("unable to connect bob to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	const bobChanAmt = btcutil.Amount(btcutil.SatoshiPerBitcoin / 2)
	chanPointBob := openChannelAndAssert(ctxt, t, net, net.Bob, carol,
		chanAmt, 0)

	// Ensure that Alice has Carol in her routing table before proceeding.
	nodeInfoReq := &lnrpc.NodeInfoRequest{
		PubKey: carol.PubKeyStr,
	}
	checkTableTimeout := time.After(time.Second * 10)
	checkTableTicker := time.NewTicker(100 * time.Millisecond)
	defer checkTableTicker.Stop()

out:
	// TODO(roasbeef): make into async hook for node announcements
	for {
		select {
		case <-checkTableTicker.C:
			_, err := net.Alice.GetNodeInfo(ctxb, nodeInfoReq)
			if err != nil && strings.Contains(err.Error(),
				"unable to find") {

				continue
			}

			break out
		case <-checkTableTimeout:
			t.Fatalf("carol's node announcement didn't propagate within " +
				"the timeout period")
		}
	}

	// With the channels, open we can now start to test our multi-hop error
	// scenarios. First, we'll generate an invoice from carol that we'll
	// use to test some error cases.
	const payAmt = 10000
	invoiceReq := &lnrpc.Invoice{
		Memo:  "kek99",
		Value: payAmt,
	}
	carolInvoice, err := carol.AddInvoice(ctxb, invoiceReq)
	if err != nil {
		t.Fatalf("unable to generate carol invoice: %v", err)
	}

	// Before we send the payment, ensure that the announcement of the new
	// channel has been processed by Alice.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPointBob); err != nil {
		t.Fatalf("channel not seen by alice before timeout: %v", err)
	}

	// TODO(roasbeef): return failure response rather than failing entire
	// stream on payment error.
	alicePayStream, err := net.Alice.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create payment stream: %v", err)
	}

	// For the first scenario, we'll test the cancellation of an HTLC with
	// an unknown payment hash.
	sendReq := &lnrpc.SendRequest{
		PaymentHash: bytes.Repeat([]byte("Z"), 32), // Wrong hash.
		Dest:        carol.PubKey[:],
		Amt:         payAmt,
	}
	if err := alicePayStream.Send(sendReq); err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// The payment should've resulted in an error since we went it with the
	// wrong payment hash.
	_, err = alicePayStream.Recv()
	if err == nil {
		t.Fatalf("payment should have been rejected due to invalid " +
			"payment hash")
	} else if !strings.Contains(err.Error(), "preimage") {
		// TODO(roasbeef): make into proper gRPC error code
		t.Fatalf("payment should have failed due to unknown preimage, "+
			"instead failed due to : %v", err)
	}

	// The balances of all parties should be the same as initially since
	// the HTLC was cancelled.
	assertBaseBalance()

	// We need to create another payment stream since the first one was
	// closed due to an error.
	alicePayStream, err = net.Alice.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create payment stream: %v", err)
	}

	// Next, we'll test the case of a recognized payHash but, an incorrect
	// value on the extended HTLC.
	sendReq = &lnrpc.SendRequest{
		PaymentHash: carolInvoice.RHash,
		Dest:        carol.PubKey[:],
		Amt:         1000, // 10k satoshis are expected.
	}
	if err := alicePayStream.Send(sendReq); err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// The payment should fail with an error since we sent 1k satoshis
	// isn't of 10k as was requested.
	_, err = alicePayStream.Recv()
	if err == nil {
		t.Fatalf("payment should have been rejected due to wrong " +
			"HTLC amount")
	} else if !strings.Contains(err.Error(), "htlc value") {
		t.Fatalf("payment should have failed due to wrong amount, "+
			"instead failed due to: %v", err)
	}

	// The balances of all parties should be the same as initially since
	// the HTLC was cancelled.
	assertBaseBalance()

	// Next we'll test an error that occurs mid-route due to an outgoing
	// link having insufficient capacity. In order to do so, we'll first
	// need to unbalance the link connecting Bob<->Carol.
	bobPayStream, err := net.Bob.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create payment stream: %v", err)
	}

	// To do so, we'll push most of the funds in the channel over to
	// Alice's side, leaving on 10k satoshis of available balance for bob.
	invoiceReq = &lnrpc.Invoice{
		Value: int64(chanAmt) - 10000,
	}
	carolInvoice2, err := carol.AddInvoice(ctxb, invoiceReq)
	if err != nil {
		t.Fatalf("unable to generate carol invoice: %v", err)
	}
	if err := bobPayStream.Send(&lnrpc.SendRequest{
		PaymentRequest: carolInvoice2.PaymentRequest,
	}); err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}
	if _, err := bobPayStream.Recv(); err != nil {
		t.Fatalf("bob's payment failed: %v", err)
	}

	// At this point, Alice has 50mil satoshis on her side of the channel,
	// but Bob only has 10k available on his side of the channel. So a
	// payment from Alice to Carol worth 100k satoshis should fail.
	alicePayStream, err = net.Alice.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create payment stream: %v", err)
	}
	invoiceReq = &lnrpc.Invoice{
		Value: 100000,
	}
	carolInvoice3, err := carol.AddInvoice(ctxb, invoiceReq)
	if err != nil {
		t.Fatalf("unable to generate carol invoice: %v", err)
	}
	if err := alicePayStream.Send(&lnrpc.SendRequest{
		PaymentRequest: carolInvoice3.PaymentRequest,
	}); err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}
	_, err = alicePayStream.Recv()
	if err == nil {
		t.Fatalf("payment should fail due to insufficient "+
			"capacity: %v", err)
	} else if !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("payment should fail due to insufficient capacity, "+
			"instead: %v", err)
	}

	// For our final test, we'll ensure that if a target link isn't
	// available for what ever reason then the payment fails accordingly.
	//
	// We'll attempt to complete the original invoice we created with Carol
	// above, but before we do so, Carol will go offline, resulting in a
	// failed payment.
	if err := carol.Shutdown(); err != nil {
		t.Fatalf("unable to shutdown carol: %v", err)
	}
	time.Sleep(time.Second * 2)
	alicePayStream, err = net.Alice.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create payment stream: %v", err)
	}
	if err := alicePayStream.Send(&lnrpc.SendRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
	}); err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}
	_, err = alicePayStream.Recv()
	if err == nil {
		t.Fatalf("payment should have failed")
	} else if !strings.Contains(err.Error(), "hop unknown") {
		t.Fatalf("payment should fail due to unknown hop, instead: %v",
			err)
	}

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)

	// Force close Bob's final channel, also mining enough blocks to
	// trigger a sweep of the funds by the utxoNursery.
	// TODO(roasbeef): use config value for default CSV here.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, chanPointBob, true)
	if _, err := net.Miner.Node.Generate(5); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}
}

func testGraphTopologyNotifications(net *networkHarness, t *harnessTest) {
	const chanAmt = btcutil.Amount(btcutil.SatoshiPerBitcoin / 2)
	timeout := time.Duration(time.Second * 5)
	ctxb := context.Background()

	// We'll first start by establishing a notification client to Alice
	// which'll send us notifications upon detected changes in the channel
	// graph.
	req := &lnrpc.GraphTopologySubscription{}
	topologyClient, err := net.Alice.SubscribeChannelGraph(ctxb, req)
	if err != nil {
		t.Fatalf("unable to create topology client: %v", err)
	}

	// Open a new channel between Alice and Bob.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(ctxt, t, net, net.Alice, net.Bob,
		chanAmt, 0)

	// We'll launch a goroutine that'll be responsible for proxying all
	// notifications recv'd from the client into the channel below.
	quit := make(chan struct{})
	graphUpdates := make(chan *lnrpc.GraphTopologyUpdate, 3)
	go func() {
		for {
			select {
			case <-quit:
				return
			default:
				graphUpdate, err := topologyClient.Recv()
				if err == io.EOF {
					return
				} else if err != nil {
					t.Fatalf("unable to recv graph update: %v", err)
				}

				graphUpdates <- graphUpdate
			}
		}
	}()

	// The channel opening above should've triggered a new notification
	// sent to the notification client.
	const numExpectedUpdates = 2
	for i := 0; i < numExpectedUpdates; i++ {
		select {
		// Ensure that a new update for both created edges is properly
		// dispatched to our registered client.
		case graphUpdate := <-graphUpdates:
			if len(graphUpdate.ChannelUpdates) != 1 {
				t.Fatalf("expected a single update, instead "+
					"have %v", len(graphUpdate.ChannelUpdates))
			}

			chanUpdate := graphUpdate.ChannelUpdates[0]
			if chanUpdate.Capacity != int64(chanAmt) {
				t.Fatalf("channel capacities mismatch: expected %v, "+
					"got %v", chanAmt, chanUpdate.Capacity)
			}
			switch chanUpdate.AdvertisingNode {
			case net.Alice.PubKeyStr:
			case net.Bob.PubKeyStr:
			default:
				t.Fatalf("unknown advertising node: %v",
					chanUpdate.AdvertisingNode)
			}
			switch chanUpdate.ConnectingNode {
			case net.Alice.PubKeyStr:
			case net.Bob.PubKeyStr:
			default:
				t.Fatalf("unknown connecting node: %v",
					chanUpdate.ConnectingNode)
			}
		case <-time.After(time.Second * 10):
			t.Fatalf("notification for new channel not sent")
		}
	}

	_, blockHeight, err := net.Miner.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	// Now we'll test that updates upon a channel closure are properly sent
	// when channels are closed within the network.
	ctxt, _ = context.WithTimeout(context.Background(), timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)

	// Similar to the case above, we should receive another notification
	// detailing the channel closure.
	select {
	case graphUpdate := <-graphUpdates:
		if len(graphUpdate.ClosedChans) != 1 {
			t.Fatalf("expected a single update, instead "+
				"have %v", len(graphUpdate.ClosedChans))
		}

		closedChan := graphUpdate.ClosedChans[0]
		if closedChan.ClosedHeight != uint32(blockHeight+1) {
			t.Fatalf("close heights of channel mismatch: expected "+
				"%v, got %v", blockHeight+1, closedChan.ClosedHeight)
		}
		if !bytes.Equal(closedChan.ChanPoint.FundingTxid,
			chanPoint.FundingTxid) {
			t.Fatalf("channel point hash mismatch: expected %v, "+
				"got %v", chanPoint.FundingTxid,
				closedChan.ChanPoint.FundingTxid)
		}
		if closedChan.ChanPoint.OutputIndex != chanPoint.OutputIndex {
			t.Fatalf("output index mismatch: expected %v, got %v",
				chanPoint.OutputIndex, closedChan.ChanPoint)
		}
	case <-time.After(time.Second * 10):
		t.Fatalf("notification for channel closure not " +
			"sent")
	}

	// For the final portion of the test, we'll ensure that once a new node
	// appears in the network, the proper notification is dispatched.
	carol, err := net.NewNode(nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}

	// Connect the new node above to Alice. This should result in the nodes
	// syncing up their respective graph state, with the new addition being
	// the existence of Carol in the graph.
	if err := net.ConnectNodes(ctxb, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect alice to carol: %v", err)
	}

	// We should receive an update advertising the newly connected node.
	select {
	case graphUpdate := <-graphUpdates:
		if len(graphUpdate.NodeUpdates) != 1 {
			t.Fatalf("expected a single update, instead "+
				"have %v", len(graphUpdate.NodeUpdates))
		}

		nodeUpdate := graphUpdate.NodeUpdates[0]
		if nodeUpdate.IdentityKey != carol.PubKeyStr {
			t.Fatalf("node update pubkey mismatch: expected %v, got %v",
				carol.PubKeyStr, nodeUpdate.IdentityKey)
		}
	case <-time.After(time.Second * 10):
		t.Fatalf("node update ntfn not sent")
	}

	close(quit)

	// Finally, shutdown carol as our test has concluded successfully.
	if err := carol.Shutdown(); err != nil {
		t.Fatalf("unable to shutdown carol: %v", err)
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
		name: "graph topology notifications",
		test: testGraphTopologyNotifications,
	},
	{
		name: "funding flow persistence",
		test: testChannelFundingPersistence,
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
		name: "multi-hop htlc error propagation",
		test: testHtlcErrorPropagation,
	},
	// TODO(roasbeef): multi-path integration test
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
	// drive blockchain related events within the network. Revert the default
	// setting of accepting non-standard transactions on simnet to reject them.
	// Transactions on the lightning network should always be standard to get
	// better guarantees of getting included in to blocks.
	args := []string{"--rejectnonstd"}
	btcdHarness, err := rpctest.New(harnessNetParams, handlers, args)
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
