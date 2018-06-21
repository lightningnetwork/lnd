// +build rpctest

package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sync/atomic"

	"encoding/hex"
	"reflect"

	"crypto/rand"
	prand "math/rand"

	"github.com/btcsuite/btclog"
	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/integration/rpctest"
	"github.com/roasbeef/btcd/rpcclient"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

var (
	harnessNetParams = &chaincfg.SimNetParams
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
func (h *harnessTest) RunTestCase(testCase *testCase,
	net *lntest.NetworkHarness) {

	h.testCase = testCase
	defer func() {
		h.testCase = nil
	}()

	defer func() {
		if err := recover(); err != nil {
			description := errors.Wrap(err, 2).ErrorStack()
			h.t.Fatalf("Failed: (%v) panicked with: \n%v",
				h.testCase.name, description)
		}
	}()

	testCase.test(net, h)

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

	t.Fatalf("tx was not included in block")
}

// mineBlocks mine 'num' of blocks and check that blocks are present in
// node blockchain.
func mineBlocks(t *harnessTest, net *lntest.NetworkHarness, num uint32) []*wire.MsgBlock {
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
func openChannelAndAssert(ctx context.Context, t *harnessTest,
	net *lntest.NetworkHarness, alice, bob *lntest.HarnessNode,
	fundingAmt btcutil.Amount, pushAmt btcutil.Amount,
	private bool) *lnrpc.ChannelPoint {

	chanOpenUpdate, err := net.OpenChannel(
		ctx, alice, bob, fundingAmt, pushAmt, private,
	)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// Mine 6 blocks, then wait for Alice's node to notify us that the
	// channel has been opened. The funding transaction should be found
	// within the first newly mined block. We mine 6 blocks so that in the
	// case that the channel is public, it is announced to the network.
	block := mineBlocks(t, net, 6)[0]

	fundingChanPoint, err := net.WaitForChannelOpen(ctx, chanOpenUpdate)
	if err != nil {
		t.Fatalf("error while waiting for channel open: %v", err)
	}
	txidHash, err := getChanPointFundingTxid(fundingChanPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	fundingTxID, err := chainhash.NewHash(txidHash)
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
	if err := net.AssertChannelExists(ctx, bob, &chanPoint); err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}

	return fundingChanPoint
}

// closeChannelAndAssert attempts to close a channel identified by the passed
// channel point owned by the passed Lightning node. A fully blocking channel
// closure is attempted, therefore the passed context should be a child derived
// via timeout from a base parent. Additionally, once the channel has been
// detected as closed, an assertion checks that the transaction is found within
// a block.
func closeChannelAndAssert(ctx context.Context, t *harnessTest,
	net *lntest.NetworkHarness, node *lntest.HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint, force bool) *chainhash.Hash {

	closeUpdates, _, err := net.CloseChannel(ctx, node, fundingChanPoint, force)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	txidHash, err := getChanPointFundingTxid(fundingChanPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	txid, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to convert to chainhash: %v", err)
	}
	chanPointStr := fmt.Sprintf("%v:%v", txid, fundingChanPoint.OutputIndex)

	// If we didn't force close the transaction, at this point, the channel
	// should now be marked as being in the state of "waiting close".
	if !force {
		pendingChansRequest := &lnrpc.PendingChannelsRequest{}
		pendingChanResp, err := node.PendingChannels(ctx, pendingChansRequest)
		if err != nil {
			t.Fatalf("unable to query for pending channels: %v", err)
		}
		var found bool
		for _, pendingClose := range pendingChanResp.WaitingCloseChannels {
			if pendingClose.Channel.ChannelPoint == chanPointStr {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("channel not marked as waiting close")
		}
	}

	// We'll now, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block.
	block := mineBlocks(t, net, 1)[0]

	closingTxid, err := net.WaitForChannelClose(ctx, closeUpdates)
	if err != nil {
		t.Fatalf("error while waiting for channel close: %v", err)
	}

	assertTxInBlock(t, block, closingTxid)

	// Finally, the transaction should no longer be in the waiting close
	// state as we've just mined a block that should include the closing
	// transaction. This only applies for co-op close channels though.
	if !force {
		err = lntest.WaitPredicate(func() bool {
			pendingChansRequest := &lnrpc.PendingChannelsRequest{}
			pendingChanResp, err := node.PendingChannels(
				ctx, pendingChansRequest,
			)
			if err != nil {
				return false
			}

			for _, pendingClose := range pendingChanResp.WaitingCloseChannels {
				if pendingClose.Channel.ChannelPoint == chanPointStr {
					return false
				}
			}

			return true
		}, time.Second*15)
		if err != nil {
			t.Fatalf("closing transaction not marked as fully closed")
		}
	}

	return closingTxid
}

// numOpenChannelsPending sends an RPC request to a node to get a count of the
// node's channels that are currently in a pending state (with a broadcast, but
// not confirmed funding transaction).
func numOpenChannelsPending(ctxt context.Context, node *lntest.HarnessNode) (int, error) {
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	resp, err := node.PendingChannels(ctxt, pendingChansRequest)
	if err != nil {
		return 0, err
	}
	return len(resp.PendingOpenChannels), nil
}

// assertNumOpenChannelsPending asserts that a pair of nodes have the expected
// number of pending channels between them.
func assertNumOpenChannelsPending(ctxt context.Context, t *harnessTest,
	alice, bob *lntest.HarnessNode, expected int) {

	const nPolls = 10

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for i := 0; i < nPolls; i++ {
		aliceNumChans, err := numOpenChannelsPending(ctxt, alice)
		if err != nil {
			t.Fatalf("error fetching alice's node (%v) pending channels %v",
				alice.NodeID, err)
		}
		bobNumChans, err := numOpenChannelsPending(ctxt, bob)
		if err != nil {
			t.Fatalf("error fetching bob's node (%v) pending channels %v",
				bob.NodeID, err)
		}

		isLastIteration := i == nPolls-1

		aliceStateCorrect := aliceNumChans == expected
		if !aliceStateCorrect && isLastIteration {
			t.Fatalf("number of pending channels for alice incorrect. "+
				"expected %v, got %v", expected, aliceNumChans)
		}

		bobStateCorrect := bobNumChans == expected
		if !bobStateCorrect && isLastIteration {
			t.Fatalf("number of pending channels for bob incorrect. "+
				"expected %v, got %v",
				expected, bobNumChans)
		}

		if aliceStateCorrect && bobStateCorrect {
			return
		}

		<-ticker.C
	}
}

// assertNumConnections asserts number current connections between two peers.
func assertNumConnections(ctxt context.Context, t *harnessTest,
	alice, bob *lntest.HarnessNode, expected int) {

	const nPolls = 10

	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()

	for i := nPolls - 1; i >= 0; i-- {
		select {
		case <-tick.C:
			aNumPeers, err := alice.ListPeers(ctxt, &lnrpc.ListPeersRequest{})
			if err != nil {
				t.Fatalf("unable to fetch alice's node (%v) list peers %v",
					alice.NodeID, err)
			}
			bNumPeers, err := bob.ListPeers(ctxt, &lnrpc.ListPeersRequest{})
			if err != nil {
				t.Fatalf("unable to fetch bob's node (%v) list peers %v",
					bob.NodeID, err)
			}
			if len(aNumPeers.Peers) != expected {
				// Continue polling if this is not the final
				// loop.
				if i > 0 {
					continue
				}
				t.Fatalf("number of peers connected to alice is incorrect: "+
					"expected %v, got %v", expected, len(aNumPeers.Peers))
			}
			if len(bNumPeers.Peers) != expected {
				// Continue polling if this is not the final
				// loop.
				if i > 0 {
					continue
				}
				t.Fatalf("number of peers connected to bob is incorrect: "+
					"expected %v, got %v", expected, len(bNumPeers.Peers))
			}

			// Alice and Bob both have the required number of
			// peers, stop polling and return to caller.
			return
		}
	}
}

// shutdownAndAssert shuts down the given node and asserts that no errors
// occur.
func shutdownAndAssert(net *lntest.NetworkHarness, t *harnessTest,
	node *lntest.HarnessNode) {
	if err := net.ShutdownNode(node); err != nil {
		t.Fatalf("unable to shutdown %v: %v", node.Name(), err)
	}
}

// calcStaticFee calculates appropriate fees for commitment transactions.  This
// function provides a simple way to allow test balance assertions to take fee
// calculations into account.
//
// TODO(bvu): Refactor when dynamic fee estimation is added.
// TODO(conner) remove code duplication
func calcStaticFee(numHTLCs int) btcutil.Amount {
	const (
		commitWeight = btcutil.Amount(724)
		htlcWeight   = 172
		feePerKw     = btcutil.Amount(50 * 1000 / 4)
	)
	return feePerKw * (commitWeight +
		btcutil.Amount(htlcWeight*numHTLCs)) / 1000
}

// completePaymentRequests sends payments from a lightning node to complete all
// payment requests. If the awaitResponse parameter is true, this function
// does not return until all payments successfully complete without errors.
func completePaymentRequests(ctx context.Context, client lnrpc.LightningClient,
	paymentRequests []string, awaitResponse bool) error {

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	payStream, err := client.SendPayment(ctx)
	if err != nil {
		return err
	}

	for _, payReq := range paymentRequests {
		sendReq := &lnrpc.SendRequest{PaymentRequest: payReq}
		err := payStream.Send(sendReq)
		if err != nil {
			return err
		}
	}

	if awaitResponse {
		for range paymentRequests {
			resp, err := payStream.Recv()
			if err != nil {
				return err
			}
			if resp.PaymentError != "" {
				return fmt.Errorf("received payment error: %v",
					resp.PaymentError)
			}
		}
	} else {
		// We are not waiting for feedback in the form of a response, but we
		// should still wait long enough for the server to receive and handle
		// the send before cancelling the request.
		time.Sleep(200 * time.Millisecond)
	}

	return nil
}

const (
	AddrTypeWitnessPubkeyHash = lnrpc.NewAddressRequest_WITNESS_PUBKEY_HASH
	AddrTypeNestedPubkeyHash  = lnrpc.NewAddressRequest_NESTED_PUBKEY_HASH
)

// testOnchainFundRecovery checks lnd's ability to rescan for onchain outputs
// when providing a valid aezeed that owns outputs on the chain. This test
// performs multiple restorations using the same seed and various recovery
// windows to ensure we detect funds properly.
func testOnchainFundRecovery(net *lntest.NetworkHarness, t *harnessTest) {
	timeout := time.Duration(time.Second * 15)
	ctxb := context.Background()

	// First, create a new node with strong passphrase and grab the mnemonic
	// used for key derivation. This will bring up Carol with an empty
	// wallet, and such that she is synced up.
	password := []byte("The Magic Words are Squeamish Ossifrage")
	carol, mnemonic, err := net.NewNodeWithSeed("Carol", nil, password)
	if err != nil {
		t.Fatalf("unable to create node with seed; %v", err)
	}
	shutdownAndAssert(net, t, carol)

	// Create a closure for testing the recovery of Carol's wallet. This
	// method takes the expected value of Carol's balance when using the
	// given recovery window. Additionally, the caller can specify an action
	// to perform on the restored node before the node is shutdown.
	restoreCheckBalance := func(expAmount int64, recoveryWindow int32,
		fn func(*lntest.HarnessNode)) {

		// Restore Carol, passing in the password, mnemonic, and
		// desired recovery window.
		node, err := net.RestoreNodeWithSeed(
			"Carol", nil, password, mnemonic, recoveryWindow,
		)
		if err != nil {
			t.Fatalf("unable to restore node: %v", err)
		}

		// Query carol for her current wallet balance.
		var currBalance int64
		err = lntest.WaitPredicate(func() bool {
			req := &lnrpc.WalletBalanceRequest{}
			ctxt, _ := context.WithTimeout(ctxb, timeout)
			resp, err := node.WalletBalance(ctxt, req)
			if err != nil {
				t.Fatalf("unable to query wallet balance: %v",
					err)
			}

			// Verify that Carol's balance matches our expected
			// amount.
			currBalance = resp.ConfirmedBalance
			if expAmount != currBalance {
				return false
			}

			return true
		}, 15*time.Second)
		if err != nil {
			t.Fatalf("expected restored node to have %d satoshis, "+
				"instead has %d satoshis", expAmount,
				currBalance)
		}

		// If the user provided a callback, execute the commands against
		// the restored Carol.
		if fn != nil {
			fn(node)
		}

		// Lastly, shutdown this Carol so we can move on to the next
		// restoration.
		shutdownAndAssert(net, t, node)
	}

	// Create a closure-factory for building closures that can generate and
	// skip a configurable number of addresses, before finally sending coins
	// to a next generated address. The returned closure will apply the same
	// behavior to both default P2WKH and NP2WKH scopes.
	skipAndSend := func(nskip int) func(*lntest.HarnessNode) {
		return func(node *lntest.HarnessNode) {
			newP2WKHAddrReq := &lnrpc.NewAddressRequest{
				Type: AddrTypeWitnessPubkeyHash,
			}

			newNP2WKHAddrReq := &lnrpc.NewAddressRequest{
				Type: AddrTypeNestedPubkeyHash,
			}

			// Generate and skip the number of addresses requested.
			for i := 0; i < nskip; i++ {
				ctxt, _ := context.WithTimeout(ctxb, timeout)
				_, err = node.NewAddress(ctxt, newP2WKHAddrReq)
				if err != nil {
					t.Fatalf("unable to generate new "+
						"p2wkh address: %v", err)
				}

				ctxt, _ = context.WithTimeout(ctxb, timeout)
				_, err = node.NewAddress(ctxt, newNP2WKHAddrReq)
				if err != nil {
					t.Fatalf("unable to generate new "+
						"np2wkh address: %v", err)
				}
			}

			// Send one BTC to the next P2WKH address.
			ctxt, _ := context.WithTimeout(ctxb, timeout)
			err = net.SendCoins(
				ctxt, btcutil.SatoshiPerBitcoin, node,
			)
			if err != nil {
				t.Fatalf("unable to send coins to node: %v",
					err)
			}

			// And another to the next NP2WKH address.
			ctxt, _ = context.WithTimeout(ctxb, timeout)
			err = net.SendCoinsNP2WKH(
				ctxt, btcutil.SatoshiPerBitcoin, node,
			)
			if err != nil {
				t.Fatalf("unable to send coins to node: %v",
					err)
			}
		}
	}

	// Restore Carol with a recovery window of 0. Since no coins have been
	// sent, her balance should be zero.
	//
	// After, one BTC is sent to both her first external P2WKH and NP2WKH
	// addresses.
	restoreCheckBalance(0, 0, skipAndSend(0))

	// Check that restoring without a look-ahead results in having no funds
	// in the wallet, even though they exist on-chain.
	restoreCheckBalance(0, 0, nil)

	// Now, check that using a look-ahead of 1 recovers the balance from the
	// two transactions above.
	//
	// After, we will generate and skip 9 P2WKH and NP2WKH addresses, and
	// send another BTC to the subsequent 10th address in each derivation
	// path.
	restoreCheckBalance(2*btcutil.SatoshiPerBitcoin, 1, skipAndSend(9))

	// Check that using a recovery window of 9 does not find the two most
	// recent txns.
	restoreCheckBalance(2*btcutil.SatoshiPerBitcoin, 9, nil)

	// Extending our recovery window to 10 should find the most recent
	// transactions, leaving the wallet with 4 BTC total.
	//
	// After, we will skip 19 more addrs, sending to the 20th address past
	// our last found address, and repeat the same checks.
	restoreCheckBalance(4*btcutil.SatoshiPerBitcoin, 10, skipAndSend(19))

	// Check that recovering with a recovery window of 19 fails to find the
	// most recent transactions.
	restoreCheckBalance(4*btcutil.SatoshiPerBitcoin, 19, nil)

	// Ensure that using a recovery window of 20 succeeds.
	restoreCheckBalance(6*btcutil.SatoshiPerBitcoin, 20, nil)
}

// testBasicChannelFunding performs a test exercising expected behavior from a
// basic funding workflow. The test creates a new channel between Alice and
// Bob, then immediately closes the channel after asserting some expected post
// conditions. Finally, the chain itself is checked to ensure the closing
// transaction was mined.
func testBasicChannelFunding(net *lntest.NetworkHarness, t *harnessTest) {
	timeout := time.Duration(time.Second * 5)
	ctxb := context.Background()

	chanAmt := maxBtcFundingAmount
	pushAmt := btcutil.Amount(100000)

	// First establish a channel with a capacity of 0.5 BTC between Alice
	// and Bob with Alice pushing 100k satoshis to Bob's side during
	// funding. This function will block until the channel itself is fully
	// open or an error occurs in the funding process. A series of
	// assertions will be executed to ensure the funding process completed
	// successfully.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, chanAmt, pushAmt, false,
	)

	ctxt, _ = context.WithTimeout(ctxb, time.Second*15)
	err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
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
	if aliceBal.Balance != int64(chanAmt-pushAmt-calcStaticFee(0)) {
		t.Fatalf("alice's balance is incorrect: expected %v got %v",
			chanAmt-pushAmt-calcStaticFee(0), aliceBal)
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

// txStr returns the string representation of the channel's funding transaction.
func txStr(chanPoint *lnrpc.ChannelPoint) string {
	txidHash, err := getChanPointFundingTxid(chanPoint)
	if err != nil {
		return ""
	}
	fundingTxID, err := chainhash.NewHash(txidHash)
	if err != nil {
		return ""
	}
	cp := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: chanPoint.OutputIndex,
	}
	return cp.String()
}

// waitForChannelUpdate waits for a node to receive updates from the advertising
// node for the specified channels.
func waitForChannelUpdate(t *harnessTest, graphUpdates chan *lnrpc.GraphTopologyUpdate,
	advertisingNode string, expectedPolicy *lnrpc.RoutingPolicy,
	chanPoints ...*lnrpc.ChannelPoint) {

	// Create a set containing all the channel points we are awaiting
	// updates for.
	cps := make(map[string]struct{})
	for _, chanPoint := range chanPoints {
		cps[txStr(chanPoint)] = struct{}{}
	}
out:
	for {
		select {
		case graphUpdate := <-graphUpdates:
			for _, update := range graphUpdate.ChannelUpdates {
				fundingTxStr := txStr(update.ChanPoint)
				if _, ok := cps[fundingTxStr]; !ok {
					continue
				}

				if update.AdvertisingNode != advertisingNode {
					continue
				}

				err := checkChannelPolicy(
					update.RoutingPolicy, expectedPolicy,
				)
				if err != nil {
					continue
				}

				// We got a policy update that matched the
				// values and channel point of what we
				// expected, delete it from the map.
				delete(cps, fundingTxStr)

				// If we have no more channel points we are
				// waiting for, break out of the loop.
				if len(cps) == 0 {
					break out
				}
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("did not receive channel update")
		}
	}
}

// assertChannelPolicy asserts that the passed node's known channel policy for
// the passed chanPoint is consistent with the expected policy values.
func assertChannelPolicy(t *harnessTest, node *lntest.HarnessNode,
	advertisingNode string, expectedPolicy *lnrpc.RoutingPolicy,
	chanPoints ...*lnrpc.ChannelPoint) {

	descReq := &lnrpc.ChannelGraphRequest{}
	chanGraph, err := node.DescribeGraph(context.Background(), descReq)
	if err != nil {
		t.Fatalf("unable to query for alice's graph: %v", err)
	}

out:
	for _, chanPoint := range chanPoints {
		for _, e := range chanGraph.Edges {
			if e.ChanPoint != txStr(chanPoint) {
				continue
			}

			var err error
			if e.Node1Pub == advertisingNode {
				err = checkChannelPolicy(
					e.Node1Policy, expectedPolicy,
				)
			} else {
				err = checkChannelPolicy(
					e.Node2Policy, expectedPolicy,
				)
			}
			if err != nil {
				t.Fatalf(err.Error())
			}

			continue out
		}

		// If we've iterated over all the known edges and we weren't
		// able to find this specific one, then we'll fail.
		t.Fatalf("did not find edge %v", txStr(chanPoint))
	}
}

// checkChannelPolicy checks that the policy matches the expected one.
func checkChannelPolicy(policy, expectedPolicy *lnrpc.RoutingPolicy) error {
	if policy.FeeBaseMsat != expectedPolicy.FeeBaseMsat {
		return fmt.Errorf("expected base fee %v, got %v",
			expectedPolicy.FeeBaseMsat, policy.FeeBaseMsat)
	}
	if policy.FeeRateMilliMsat != expectedPolicy.FeeRateMilliMsat {
		return fmt.Errorf("expected fee rate %v, got %v",
			expectedPolicy.FeeRateMilliMsat,
			policy.FeeRateMilliMsat)
	}
	if policy.TimeLockDelta != expectedPolicy.TimeLockDelta {
		return fmt.Errorf("expected time lock delta %v, got %v",
			expectedPolicy.TimeLockDelta,
			policy.TimeLockDelta)
	}

	return nil
}

// testUpdateChannelPolicy tests that policy updates made to a channel
// gets propagated to other nodes in the network.
func testUpdateChannelPolicy(net *lntest.NetworkHarness, t *harnessTest) {
	timeout := time.Duration(time.Second * 5)
	ctxb := context.Background()

	// Launch notification clients for all nodes, such that we can
	// get notified when they discover new channels and updates in the
	// graph.
	aliceUpdates, aQuit := subscribeGraphNotifications(t, ctxb, net.Alice)
	defer close(aQuit)
	bobUpdates, bQuit := subscribeGraphNotifications(t, ctxb, net.Bob)
	defer close(bQuit)

	chanAmt := maxBtcFundingAmount
	pushAmt := btcutil.Amount(100000)

	// Create a channel Alice->Bob.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, chanAmt, pushAmt, false,
	)

	ctxt, _ = context.WithTimeout(ctxb, time.Second*15)
	err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}

	// Create Carol and a new channel Bob->Carol.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}

	// Clean up carol's node when the test finishes.
	defer shutdownAndAssert(net, t, carol)

	carolUpdates, cQuit := subscribeGraphNotifications(t, ctxb, carol)
	defer close(cQuit)

	if err := net.ConnectNodes(ctxb, carol, net.Bob); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPoint2 := openChannelAndAssert(
		ctxt, t, net, net.Bob, carol, chanAmt, pushAmt, false,
	)

	ctxt, _ = context.WithTimeout(ctxb, time.Second*15)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint2)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint2)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPoint2)
	if err != nil {
		t.Fatalf("carol didn't report channel: %v", err)
	}

	// With our little cluster set up, we'll update the fees for the
	// channel Bob side of the Alice->Bob channel, and make sure all nodes
	// learn about it.
	const feeBase = 1000000
	baseFee := int64(1500)
	feeRate := int64(12)
	timeLockDelta := uint32(66)

	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMsat:      baseFee,
		FeeRateMilliMsat: feeBase * feeRate,
		TimeLockDelta:    timeLockDelta,
	}

	req := &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   baseFee,
		FeeRate:       float64(feeRate),
		TimeLockDelta: timeLockDelta,
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: chanPoint,
		},
	}

	if _, err := net.Bob.UpdateChannelPolicy(ctxb, req); err != nil {
		t.Fatalf("unable to get alice's balance: %v", err)
	}

	// Wait for all nodes to have seen the policy update done by Bob.
	waitForChannelUpdate(
		t, aliceUpdates, net.Bob.PubKeyStr, expectedPolicy, chanPoint,
	)
	waitForChannelUpdate(
		t, bobUpdates, net.Bob.PubKeyStr, expectedPolicy, chanPoint,
	)
	waitForChannelUpdate(
		t, carolUpdates, net.Bob.PubKeyStr, expectedPolicy, chanPoint,
	)

	// Check that all nodes now know about Bob's updated policy.
	assertChannelPolicy(
		t, net.Alice, net.Bob.PubKeyStr, expectedPolicy, chanPoint,
	)
	assertChannelPolicy(
		t, net.Bob, net.Bob.PubKeyStr, expectedPolicy, chanPoint,
	)
	assertChannelPolicy(
		t, carol, net.Bob.PubKeyStr, expectedPolicy, chanPoint,
	)

	// Now that all nodes have received the new channel update, we'll try
	// to send a payment from Alice to Carol to ensure that Alice has
	// internalized this fee update. This shouldn't affect the route that
	// Alice takes though: we updated the Alice -> Bob channel and she
	// doesn't pay for transit over that channel as it's direct.
	payAmt := lnwire.MilliSatoshi(2000)
	invoice := &lnrpc.Invoice{
		Memo:  "testing",
		Value: int64(payAmt),
	}
	resp, err := carol.AddInvoice(ctxb, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(
		ctxt, net.Alice, []string{resp.PaymentRequest}, true,
	)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// We'll now open a channel from Alice directly to Carol.
	if err := net.ConnectNodes(ctxb, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPoint3 := openChannelAndAssert(
		ctxt, t, net, net.Alice, carol, chanAmt, pushAmt, false,
	)

	ctxt, _ = context.WithTimeout(ctxb, time.Second*15)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint3)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPoint3)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}

	// Make a global update, and check that both channels' new policies get
	// propagated.
	baseFee = int64(800)
	feeRate = int64(123)
	timeLockDelta = uint32(22)

	expectedPolicy.FeeBaseMsat = baseFee
	expectedPolicy.FeeRateMilliMsat = feeBase * feeRate
	expectedPolicy.TimeLockDelta = timeLockDelta

	req = &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   baseFee,
		FeeRate:       float64(feeRate),
		TimeLockDelta: timeLockDelta,
	}
	req.Scope = &lnrpc.PolicyUpdateRequest_Global{}

	_, err = net.Alice.UpdateChannelPolicy(ctxb, req)
	if err != nil {
		t.Fatalf("unable to get alice's balance: %v", err)
	}

	// Wait for all nodes to have seen the policy updates for both of
	// Alice's channels.
	waitForChannelUpdate(
		t, aliceUpdates, net.Alice.PubKeyStr, expectedPolicy, chanPoint,
		chanPoint3,
	)
	waitForChannelUpdate(
		t, bobUpdates, net.Alice.PubKeyStr, expectedPolicy, chanPoint,
		chanPoint3,
	)
	waitForChannelUpdate(
		t, carolUpdates, net.Alice.PubKeyStr, expectedPolicy, chanPoint,
		chanPoint3,
	)

	// And finally check that all nodes remembers the policy update they
	// received.
	assertChannelPolicy(
		t, net.Alice, net.Alice.PubKeyStr, expectedPolicy, chanPoint,
		chanPoint3,
	)
	assertChannelPolicy(
		t, net.Bob, net.Alice.PubKeyStr, expectedPolicy, chanPoint,
		chanPoint3,
	)
	assertChannelPolicy(
		t, carol, net.Alice.PubKeyStr, expectedPolicy, chanPoint,
		chanPoint3,
	)

	// Close the channels.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, chanPoint2, false)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint3, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
}

// testOpenChannelAfterReorg tests that in the case where we have an open
// channel where the funding tx gets reorged out, the channel will no
// longer be present in the node's routing table.
func testOpenChannelAfterReorg(net *lntest.NetworkHarness, t *harnessTest) {
	timeout := time.Duration(time.Second * 5)
	ctxb := context.Background()

	// Set up a new miner that we can use to cause a reorg.
	args := []string{"--rejectnonstd"}
	miner, err := rpctest.New(harnessNetParams,
		&rpcclient.NotificationHandlers{}, args)
	if err != nil {
		t.Fatalf("unable to create mining node: %v", err)
	}
	if err := miner.SetUp(true, 50); err != nil {
		t.Fatalf("unable to set up mining node: %v", err)
	}
	defer miner.TearDown()

	if err := miner.Node.NotifyNewTransactions(false); err != nil {
		t.Fatalf("unable to request transaction notifications: %v", err)
	}

	// We start by connecting the new miner to our original miner,
	// such that it will sync to our original chain.
	if err := rpctest.ConnectNode(net.Miner, miner); err != nil {
		t.Fatalf("unable to connect harnesses: %v", err)
	}
	nodeSlice := []*rpctest.Harness{net.Miner, miner}
	if err := rpctest.JoinNodes(nodeSlice, rpctest.Blocks); err != nil {
		t.Fatalf("unable to join node on blocks: %v", err)
	}

	// The two should be on the same blockheight.
	_, newNodeHeight, err := miner.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	_, orgNodeHeight, err := net.Miner.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	if newNodeHeight != orgNodeHeight {
		t.Fatalf("expected new miner(%d) and original miner(%d) to "+
			"be on the same height", newNodeHeight, orgNodeHeight)
	}

	// We disconnect the two nodes, such that we can start mining on them
	// individually without the other one learning about the new blocks.
	err = net.Miner.Node.AddNode(miner.P2PAddress(), rpcclient.ANRemove)
	if err != nil {
		t.Fatalf("unable to remove node: %v", err)
	}

	// Create a new channel that requires 1 confs before it's considered
	// open, then broadcast the funding transaction
	chanAmt := maxBtcFundingAmount
	pushAmt := btcutil.Amount(0)
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	pendingUpdate, err := net.OpenPendingChannel(ctxt, net.Alice, net.Bob,
		chanAmt, pushAmt)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// At this point, the channel's funding transaction will have been
	// broadcast, but not confirmed, and the channel should be pending.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	assertNumOpenChannelsPending(ctxt, t, net.Alice, net.Bob, 1)

	fundingTxID, err := chainhash.NewHash(pendingUpdate.Txid)
	if err != nil {
		t.Fatalf("unable to convert funding txid into chainhash.Hash:"+
			" %v", err)
	}

	// We now cause a fork, by letting our original miner mine 10 blocks,
	// and our new miner mine 15. This will also confirm our pending
	// channel, which should be considered open.
	block := mineBlocks(t, net, 10)[0]
	assertTxInBlock(t, block, fundingTxID)
	miner.Node.Generate(15)

	// Ensure the chain lengths are what we expect.
	_, newNodeHeight, err = miner.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	_, orgNodeHeight, err = net.Miner.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	if newNodeHeight != orgNodeHeight+5 {
		t.Fatalf("expected new miner(%d) to be 5 blocks ahead of "+
			"original miner(%d)", newNodeHeight, orgNodeHeight)
	}

	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: pendingUpdate.Txid,
		},
		OutputIndex: pendingUpdate.OutputIndex,
	}

	// Ensure channel is no longer pending.
	assertNumOpenChannelsPending(ctxt, t, net.Alice, net.Bob, 0)

	// Wait for Alice and Bob to recognize and advertise the new channel
	// generated above.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't advertise channel before "+
			"timeout: %v", err)
	}

	// Alice should now have 1 edge in her graph.
	req := &lnrpc.ChannelGraphRequest{}
	chanGraph, err := net.Alice.DescribeGraph(ctxb, req)
	if err != nil {
		t.Fatalf("unable to query for alice's routing table: %v", err)
	}

	numEdges := len(chanGraph.Edges)
	if numEdges != 1 {
		t.Fatalf("expected to find one edge in the graph, found %d",
			numEdges)
	}

	// Connecting the two miners should now cause our original one to sync
	// to the new, and longer chain.
	if err := rpctest.ConnectNode(net.Miner, miner); err != nil {
		t.Fatalf("unable to connect harnesses: %v", err)
	}

	if err := rpctest.JoinNodes(nodeSlice, rpctest.Blocks); err != nil {
		t.Fatalf("unable to join node on blocks: %v", err)
	}

	// Once again they should be on the same chain.
	_, newNodeHeight, err = miner.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	_, orgNodeHeight, err = net.Miner.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	if newNodeHeight != orgNodeHeight {
		t.Fatalf("expected new miner(%d) and original miner(%d) to "+
			"be on the same height", newNodeHeight, orgNodeHeight)
	}

	time.Sleep(time.Second * 2)

	// Since the fundingtx was reorged out, Alice should now have no edges
	// in her graph.
	req = &lnrpc.ChannelGraphRequest{}
	chanGraph, err = net.Alice.DescribeGraph(ctxb, req)
	if err != nil {
		t.Fatalf("unable to query for alice's routing table: %v", err)
	}

	numEdges = len(chanGraph.Edges)
	if numEdges != 0 {
		t.Fatalf("expected to find no edge in the graph, found %d",
			numEdges)
	}

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

// testDisconnectingTargetPeer performs a test which
// disconnects Alice-peer from Bob-peer and then re-connects them again
func testDisconnectingTargetPeer(net *lntest.NetworkHarness, t *harnessTest) {

	ctxb := context.Background()

	// Check existing connection.
	assertNumConnections(ctxb, t, net.Alice, net.Bob, 1)

	chanAmt := maxBtcFundingAmount
	pushAmt := btcutil.Amount(0)

	timeout := time.Duration(time.Second * 10)
	ctxt, _ := context.WithTimeout(ctxb, timeout)

	// Create a new channel that requires 1 confs before it's considered
	// open, then broadcast the funding transaction
	const numConfs = 1
	pendingUpdate, err := net.OpenPendingChannel(ctxt, net.Alice, net.Bob,
		chanAmt, pushAmt)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// At this point, the channel's funding transaction will have
	// been broadcast, but not confirmed. Alice and Bob's nodes
	// should reflect this when queried via RPC.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	assertNumOpenChannelsPending(ctxt, t, net.Alice, net.Bob, 1)

	// Disconnect Alice-peer from Bob-peer and get error
	// causes by one pending channel with detach node is existing.
	if err := net.DisconnectNodes(ctxt, net.Alice, net.Bob); err == nil {
		t.Fatalf("Bob's peer was disconnected from Alice's"+
			" while one pending channel is existing: err %v", err)
	}

	time.Sleep(time.Millisecond * 300)

	// Check existing connection.
	assertNumConnections(ctxb, t, net.Alice, net.Bob, 1)

	fundingTxID, err := chainhash.NewHash(pendingUpdate.Txid)
	if err != nil {
		t.Fatalf("unable to convert funding txid into chainhash.Hash:"+
			" %v", err)
	}

	// Mine a block, then wait for Alice's node to notify us that the
	// channel has been opened. The funding transaction should be found
	// within the newly mined block.
	block := mineBlocks(t, net, numConfs)[0]
	assertTxInBlock(t, block, fundingTxID)

	// At this point, the channel should be fully opened and there should
	// be no pending channels remaining for either node.
	time.Sleep(time.Millisecond * 300)
	ctxt, _ = context.WithTimeout(ctxb, timeout)

	assertNumOpenChannelsPending(ctxt, t, net.Alice, net.Bob, 0)

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
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: pendingUpdate.Txid,
		},
		OutputIndex: pendingUpdate.OutputIndex,
	}

	// Disconnect Alice-peer from Bob-peer and get error
	// causes by one active channel with detach node is existing.
	if err := net.DisconnectNodes(ctxt, net.Alice, net.Bob); err == nil {
		t.Fatalf("Bob's peer was disconnected from Alice's"+
			" while one active channel is existing: err %v", err)
	}

	// Check existing connection.
	assertNumConnections(ctxb, t, net.Alice, net.Bob, 1)

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, true)

	// Disconnect Alice-peer from Bob-peer without getting error
	// about existing channels.
	var predErr error
	err = lntest.WaitPredicate(func() bool {
		if err := net.DisconnectNodes(ctxt, net.Alice, net.Bob); err != nil {
			predErr = err
			return false
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("unable to disconnect Bob's peer from Alice's: err %v",
			predErr)
	}

	// Check zero peer connections.
	assertNumConnections(ctxb, t, net.Alice, net.Bob, 0)

	// Finally, re-connect both nodes.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.ConnectNodes(ctxt, net.Alice, net.Bob); err != nil {
		t.Fatalf("unable to connect Alice's peer to Bob's: err %v", err)
	}

	// Check existing connection.
	assertNumConnections(ctxb, t, net.Alice, net.Bob, 1)

	// Mine enough blocks to clear the force closed outputs from the UTXO
	// nursery.
	if _, err := net.Miner.Node.Generate(4); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
}

// testFundingPersistence is intended to ensure that the Funding Manager
// persists the state of new channels prior to broadcasting the channel's
// funding transaction. This ensures that the daemon maintains an up-to-date
// representation of channels if the system is restarted or disconnected.
// testFundingPersistence mirrors testBasicChannelFunding, but adds restarts
// and checks for the state of channels with unconfirmed funding transactions.
func testChannelFundingPersistence(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	chanAmt := maxBtcFundingAmount
	pushAmt := btcutil.Amount(0)

	timeout := time.Duration(time.Second * 10)

	// As we need to create a channel that requires more than 1
	// confirmation before it's open, with the current set of defaults,
	// we'll need to create a new node instance.
	const numConfs = 5
	carolArgs := []string{fmt.Sprintf("--bitcoin.defaultchanconfs=%v", numConfs)}
	carol, err := net.NewNode("Carol", carolArgs)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}

	// Clean up carol's node when the test finishes.
	defer shutdownAndAssert(net, t, carol)

	ctxt, _ := context.WithTimeout(ctxb, timeout)
	if err := net.ConnectNodes(ctxt, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect alice to carol: %v", err)
	}

	// Create a new channel that requires 5 confs before it's considered
	// open, then broadcast the funding transaction
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	pendingUpdate, err := net.OpenPendingChannel(ctxt, net.Alice, carol,
		chanAmt, pushAmt)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// At this point, the channel's funding transaction will have been
	// broadcast, but not confirmed. Alice and Bob's nodes should reflect
	// this when queried via RPC.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	assertNumOpenChannelsPending(ctxt, t, net.Alice, carol, 1)

	// Restart both nodes to test that the appropriate state has been
	// persisted and that both nodes recover gracefully.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}
	if err := net.RestartNode(carol, nil); err != nil {
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
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// The following block ensures that after both nodes have restarted,
	// they have reconnected before the execution of the next test.
	if err := net.EnsureConnected(ctxb, net.Alice, carol); err != nil {
		t.Fatalf("peers unable to reconnect after restart: %v", err)
	}

	// Next, mine enough blocks s.t the channel will open with a single
	// additional block mined.
	if _, err := net.Miner.Node.Generate(3); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	// Both nodes should still show a single channel as pending.
	time.Sleep(time.Second * 1)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	assertNumOpenChannelsPending(ctxt, t, net.Alice, carol, 1)

	// Finally, mine the last block which should mark the channel as open.
	if _, err := net.Miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	// At this point, the channel should be fully opened and there should
	// be no pending channels remaining for either node.
	time.Sleep(time.Second * 1)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	assertNumOpenChannelsPending(ctxt, t, net.Alice, carol, 0)

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
	if err := net.AssertChannelExists(ctxt, carol, &outPoint); err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: pendingUpdate.Txid,
		},
		OutputIndex: pendingUpdate.OutputIndex,
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

// testChannelBalance creates a new channel between Alice and  Bob, then
// checks channel balance to be equal amount specified while creation of channel.
func testChannelBalance(net *lntest.NetworkHarness, t *harnessTest) {
	timeout := time.Duration(time.Second * 5)

	// Open a channel with 0.16 BTC between Alice and Bob, ensuring the
	// channel has been opened properly.
	amount := maxBtcFundingAmount
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

	// Before beginning, make sure alice and bob are connected.
	if err := net.EnsureConnected(ctx, net.Alice, net.Bob); err != nil {
		t.Fatalf("unable to connect alice and bob: %v", err)
	}

	chanPoint := openChannelAndAssert(
		ctx, t, net, net.Alice, net.Bob, amount, 0, false,
	)

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
	checkChannelBalance(net.Alice, amount-calcStaticFee(0))

	// Ensure Bob currently has no available balance within the channel.
	checkChannelBalance(net.Bob, 0)

	// Finally close the channel between Alice and Bob, asserting that the
	// channel has been properly closed on-chain.
	ctx, _ = context.WithTimeout(context.Background(), timeout)
	closeChannelAndAssert(ctx, t, net, net.Alice, chanPoint, false)
}

// findForceClosedChannel searches a pending channel response for a particular
// channel, returning the force closed channel upon success.
func findForceClosedChannel(t *harnessTest,
	pendingChanResp *lnrpc.PendingChannelsResponse,
	op *wire.OutPoint) *lnrpc.PendingChannelsResponse_ForceClosedChannel {

	var found bool
	var forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel
	for _, forceClose = range pendingChanResp.PendingForceClosingChannels {
		if forceClose.Channel.ChannelPoint == op.String() {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("channel not marked as force closed")
	}

	return forceClose
}

// findWaitingCloseChannel searches a pending channel response for a particular
// channel, returning the waiting close channel upon success.
func findWaitingCloseChannel(t *harnessTest,
	pendingChanResp *lnrpc.PendingChannelsResponse,
	op *wire.OutPoint) *lnrpc.PendingChannelsResponse_WaitingCloseChannel {

	var found bool
	var waitingClose *lnrpc.PendingChannelsResponse_WaitingCloseChannel
	for _, waitingClose = range pendingChanResp.WaitingCloseChannels {
		if waitingClose.Channel.ChannelPoint == op.String() {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("channel not marked as waiting close")
	}

	return waitingClose
}

func assertCommitmentMaturity(t *harnessTest,
	forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel,
	maturityHeight uint32, blocksTilMaturity int32) {

	if forceClose.MaturityHeight != maturityHeight {
		t.Fatalf("expected commitment maturity height to be %d, "+
			"found %d instead", maturityHeight,
			forceClose.MaturityHeight)
	}
	if forceClose.BlocksTilMaturity != blocksTilMaturity {
		t.Fatalf("expected commitment blocks til maturity to be %d, "+
			"found %d instead", blocksTilMaturity,
			forceClose.BlocksTilMaturity)
	}
}

// assertForceClosedChannelNumHtlcs verifies that a force closed channel has the
// proper number of htlcs.
func assertPendingChannelNumHtlcs(t *harnessTest,
	forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel,
	expectedNumHtlcs int) {

	if len(forceClose.PendingHtlcs) != expectedNumHtlcs {
		t.Fatalf("expected force closed channel to have %d pending "+
			"htlcs, found %d instead", expectedNumHtlcs,
			len(forceClose.PendingHtlcs))
	}
}

// assertNumForceClosedChannels checks that a pending channel response has the
// expected number of force closed channels.
func assertNumForceClosedChannels(t *harnessTest,
	pendingChanResp *lnrpc.PendingChannelsResponse, expectedNumChans int) {

	if len(pendingChanResp.PendingForceClosingChannels) != expectedNumChans {
		t.Fatalf("expected to find %d force closed channels, got %d",
			expectedNumChans,
			len(pendingChanResp.PendingForceClosingChannels))
	}
}

// assertNumWaitingCloseChannels checks that a pending channel response has the
// expected number of channels waiting for closing tx to confirm.
func assertNumWaitingCloseChannels(t *harnessTest,
	pendingChanResp *lnrpc.PendingChannelsResponse, expectedNumChans int) {

	if len(pendingChanResp.WaitingCloseChannels) != expectedNumChans {
		t.Fatalf("expected to find %d channels waiting closure, got %d",
			expectedNumChans,
			len(pendingChanResp.WaitingCloseChannels))
	}
}

// assertPendingHtlcStageAndMaturity uniformly tests all pending htlc's
// belonging to a force closed channel, testing for the expected stage number,
// blocks till maturity, and the maturity height.
func assertPendingHtlcStageAndMaturity(t *harnessTest,
	forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel,
	stage, maturityHeight uint32, blocksTillMaturity int32) {

	for _, pendingHtlc := range forceClose.PendingHtlcs {
		if pendingHtlc.Stage != stage {
			t.Fatalf("expected pending htlc to be stage %d, "+
				"found %d", stage, pendingHtlc.Stage)
		}
		if pendingHtlc.MaturityHeight != maturityHeight {
			t.Fatalf("expected pending htlc maturity height to be "+
				"%d, instead has %d", maturityHeight,
				pendingHtlc.MaturityHeight)
		}
		if pendingHtlc.BlocksTilMaturity != blocksTillMaturity {
			t.Fatalf("expected pending htlc blocks til maturity "+
				"to be %d, instead has %d", blocksTillMaturity,
				pendingHtlc.BlocksTilMaturity)
		}
	}
}

// testChannelForceClosure performs a test to exercise the behavior of "force"
// closing a channel or unilaterally broadcasting the latest local commitment
// state on-chain. The test creates a new channel between Alice and Carol, then
// force closes the channel after some cursory assertions. Within the test, a
// total of 3 + n transactions will be broadcast, representing the commitment
// transaction, a transaction sweeping the local CSV delayed output, a
// transaction sweeping the CSV delayed 2nd-layer htlcs outputs, and n
// htlc success transactions, where n is the number of payments Alice attempted
// to send to Carol.  This test includes several restarts to ensure that the
// transaction output states are persisted throughout the forced closure
// process.
//
// TODO(roasbeef): also add an unsettled HTLC before force closing.
func testChannelForceClosure(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	const (
		timeout     = time.Duration(time.Second * 10)
		chanAmt     = btcutil.Amount(10e6)
		pushAmt     = btcutil.Amount(5e6)
		paymentAmt  = 100000
		numInvoices = 6
	)

	// TODO(roasbeef): should check default value in config here
	// instead, or make delay a param
	defaultCSV := uint32(4)
	defaultCLTV := uint32(defaultBitcoinTimeLockDelta)

	// Since we'd like to test failure scenarios with outstanding htlcs,
	// we'll introduce another node into our test network: Carol.
	carol, err := net.NewNode("Carol", []string{"--debughtlc", "--hodl.exit-settle"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	// We must let Alice have an open channel before she can send a node
	// announcement, so we open a channel with Carol,
	if err := net.ConnectNodes(ctxb, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect alice to carol: %v", err)
	}

	// Before we start, obtain Carol's current wallet balance, we'll check
	// to ensure that at the end of the force closure by Alice, Carol
	// recognizes his new on-chain output.
	carolBalReq := &lnrpc.WalletBalanceRequest{}
	carolBalResp, err := carol.WalletBalance(ctxb, carolBalReq)
	if err != nil {
		t.Fatalf("unable to get carol's balance: %v", err)
	}

	carolStartingBalance := carolBalResp.ConfirmedBalance

	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, carol, chanAmt, pushAmt, false,
	)

	// Wait for Alice and Carol to receive the channel edge from the
	// funding manager.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't see the alice->carol channel before "+
			"timeout: %v", err)
	}
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't see the alice->carol channel before "+
			"timeout: %v", err)
	}

	// Send payments from Alice to Carol, since Carol is htlchodl mode, the
	// htlc outputs should be left unsettled, and should be swept by the
	// utxo nursery.
	alicePayStream, err := net.Alice.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}
	carolPubKey := carol.PubKey[:]
	payHash := bytes.Repeat([]byte{2}, 32)
	for i := 0; i < numInvoices; i++ {
		err = alicePayStream.Send(&lnrpc.SendRequest{
			Dest:           carolPubKey,
			Amt:            int64(paymentAmt),
			PaymentHash:    payHash,
			FinalCltvDelta: defaultBitcoinTimeLockDelta,
		})
		if err != nil {
			t.Fatalf("unable to send alice htlc: %v", err)
		}
	}

	// Once the HTLC has cleared, all the nodes n our mini network should
	// show that the HTLC has been locked in.
	nodes := []*lntest.HarnessNode{net.Alice, carol}
	err = lntest.WaitPredicate(func() bool {
		return assertNumActiveHtlcs(nodes, numInvoices)
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", err)
	}

	// As we'll be querying the state of Carol's channels frequently we'll
	// create a closure helper function for the purpose.
	getAliceChanInfo := func() (*lnrpc.Channel, error) {
		req := &lnrpc.ListChannelsRequest{}
		aliceChannelInfo, err := net.Alice.ListChannels(ctxb, req)
		if err != nil {
			return nil, err
		}
		if len(aliceChannelInfo.Channels) != 1 {
			t.Fatalf("alice should only have a single channel, "+
				"instead he has %v",
				len(aliceChannelInfo.Channels))
		}

		return aliceChannelInfo.Channels[0], nil
	}

	// Fetch starting height of this test so we can compute the block
	// heights we expect certain events to take place.
	_, curHeight, err := net.Miner.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get best block height")
	}

	// Using the current height of the chain, derive the relevant heights
	// for incubating two-stage htlcs.
	var (
		startHeight           = uint32(curHeight)
		commCsvMaturityHeight = startHeight + 1 + defaultCSV
		htlcExpiryHeight      = startHeight + defaultCLTV
		htlcCsvMaturityHeight = startHeight + defaultCLTV + 1 + defaultCSV
	)

	time.Sleep(200 * time.Millisecond)

	aliceChan, err := getAliceChanInfo()
	if err != nil {
		t.Fatalf("unable to get alice's channel info: %v", err)
	}
	if aliceChan.NumUpdates == 0 {
		t.Fatalf("alice should see at least one update to her channel")
	}

	// Now that the channel is open and we have unsettled htlcs, immediately
	// execute a force closure of the channel. This will also assert that
	// the commitment transaction was immediately broadcast in order to
	// fulfill the force closure request.
	_, closingTxID, err := net.CloseChannel(ctxb, net.Alice, chanPoint, true)
	if err != nil {
		t.Fatalf("unable to execute force channel closure: %v", err)
	}

	// Now that the channel has been force closed, it should show up in the
	// PendingChannels RPC under the waiting close section.
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	pendingChanResp, err := net.Alice.PendingChannels(ctxb, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	assertNumWaitingCloseChannels(t, pendingChanResp, 1)

	// Compute the outpoint of the channel, which we will use repeatedly to
	// locate the pending channel information in the rpc responses.
	txidHash, err := getChanPointFundingTxid(chanPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	txid, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	op := wire.OutPoint{
		Hash:  *txid,
		Index: chanPoint.OutputIndex,
	}

	waitingClose := findWaitingCloseChannel(t, pendingChanResp, &op)

	// Immediately after force closing, all of the funds should be in limbo.
	if waitingClose.LimboBalance == 0 {
		t.Fatalf("all funds should still be in limbo")
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

	// Now that the commitment has been confirmed, the channel should be
	// marked as force closed.
	pendingChanResp, err = net.Alice.PendingChannels(ctxb, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	assertNumForceClosedChannels(t, pendingChanResp, 1)

	forceClose := findForceClosedChannel(t, pendingChanResp, &op)

	// Now that the channel has been force closed, it should now have the
	// height and number of blocks to confirm populated.
	assertCommitmentMaturity(t, forceClose, commCsvMaturityHeight,
		int32(defaultCSV))

	// None of our outputs have been swept, so they should all be limbo.
	if forceClose.LimboBalance == 0 {
		t.Fatalf("all funds should still be in limbo")
	}
	if forceClose.RecoveredBalance != 0 {
		t.Fatalf("no funds should yet be shown as recovered")
	}

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
	if _, err := net.Miner.Node.Generate(defaultCSV - 1); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	// The following restart checks to ensure that outputs in the
	// kindergarten bucket are persisted while waiting for the required
	// number of confirmations to be reported.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	pendingChanResp, err = net.Alice.PendingChannels(ctxb, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	assertNumForceClosedChannels(t, pendingChanResp, 1)

	forceClose = findForceClosedChannel(t, pendingChanResp, &op)

	// At this point, the nursery should show that the commitment output has
	// 1 block left before its CSV delay expires. In total, we have mined
	// exactly defaultCSV blocks, so the htlc outputs should also reflect
	// that this many blocks have passed.
	assertCommitmentMaturity(t, forceClose, commCsvMaturityHeight, 1)

	// All funds should still be shown in limbo.
	if forceClose.LimboBalance == 0 {
		t.Fatalf("all funds should still be in limbo")
	}
	if forceClose.RecoveredBalance != 0 {
		t.Fatalf("no funds should yet be shown as recovered")
	}

	// Generate an additional block, which should cause the CSV delayed
	// output from the commitment txn to expire.
	if _, err := net.Miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	// At this point, the sweeping transaction should now be broadcast. So
	// we fetch the node's mempool to ensure it has been properly
	// broadcast.
	sweepingTXID, err := waitForTxInMempool(net.Miner.Node, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to get sweep tx from mempool: %v", err)
	}

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

	// Restart Alice to ensure that she resumes watching the finalized
	// commitment sweep txid.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Next, we mine an additional block which should include the sweep
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

	// We sleep here to ensure that Alice has enough time to receive a
	// confirmation for the commitment transaction, which we already
	// asserted was in the last block.
	time.Sleep(300 * time.Millisecond)

	// Now that the commit output has been fully swept, check to see that
	// the channel remains open for the pending htlc outputs.
	pendingChanResp, err = net.Alice.PendingChannels(ctxb, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	assertNumForceClosedChannels(t, pendingChanResp, 1)

	// The htlc funds will still be shown as limbo, since they are still in
	// their first stage. The commitment funds will have been recovered
	// after the commit txn was included in the last block.
	if forceClose.LimboBalance == 0 {
		t.Fatalf("htlc funds should still be in limbo")
	}

	// Compute the height preceding that which will cause the htlc CLTV
	// timeouts will expire. The outputs entered at the same height as the
	// output spending from the commitment txn, so we must deduct the number
	// of blocks we have generated since adding it to the nursery, and take
	// an additional block off so that we end up one block shy of the expiry
	// height.
	cltvHeightDelta := defaultCLTV - defaultCSV - 2 - 1

	// Advance the blockchain until just before the CLTV expires, nothing
	// exciting should have happened during this time.
	blockHash, err = net.Miner.Node.Generate(cltvHeightDelta)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	time.Sleep(duration)

	// We now restart Alice, to ensure that she will broadcast the presigned
	// htlc timeout txns after the delay expires after experiencing a while
	// waiting for the htlc outputs to incubate.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}
	time.Sleep(duration)

	pendingChanResp, err = net.Alice.PendingChannels(ctxb, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	assertNumForceClosedChannels(t, pendingChanResp, 1)

	forceClose = findForceClosedChannel(t, pendingChanResp, &op)

	// We should now be at the block just before the utxo nursery will
	// attempt to broadcast the htlc timeout transactions.
	assertPendingChannelNumHtlcs(t, forceClose, numInvoices)
	assertPendingHtlcStageAndMaturity(t, forceClose, 1, htlcExpiryHeight, 1)

	// Now that our commitment confirmation depth has been surpassed, we
	// should now see a non-zero recovered balance. All htlc outputs are
	// still left in limbo, so it should be non-zero as well.
	if forceClose.LimboBalance == 0 {
		t.Fatalf("htlc funds should still be in limbo")
	}

	// Now, generate the block which will cause Alice to broadcast the
	// presigned htlc timeout txns.
	blockHash, err = net.Miner.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// Since Alice had numInvoices (6) htlcs extended to Carol before force
	// closing, we expect Alice to broadcast an htlc timeout txn for each
	// one. Wait for them all to show up in the mempool.
	htlcTxIDs, err := waitForNTxsInMempool(net.Miner.Node, numInvoices,
		10*time.Second)
	if err != nil {
		t.Fatalf("unable to find htlc timeout txns in mempool: %v", err)
	}

	// Retrieve each htlc timeout txn from the mempool, and ensure it is
	// well-formed. This entails verifying that each only spends from
	// output, and that that output is from the commitment txn.
	for _, htlcTxID := range htlcTxIDs {
		// Fetch the sweep transaction, all input it's spending should
		// be from the commitment transaction which was broadcast
		// on-chain.
		htlcTx, err := net.Miner.Node.GetRawTransaction(htlcTxID)
		if err != nil {
			t.Fatalf("unable to fetch sweep tx: %v", err)
		}
		// Ensure the htlc transaction only has one input.
		if len(htlcTx.MsgTx().TxIn) != 1 {
			t.Fatalf("htlc transaction should only have one txin, "+
				"has %d", len(htlcTx.MsgTx().TxIn))
		}
		// Ensure the htlc transaction is spending from the commitment
		// transaction.
		txIn := htlcTx.MsgTx().TxIn[0]
		if !closingTxID.IsEqual(&txIn.PreviousOutPoint.Hash) {
			t.Fatalf("htlc transaction not spending from commit "+
				"tx %v, instead spending %v",
				closingTxID, txIn.PreviousOutPoint)
		}
	}

	// With the htlc timeout txns still in the mempool, we restart Alice to
	// verify that she can resume watching the htlc txns she broadcasted
	// before crashing.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}
	time.Sleep(duration)

	// Generate a block that mines the htlc timeout txns. Doing so now
	// activates the 2nd-stage CSV delayed outputs.
	blockHash, err = net.Miner.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	// This sleep gives Alice enough to time move the crib outputs into the
	// kindergarten bucket.
	time.Sleep(duration)

	// Alice is restarted here to ensure that she promptly moved the crib
	// outputs to the kindergarten bucket after the htlc timeout txns were
	// confirmed.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Advance the chain until just before the 2nd-layer CSV delays expire.
	blockHash, err = net.Miner.Node.Generate(defaultCSV - 1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// Restart Alice to ensure that she can recover from a failure before
	// having graduated the htlc outputs in the kindergarten bucket.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Now that the channel has been fully swept, it should no longer show
	// incubated, check to see that Alice's node still reports the channel
	// as pending force closed.
	pendingChanResp, err = net.Alice.PendingChannels(ctxb, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	assertNumForceClosedChannels(t, pendingChanResp, 1)

	forceClose = findForceClosedChannel(t, pendingChanResp, &op)

	if forceClose.LimboBalance == 0 {
		t.Fatalf("htlc funds should still be in limbo")
	}

	assertPendingChannelNumHtlcs(t, forceClose, numInvoices)

	// Generate a block that causes Alice to sweep the htlc outputs in the
	// kindergarten bucket.
	blockHash, err = net.Miner.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// Wait for the single sweep txn to appear in the mempool.
	htlcSweepTxID, err := waitForTxInMempool(net.Miner.Node, 15*time.Second)
	if err != nil {
		t.Fatalf("failed to get sweep tx from mempool: %v", err)
	}

	// Construct a map of the already confirmed htlc timeout txids, that
	// will count the number of times each is spent by the sweep txn. We
	// prepopulate it in this way so that we can later detect if we are
	// spending from an output that was not a confirmed htlc timeout txn.
	var htlcTxIDSet = make(map[chainhash.Hash]int)
	for _, htlcTxID := range htlcTxIDs {
		htlcTxIDSet[*htlcTxID] = 0
	}

	// Fetch the htlc sweep transaction from the mempool.
	htlcSweepTx, err := net.Miner.Node.GetRawTransaction(htlcSweepTxID)
	if err != nil {
		t.Fatalf("unable to fetch sweep tx: %v", err)
	}
	// Ensure the htlc sweep transaction only has one input for each htlc
	// Alice extended before force closing.
	if len(htlcSweepTx.MsgTx().TxIn) != numInvoices {
		t.Fatalf("htlc transaction should have %d txin, "+
			"has %d", numInvoices, len(htlcSweepTx.MsgTx().TxIn))
	}
	// Ensure that each output spends from exactly one htlc timeout txn.
	for _, txIn := range htlcSweepTx.MsgTx().TxIn {
		outpoint := txIn.PreviousOutPoint.Hash
		// Check that the input is a confirmed htlc timeout txn.
		if _, ok := htlcTxIDSet[outpoint]; !ok {
			t.Fatalf("htlc sweep output not spending from htlc "+
				"tx, instead spending output %v", outpoint)
		}
		// Increment our count for how many times this output was spent.
		htlcTxIDSet[outpoint]++

		// Check that each is only spent once.
		if htlcTxIDSet[outpoint] > 1 {
			t.Fatalf("htlc sweep tx has multiple spends from "+
				"outpoint %v", outpoint)
		}
	}

	// The following restart checks to ensure that the nursery store is
	// storing the txid of the previously broadcast htlc sweep txn, and that
	// it begins watching that txid after restarting.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}
	time.Sleep(duration)

	// Now that the channel has been fully swept, it should no longer show
	// incubated, check to see that Alice's node still reports the channel
	// as pending force closed.
	pendingChanResp, err = net.Alice.PendingChannels(ctxb, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	assertNumForceClosedChannels(t, pendingChanResp, 1)

	// All htlcs should show zero blocks until maturity, as evidenced by
	// having checked the sweep transaction in the mempool.
	forceClose = findForceClosedChannel(t, pendingChanResp, &op)
	assertPendingChannelNumHtlcs(t, forceClose, numInvoices)
	assertPendingHtlcStageAndMaturity(t, forceClose, 2,
		htlcCsvMaturityHeight, 0)

	// Generate the final block that sweeps all htlc funds into the user's
	// wallet.
	blockHash, err = net.Miner.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	time.Sleep(3 * duration)

	// Now that the channel has been fully swept, it should no longer show
	// up within the pending channels RPC.
	pendingChanResp, err = net.Alice.PendingChannels(ctxb, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	assertNumForceClosedChannels(t, pendingChanResp, 0)

	// In addition to there being no pending channels, we verify that
	// pending channels does not report any money still in limbo.
	if pendingChanResp.TotalLimboBalance != 0 {
		t.Fatalf("no user funds should be left in limbo after incubation")
	}

	// At this point, Carol should now be aware of his new immediately
	// spendable on-chain balance, as it was Alice who broadcast the
	// commitment transaction.
	carolBalResp, err = net.Bob.WalletBalance(ctxb, carolBalReq)
	if err != nil {
		t.Fatalf("unable to get carol's balance: %v", err)
	}
	carolExpectedBalance := btcutil.Amount(carolStartingBalance) + pushAmt
	if btcutil.Amount(carolBalResp.ConfirmedBalance) < carolExpectedBalance {
		t.Fatalf("carol's balance is incorrect: expected %v got %v",
			carolExpectedBalance,
			carolBalResp.ConfirmedBalance)
	}
}

// testSphinxReplayPersistence verifies that replayed onion packets are rejected
// by a remote peer after a restart. We use a combination of unsafe
// configuration arguments to force Carol to replay the same sphinx packet after
// reconnecting to Dave, and compare the returned failure message with what we
// expect for replayed onion packets.
func testSphinxReplayPersistence(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 5)

	// Open a channel with 100k satoshis between Carol and Dave with Carol being
	// the sole funder of the channel.
	chanAmt := btcutil.Amount(100000)

	// First, we'll create Dave, the receiver, and start him in hodl mode.
	dave, err := net.NewNode("Dave", []string{"--debughtlc", "--hodl.exit-settle"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}

	// We must remember to shutdown the nodes we created for the duration
	// of the tests, only leaving the two seed nodes (Alice and Bob) within
	// our test network.
	defer shutdownAndAssert(net, t, dave)

	// Next, we'll create Carol and establish a channel to from her to
	// Dave. Carol is started in both unsafe-replay and unsafe-disconnect,
	// which will cause her to replay any pending Adds held in memory upon
	// reconnection.
	carol, err := net.NewNode("Carol", []string{"--unsafe-replay"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	if err := net.ConnectNodes(ctxb, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, carol, dave, chanAmt, 0, false,
	)

	assertAmountSent := func(amt btcutil.Amount) {
		// Both channels should also have properly accounted from the
		// amount that has been sent/received over the channel.
		listReq := &lnrpc.ListChannelsRequest{}
		carolListChannels, err := carol.ListChannels(ctxb, listReq)
		if err != nil {
			t.Fatalf("unable to query for alice's channel list: %v", err)
		}
		carolSatoshisSent := carolListChannels.Channels[0].TotalSatoshisSent
		if carolSatoshisSent != int64(amt) {
			t.Fatalf("Carol's satoshis sent is incorrect got %v, expected %v",
				carolSatoshisSent, amt)
		}

		daveListChannels, err := dave.ListChannels(ctxb, listReq)
		if err != nil {
			t.Fatalf("unable to query for Dave's channel list: %v", err)
		}
		daveSatoshisReceived := daveListChannels.Channels[0].TotalSatoshisReceived
		if daveSatoshisReceived != int64(amt) {
			t.Fatalf("Dave's satoshis received is incorrect got %v, expected %v",
				daveSatoshisReceived, amt)
		}
	}

	// Now that the channel is open, create an invoice for Dave which
	// expects a payment of 1000 satoshis from Carol paid via a particular
	// preimage.
	const paymentAmt = 1000
	preimage := bytes.Repeat([]byte("A"), 32)
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: preimage,
		Value:     paymentAmt,
	}
	invoiceResp, err := dave.AddInvoice(ctxb, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Wait for Carol to recognize and advertise the new channel generated
	// above.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
	}
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't advertise channel before "+
			"timeout: %v", err)
	}

	// With the invoice for Dave added, send a payment from Carol paying
	// to the above generated invoice.
	ctx, cancel := context.WithCancel(ctxb)
	defer cancel()

	payStream, err := carol.SendPayment(ctx)
	if err != nil {
		t.Fatalf("unable to open payment stream: %v", err)
	}

	sendReq := &lnrpc.SendRequest{PaymentRequest: invoiceResp.PaymentRequest}
	err = payStream.Send(sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// Dave's invoice should not be marked as settled.
	payHash := &lnrpc.PaymentHash{
		RHash: invoiceResp.RHash,
	}
	dbInvoice, err := dave.LookupInvoice(ctxb, payHash)
	if err != nil {
		t.Fatalf("unable to lookup invoice: %v", err)
	}
	if dbInvoice.Settled {
		t.Fatalf("dave's invoice should not be marked as settled: %v",
			spew.Sdump(dbInvoice))
	}

	// With the payment sent but hedl, all balance related stats should not
	// have changed.
	time.Sleep(time.Millisecond * 200)
	assertAmountSent(0)

	// With the first payment sent, restart dave to make sure he is
	// persisting the information required to detect replayed sphinx
	// packets.
	if err := net.RestartNode(dave, nil); err != nil {
		t.Fatalf("unable to restart dave: %v", err)
	}

	// Carol should retransmit the Add hedl in her mailbox on startup. Dave
	// should not accept the replayed Add, and actually fail back the
	// pending payment. Even though he still holds the original settle, if
	// he does fail, it is almost certainly caused by the sphinx replay
	// protection, as it is the only validation we do in hodl mode.
	resp, err := payStream.Recv()
	if err != nil {
		t.Fatalf("unable to receive payment response: %v", err)
	}

	// Construct the response we expect after sending a duplicate packet
	// that fails due to sphinx replay detection.
	replayErr := fmt.Sprintf("unable to route payment to destination: "+
		"TemporaryChannelFailure: unable to de-obfuscate onion failure, "+
		"htlc with hash(%x): unable to retrieve onion failure",
		invoiceResp.RHash)

	if resp.PaymentError != replayErr {
		t.Fatalf("received payment error: %v", resp.PaymentError)
	}

	// Since the payment failed, the balance should still be left
	// unaltered.
	assertAmountSent(0)

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPoint, true)
}

func testSingleHopInvoice(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 5)

	// Open a channel with 100k satoshis between Alice and Bob with Alice being
	// the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanAmt := btcutil.Amount(100000)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, chanAmt, 0, false,
	)

	assertAmountSent := func(amt btcutil.Amount) {
		// Both channels should also have properly accounted from the
		// amount that has been sent/received over the channel.
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
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("bob didn't advertise channel before "+
			"timeout: %v", err)
	}

	// With the invoice for Bob added, send a payment towards Alice paying
	// to the above generated invoice.
	sendReq := &lnrpc.SendRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	resp, err := net.Alice.SendPaymentSync(ctxt, sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// Ensure we obtain the proper preimage in the response.
	if resp.PaymentError != "" {
		t.Fatalf("error when attempting recv: %v", resp.PaymentError)
	} else if !bytes.Equal(preimage, resp.PaymentPreimage) {
		t.Fatalf("preimage mismatch: expected %v, got %v", preimage,
			resp.GetPaymentPreimage())
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
	time.Sleep(time.Millisecond * 200)
	assertAmountSent(paymentAmt)

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
	sendReq = &lnrpc.SendRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	resp, err = net.Alice.SendPaymentSync(ctxt, sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}
	if resp.PaymentError != "" {
		t.Fatalf("error when attempting recv: %v", resp.PaymentError)
	}

	// The second payment should also have succeeded, with the balances
	// being update accordingly.
	time.Sleep(time.Millisecond * 200)
	assertAmountSent(paymentAmt * 2)

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

func testListPayments(net *lntest.NetworkHarness, t *harnessTest) {
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
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, chanAmt, 0, false,
	)

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
	if err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint); err != nil {
		t.Fatalf("alice didn't advertise channel before "+
			"timeout: %v", err)
	}
	if err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint); err != nil {
		t.Fatalf("bob didn't advertise channel before "+
			"timeout: %v", err)
	}

	// With the invoice for Bob added, send a payment towards Alice paying
	// to the above generated invoice.
	sendReq := &lnrpc.SendRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	resp, err := net.Alice.SendPaymentSync(ctxt, sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}
	if resp.PaymentError != "" {
		t.Fatalf("error when attempting recv: %v", resp.PaymentError)
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

// assertAmountPaid checks that the ListChannels command of the provided
// node list the total amount sent and received as expected for the
// provided channel.
func assertAmountPaid(t *harnessTest, ctxb context.Context, channelName string,
	node *lntest.HarnessNode, chanPoint wire.OutPoint, amountSent,
	amountReceived int64) {

	checkAmountPaid := func() error {
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

			if channel.TotalSatoshisSent != amountSent {
				return fmt.Errorf("%v: incorrect amount"+
					" sent: %v != %v", channelName,
					channel.TotalSatoshisSent,
					amountSent)
			}
			if channel.TotalSatoshisReceived !=
				amountReceived {
				return fmt.Errorf("%v: incorrect amount"+
					" received: %v != %v",
					channelName,
					channel.TotalSatoshisReceived,
					amountReceived)
			}

			return nil
		}
		return fmt.Errorf("channel not found")
	}

	// As far as HTLC inclusion in commitment transaction might be
	// postponed we will try to check the balance couple of times,
	// and then if after some period of time we receive wrong
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
		if err := checkAmountPaid(); err != nil {
			if isTimeover {
				t.Fatalf("Check amount Paid failed: %v", err)
			}
		} else {
			break
		}
	}
}

func testMultiHopPayments(net *lntest.NetworkHarness, t *harnessTest) {
	const chanAmt = btcutil.Amount(100000)
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 15)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, chanAmt, 0, false,
	)
	networkChans = append(networkChans, chanPointAlice)

	txidHash, err := getChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// As preliminary setup, we'll create two new nodes: Carol and Dave,
	// such that we now have a 4 ndoe, 3 channel topology. Dave will make
	// a channel with Alice, and Carol with Dave. After this setup, the
	// network topology should now look like:
	//     Carol -> Dave -> Alice -> Bob
	//
	// First, we'll create Dave and establish a channel to Alice.
	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	if err := net.ConnectNodes(ctxb, dave, net.Alice); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointDave := openChannelAndAssert(
		ctxt, t, net, dave, net.Alice, chanAmt, 0, false,
	)
	networkChans = append(networkChans, chanPointDave)
	txidHash, err = getChanPointFundingTxid(chanPointDave)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	daveChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
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

	if err := net.ConnectNodes(ctxb, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, carol, dave, chanAmt, 0, false,
	)
	networkChans = append(networkChans, chanPointCarol)

	txidHash, err = getChanPointFundingTxid(chanPointCarol)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
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
			txidHash, err := getChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			txid, e := chainhash.NewHash(txidHash)
			if e != nil {
				t.Fatalf("unable to create sha hash: %v", e)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ = context.WithTimeout(ctxb, timeout)
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
	payReqs := make([]string, numPayments)
	for i := 0; i < numPayments; i++ {
		invoice := &lnrpc.Invoice{
			Memo:  "testing",
			Value: paymentAmt,
		}
		resp, err := net.Bob.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		payReqs[i] = resp.PaymentRequest
	}

	// We'll wait for all parties to recognize the new channels within the
	// network.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPointDave)
	if err != nil {
		t.Fatalf("dave didn't advertise his channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointCarol)
	if err != nil {
		t.Fatalf("carol didn't advertise her channel in time: %v",
			err)
	}

	time.Sleep(time.Millisecond * 50)

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
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
	const amountPaid = int64(5000)
	assertAmountPaid(t, ctxb, "Alice(local) => Bob(remote)", net.Bob,
		aliceFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, ctxb, "Alice(local) => Bob(remote)", net.Alice,
		aliceFundPoint, amountPaid, int64(0))
	assertAmountPaid(t, ctxb, "Dave(local) => Alice(remote)", net.Alice,
		daveFundPoint, int64(0), amountPaid+(baseFee*numPayments))
	assertAmountPaid(t, ctxb, "Dave(local) => Alice(remote)", dave,
		daveFundPoint, amountPaid+(baseFee*numPayments), int64(0))
	assertAmountPaid(t, ctxb, "Carol(local) => Dave(remote)", dave,
		carolFundPoint, int64(0), amountPaid+((baseFee*numPayments)*2))
	assertAmountPaid(t, ctxb, "Carol(local) => Dave(remote)", carol,
		carolFundPoint, amountPaid+(baseFee*numPayments)*2, int64(0))

	// Now that we know all the balances have been settled out properly,
	// we'll ensure that our internal record keeping for completed circuits
	// was properly updated.

	// First, check that the FeeReport response shows the proper fees
	// accrued over each time range. Dave should've earned 1 satoshi for
	// each of the forwarded payments.
	feeReport, err := dave.FeeReport(ctxb, &lnrpc.FeeReportRequest{})
	if err != nil {
		t.Fatalf("unable to query for fee report: %v", err)
	}
	const exectedFees = 5
	if feeReport.DayFeeSum != exectedFees {
		t.Fatalf("fee mismatch: expected %v, got %v", 5,
			feeReport.DayFeeSum)
	}
	if feeReport.WeekFeeSum != exectedFees {
		t.Fatalf("fee mismatch: expected %v, got %v", 5,
			feeReport.WeekFeeSum)
	}
	if feeReport.MonthFeeSum != exectedFees {
		t.Fatalf("fee mismatch: expected %v, got %v", 5,
			feeReport.MonthFeeSum)
	}

	// Next, ensure that if we issue the vanilla query for the forwarding
	// history, it returns 5 values, and each entry is formatted properly.
	fwdingHistory, err := dave.ForwardingHistory(
		ctxb, &lnrpc.ForwardingHistoryRequest{},
	)
	if err != nil {
		t.Fatalf("unable to query for fee report: %v", err)
	}
	if len(fwdingHistory.ForwardingEvents) != 5 {
		t.Fatalf("wrong number of forwarding event: expected %v, "+
			"got %v", 5, len(fwdingHistory.ForwardingEvents))
	}
	for _, event := range fwdingHistory.ForwardingEvents {
		// Each event should show a fee of 1 satoshi.
		if event.Fee != 1 {
			t.Fatalf("fee mismatch: expected %v, got %v", 1,
				event.Fee)
		}
	}

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, dave, chanPointDave, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)
}

// testSingleHopSendToRoute tests that payments are properly processed
// through a provided route with a single hop. We'll create the
// following network topology:
//      Alice --100k--> Bob
// We'll query the daemon for routes from Alice to Bob and then
// send payments through the route.
func testSingleHopSendToRoute(net *lntest.NetworkHarness, t *harnessTest) {
	const chanAmt = btcutil.Amount(100000)
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 15)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointAlice := openChannelAndAssert(ctxt, t, net, net.Alice,
		net.Bob, chanAmt, 0, false)
	networkChans = append(networkChans, chanPointAlice)

	txidHash, err := getChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// Wait for all nodes to have seen all channels.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob}
	nodeNames := []string{"Alice", "Bob"}
	for _, chanPoint := range networkChans {
		for i, node := range nodes {
			txidHash, err := getChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			txid, e := chainhash.NewHash(txidHash)
			if e != nil {
				t.Fatalf("unable to create sha hash: %v", e)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ = context.WithTimeout(ctxb, timeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, point, err)
			}
		}
	}

	// Query for routes to pay from Alice to Bob.
	// We set FinalCltvDelta to 144 since by default QueryRoutes returns
	// the last hop with a final cltv delta of 9 where as the default in
	// htlcswitch is 144.
	const paymentAmt = 1000
	routesReq := &lnrpc.QueryRoutesRequest{
		PubKey:         net.Bob.PubKeyStr,
		Amt:            paymentAmt,
		NumRoutes:      1,
		FinalCltvDelta: 144,
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	routes, err := net.Alice.QueryRoutes(ctxt, routesReq)
	if err != nil {
		t.Fatalf("unable to get route: %v", err)
	}

	// Create 5 invoices for Bob, which expect a payment from Alice for 1k
	// satoshis with a different preimage each time.
	const numPayments = 5
	rHashes := make([][]byte, numPayments)
	for i := 0; i < numPayments; i++ {
		invoice := &lnrpc.Invoice{
			Value: paymentAmt,
		}
		resp, err := net.Bob.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		rHashes[i] = resp.RHash
	}

	// We'll wait for all parties to recognize the new channels within the
	// network.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPointAlice)
	if err != nil {
		t.Fatalf("alice didn't advertise her channel in time: %v", err)
	}

	time.Sleep(time.Millisecond * 50)

	// Using Alice as the source, pay to the 5 invoices from Carol created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	alicePayStream, err := net.Alice.SendToRoute(ctxt)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}

	for _, rHash := range rHashes {
		sendReq := &lnrpc.SendToRouteRequest{
			PaymentHash: rHash,
			Routes:      routes.Routes,
		}
		err := alicePayStream.Send(sendReq)

		if err != nil {
			t.Fatalf("unable to send payment: %v", err)
		}
	}

	for range rHashes {
		resp, err := alicePayStream.Recv()
		if err != nil {
			t.Fatalf("unable to send payment: %v", err)
		}
		if resp.PaymentError != "" {
			t.Fatalf("received payment error: %v", resp.PaymentError)
		}
	}

	// At this point all the channels within our proto network should be
	// shifted by 5k satoshis in the direction of Bob, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Alice->Bob, order is Bob and then Alice.
	const amountPaid = int64(5000)
	assertAmountPaid(t, ctxb, "Alice(local) => Bob(remote)", net.Bob,
		aliceFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, ctxb, "Alice(local) => Bob(remote)", net.Alice,
		aliceFundPoint, amountPaid, int64(0))

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
}

// testMultiHopSendToRoute tests that payments are properly processed
// through a provided route. We'll create the following network topology:
//      Alice --100k--> Bob --100k--> Carol
// We'll query the daemon for routes from Alice to Carol and then
// send payments through the routes.
func testMultiHopSendToRoute(net *lntest.NetworkHarness, t *harnessTest) {
	const chanAmt = btcutil.Amount(100000)
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 15)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointAlice := openChannelAndAssert(ctxt, t, net, net.Alice,
		net.Bob, chanAmt, 0, false)
	networkChans = append(networkChans, chanPointAlice)

	txidHash, err := getChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// Create Carol and establish a channel from Bob. Bob is the sole funder
	// of the channel with 100k satoshis. The network topology should look like:
	// Alice -> Bob -> Carol
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	if err := net.ConnectNodes(ctxb, carol, net.Bob); err != nil {
		t.Fatalf("unable to connect carol to alice: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, net.Bob)
	if err != nil {
		t.Fatalf("unable to send coins to bob: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointBob := openChannelAndAssert(ctxt, t, net, net.Bob,
		carol, chanAmt, 0, false)
	networkChans = append(networkChans, chanPointBob)
	txidHash, err = getChanPointFundingTxid(chanPointBob)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	bobChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	bobFundPoint := wire.OutPoint{
		Hash:  *bobChanTXID,
		Index: chanPointBob.OutputIndex,
	}

	// Wait for all nodes to have seen all channels.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol}
	nodeNames := []string{"Alice", "Bob", "Carol"}
	for _, chanPoint := range networkChans {
		for i, node := range nodes {
			txidHash, err := getChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			txid, e := chainhash.NewHash(txidHash)
			if e != nil {
				t.Fatalf("unable to create sha hash: %v", e)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ = context.WithTimeout(ctxb, timeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, point, err)
			}
		}
	}

	// Query for routes to pay from Alice to Carol.
	// We set FinalCltvDelta to 144 since by default QueryRoutes returns
	// the last hop with a final cltv delta of 9 where as the default in
	// htlcswitch is 144.
	const paymentAmt = 1000
	routesReq := &lnrpc.QueryRoutesRequest{
		PubKey:         carol.PubKeyStr,
		Amt:            paymentAmt,
		NumRoutes:      1,
		FinalCltvDelta: 144,
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	routes, err := net.Alice.QueryRoutes(ctxt, routesReq)
	if err != nil {
		t.Fatalf("unable to get route: %v", err)
	}

	// Create 5 invoices for Carol, which expect a payment from Alice for 1k
	// satoshis with a different preimage each time.
	const numPayments = 5
	rHashes := make([][]byte, numPayments)
	for i := 0; i < numPayments; i++ {
		invoice := &lnrpc.Invoice{
			Value: paymentAmt,
		}
		resp, err := carol.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		rHashes[i] = resp.RHash
	}

	// We'll wait for all parties to recognize the new channels within the
	// network.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointBob)
	if err != nil {
		t.Fatalf("bob didn't advertise his channel in time: %v", err)
	}

	time.Sleep(time.Millisecond * 50)

	// Using Alice as the source, pay to the 5 invoices from Carol created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	alicePayStream, err := net.Alice.SendToRoute(ctxt)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}

	for _, rHash := range rHashes {
		sendReq := &lnrpc.SendToRouteRequest{
			PaymentHash: rHash,
			Routes:      routes.Routes,
		}
		err := alicePayStream.Send(sendReq)

		if err != nil {
			t.Fatalf("unable to send payment: %v", err)
		}
	}

	for range rHashes {
		resp, err := alicePayStream.Recv()
		if err != nil {
			t.Fatalf("unable to send payment: %v", err)
		}
		if resp.PaymentError != "" {
			t.Fatalf("received payment error: %v", resp.PaymentError)
		}
	}

	// When asserting the amount of satoshis moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.
	const baseFee = 1

	// At this point all the channels within our proto network should be
	// shifted by 5k satoshis in the direction of Carol, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Alice->Bob->Carol, order is Carol, Bob,
	// Alice.
	const amountPaid = int64(5000)
	assertAmountPaid(t, ctxb, "Bob(local) => Carol(remote)", carol,
		bobFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, ctxb, "Bob(local) => Carol(remote)", net.Bob,
		bobFundPoint, amountPaid, int64(0))
	assertAmountPaid(t, ctxb, "Alice(local) => Bob(remote)", net.Bob,
		aliceFundPoint, int64(0), amountPaid+(baseFee*numPayments))
	assertAmountPaid(t, ctxb, "Alice(local) => Bob(remote)", net.Alice,
		aliceFundPoint, amountPaid+(baseFee*numPayments), int64(0))

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointBob, false)
}

// testSendToRouteErrorPropagation tests propagation of errors that occur
// while processing a multi-hop payment through an unknown route.
func testSendToRouteErrorPropagation(net *lntest.NetworkHarness, t *harnessTest) {
	const chanAmt = btcutil.Amount(100000)
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 5)

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointAlice := openChannelAndAssert(ctxt, t, net, net.Alice,
		net.Bob, chanAmt, 0, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPointAlice)
	if err != nil {
		t.Fatalf("alice didn't advertise her channel: %v", err)
	}

	// Create a new nodes (Carol and Charlie), load her with some funds, then establish
	// a connection between Carol and Charlie with a channel that has
	// identical capacity to the one created above.Then we will get route via queryroutes call
	// which will be fake route for Alice -> Bob graph.
	//
	// The network topology should now look like: Alice -> Bob; Carol -> Charlie.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}

	charlie, err := net.NewNode("Charlie", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, charlie)

	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, charlie)
	if err != nil {
		t.Fatalf("unable to send coins to charlie: %v", err)
	}

	if err := net.ConnectNodes(ctxb, carol, charlie); err != nil {
		t.Fatalf("unable to connect carol to alice: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointCarol := openChannelAndAssert(ctxt, t, net, carol,
		charlie, chanAmt, 0, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointCarol)
	if err != nil {
		t.Fatalf("carol didn't advertise her channel: %v", err)
	}

	// Query routes from Carol to Charlie which will be an invalid route
	// for Alice -> Bob.
	fakeReq := &lnrpc.QueryRoutesRequest{
		PubKey:    charlie.PubKeyStr,
		Amt:       int64(1),
		NumRoutes: 1,
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	fakeRoute, err := carol.QueryRoutes(ctxt, fakeReq)
	if err != nil {
		t.Fatalf("unable get fake route: %v", err)
	}

	// Create 1 invoices for Bob, which expect a payment from Alice for 1k
	// satoshis
	const paymentAmt = 1000

	invoice := &lnrpc.Invoice{
		Memo:  "testing",
		Value: paymentAmt,
	}
	resp, err := net.Bob.AddInvoice(ctxb, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	rHash := resp.RHash

	// Using Alice as the source, pay to the 5 invoices from Bob created above.
	alicePayStream, err := net.Alice.SendToRoute(ctxb)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}

	sendReq := &lnrpc.SendToRouteRequest{
		PaymentHash: rHash,
		Routes:      fakeRoute.Routes,
	}

	if err := alicePayStream.Send(sendReq); err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// At this place we should get an rpc error with notification
	// that edge is not found on hop(0)
	if _, err := alicePayStream.Recv(); err != nil && strings.Contains(err.Error(),
		"edge not found") {

	} else if err != nil {
		t.Fatalf("payment stream has been closed but fake route has consumed: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)
}

// testPrivateChannels tests that a private channel can be used for
// routing by the two endpoints of the channel, but is not known by
// the rest of the nodes in the graph.
func testPrivateChannels(net *lntest.NetworkHarness, t *harnessTest) {
	const chanAmt = btcutil.Amount(100000)
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 5)
	var networkChans []*lnrpc.ChannelPoint

	// We create the following topology:
	//
	// Dave --100k--> Alice --200k--> Bob
	//  ^		    ^
	//  |		    |
	// 100k		   100k
	//  |		    |
	//  +---- Carol ----+
	//
	// where the 100k channel between Carol and Alice is private.

	// Open a channel with 200k satoshis between Alice and Bob.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, chanAmt*2, 0, false,
	)
	networkChans = append(networkChans, chanPointAlice)

	txidHash, err := getChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// Create Dave, and a channel to Alice of 100k.
	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	if err := net.ConnectNodes(ctxb, dave, net.Alice); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointDave := openChannelAndAssert(
		ctxt, t, net, dave, net.Alice, chanAmt, 0, false,
	)
	networkChans = append(networkChans, chanPointDave)
	txidHash, err = getChanPointFundingTxid(chanPointDave)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	daveChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	daveFundPoint := wire.OutPoint{
		Hash:  *daveChanTXID,
		Index: chanPointDave.OutputIndex,
	}

	// Next, we'll create Carol and establish a channel from her to
	// Dave of 100k.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	if err := net.ConnectNodes(ctxb, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, carol, dave, chanAmt, 0, false,
	)
	networkChans = append(networkChans, chanPointCarol)

	txidHash, err = getChanPointFundingTxid(chanPointCarol)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	carolFundPoint := wire.OutPoint{
		Hash:  *carolChanTXID,
		Index: chanPointCarol.OutputIndex,
	}

	// Wait for all nodes to have seen all these channels, as they
	// are all public.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol, dave}
	nodeNames := []string{"Alice", "Bob", "Carol", "Dave"}
	for _, chanPoint := range networkChans {
		for i, node := range nodes {
			txidHash, err := getChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			txid, e := chainhash.NewHash(txidHash)
			if e != nil {
				t.Fatalf("unable to create sha hash: %v", e)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ = context.WithTimeout(ctxb, timeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, point, err)
			}
		}
	}
	// Now create a _private_ channel directly between Carol and
	// Alice of 100k.
	if err := net.ConnectNodes(ctxb, carol, net.Alice); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	chanOpenUpdate, err := net.OpenChannel(ctxb, carol, net.Alice, chanAmt,
		0, true)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// One block is enough to make the channel ready for use, since the
	// nodes have defaultNumConfs=1 set.
	block := mineBlocks(t, net, 1)[0]
	chanPointPrivate, err := net.WaitForChannelOpen(ctxb, chanOpenUpdate)
	if err != nil {
		t.Fatalf("error while waiting for channel open: %v", err)
	}
	txidHash, err = getChanPointFundingTxid(chanPointPrivate)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	fundingTxID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	assertTxInBlock(t, block, fundingTxID)

	// The channel should be listed in the peer information returned by
	// both peers.
	privateFundPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: chanPointPrivate.OutputIndex,
	}
	err = net.AssertChannelExists(ctxb, carol, &privateFundPoint)
	if err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}
	err = net.AssertChannelExists(ctxb, net.Alice, &privateFundPoint)
	if err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}

	// The channel should be available for payments between Carol and Alice.
	// We check this by sending payments from Carol to Bob, that
	// collectively would deplete at least one of Carol's channels.

	// Create 2 invoices for Bob, each of 70k satoshis. Since each of
	// Carol's channels is of size 100k, these payments cannot succeed
	// by only using one of the channels.
	const numPayments = 2
	const paymentAmt = 70000
	payReqs := make([]string, numPayments)
	for i := 0; i < numPayments; i++ {
		invoice := &lnrpc.Invoice{
			Memo:  "testing",
			Value: paymentAmt,
		}
		resp, err := net.Bob.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		payReqs[i] = resp.PaymentRequest
	}

	time.Sleep(time.Millisecond * 50)

	// Let Carol pay the invoices.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(ctxt, carol, payReqs, true)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// When asserting the amount of satoshis moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.
	const baseFee = 1

	// Bob should have received 140k satoshis from Alice.
	assertAmountPaid(t, ctxb, "Alice(local) => Bob(remote)", net.Bob,
		aliceFundPoint, int64(0), 2*paymentAmt)

	// Alice sent 140k to Bob.
	assertAmountPaid(t, ctxb, "Alice(local) => Bob(remote)", net.Alice,
		aliceFundPoint, 2*paymentAmt, int64(0))

	// Alice received 70k + fee from Dave.
	assertAmountPaid(t, ctxb, "Dave(local) => Alice(remote)", net.Alice,
		daveFundPoint, int64(0), paymentAmt+baseFee)

	// Dave sent 70k+fee to Alice.
	assertAmountPaid(t, ctxb, "Dave(local) => Alice(remote)", dave,
		daveFundPoint, paymentAmt+baseFee, int64(0))

	// Dave received 70k+fee of two hops from Carol.
	assertAmountPaid(t, ctxb, "Carol(local) => Dave(remote)", dave,
		carolFundPoint, int64(0), paymentAmt+baseFee*2)

	// Carol sent 70k+fee of two hops to Dave.
	assertAmountPaid(t, ctxb, "Carol(local) => Dave(remote)", carol,
		carolFundPoint, paymentAmt+baseFee*2, int64(0))

	// Alice received 70k+fee from Carol.
	assertAmountPaid(t, ctxb, "Carol(local) [private=>] Alice(remote)",
		net.Alice, privateFundPoint, int64(0), paymentAmt+baseFee)

	// Carol sent 70k+fee to Alice.
	assertAmountPaid(t, ctxb, "Carol(local) [private=>] Alice(remote)",
		carol, privateFundPoint, paymentAmt+baseFee, int64(0))

	// Alice should also be able to route payments using this channel,
	// so send two payments of 60k back to Carol.
	const paymentAmt60k = 60000
	payReqs = make([]string, numPayments)
	for i := 0; i < numPayments; i++ {
		invoice := &lnrpc.Invoice{
			Memo:  "testing",
			Value: paymentAmt60k,
		}
		resp, err := carol.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		payReqs[i] = resp.PaymentRequest
	}

	time.Sleep(time.Millisecond * 50)

	// Let Bob pay the invoices.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(ctxt, net.Alice, payReqs, true)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Finally, we make sure Dave and Bob does not know about the
	// private channel between Carol and Alice. We first mine
	// plenty of blocks, such that the channel would have been
	// announced in case it was public.
	mineBlocks(t, net, 10)

	// We create a helper method to check how many edges each of the
	// nodes know about. Carol and Alice should know about 4, while
	// Bob and Dave should only know about 3, since one channel is
	// private.
	numChannels := func(node *lntest.HarnessNode) int {
		req := &lnrpc.ChannelGraphRequest{}
		ctxt, _ := context.WithTimeout(ctxb, timeout)
		chanGraph, err := node.DescribeGraph(ctxt, req)
		if err != nil {
			t.Fatalf("unable go describegraph: %v", err)
		}
		return len(chanGraph.Edges)
	}

	var predErr error
	err = lntest.WaitPredicate(func() bool {
		aliceChans := numChannels(net.Alice)
		if aliceChans != 4 {
			predErr = fmt.Errorf("expected Alice to know 4 edges, "+
				"had %v", aliceChans)
			return false
		}
		bobChans := numChannels(net.Bob)
		if bobChans != 3 {
			predErr = fmt.Errorf("expected Bob to know 3 edges, "+
				"had %v", bobChans)
			return false
		}
		carolChans := numChannels(carol)
		if carolChans != 4 {
			predErr = fmt.Errorf("expected Carol to know 4 edges, "+
				"had %v", carolChans)
			return false
		}
		daveChans := numChannels(dave)
		if daveChans != 3 {
			predErr = fmt.Errorf("expected Dave to know 3 edges, "+
				"had %v", daveChans)
			return false
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("%v", predErr)
	}

	// Close all channels.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, dave, chanPointDave, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointPrivate, false)
}

// testInvoiceRoutingHints tests that the routing hints for an invoice are
// created properly.
func testInvoiceRoutingHints(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	timeout := time.Duration(15 * time.Second)
	const chanAmt = btcutil.Amount(100000)

	// Throughout this test, we'll be opening a channel betwen Alice and
	// several other parties.
	//
	// First, we'll create a private channel between Alice and Bob. This
	// will be the only channel that will be considered as a routing hint
	// throughout this test. We'll include a push amount since we currently
	// require channels to have enough remote balance to cover the invoice's
	// payment.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointBob := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, chanAmt, chanAmt/2, true,
	)

	// Then, we'll create Carol's node and open a public channel between her
	// and Alice. This channel will not be considered as a routing hint due
	// to it being public.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create carol's node: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	if err := net.ConnectNodes(ctxb, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect alice to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, net.Alice, carol, chanAmt, chanAmt/2, false,
	)

	// Then, we'll create Dave's node and open a private channel between him
	// and Alice. We will not include a push amount in order to not consider
	// this channel as a routing hint as it will not have enough remote
	// balance for the invoice's amount.
	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create dave's node: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	if err := net.ConnectNodes(ctxb, net.Alice, dave); err != nil {
		t.Fatalf("unable to connect alice to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointDave := openChannelAndAssert(
		ctxt, t, net, net.Alice, dave, chanAmt, 0, true,
	)

	// Finally, we'll create Eve's node and open a private channel between
	// her and Alice. This time though, we'll take Eve's node down after the
	// channel has been created to avoid populating routing hints for
	// inactive channels.
	eve, err := net.NewNode("Eve", nil)
	if err != nil {
		t.Fatalf("unable to create eve's node: %v", err)
	}
	if err := net.ConnectNodes(ctxb, net.Alice, eve); err != nil {
		t.Fatalf("unable to connect alice to eve: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointEve := openChannelAndAssert(
		ctxt, t, net, net.Alice, eve, chanAmt, chanAmt/2, true,
	)

	// Make sure all the channels have been opened.
	nodeNames := []string{"bob", "carol", "dave", "eve"}
	aliceChans := []*lnrpc.ChannelPoint{
		chanPointBob, chanPointCarol, chanPointDave, chanPointEve,
	}
	for i, chanPoint := range aliceChans {
		ctxt, _ := context.WithTimeout(ctxb, timeout)
		err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
		if err != nil {
			t.Fatalf("timed out waiting for channel open between "+
				"alice and %s: %v", nodeNames[i], err)
		}
	}

	// Now that the channels are open, we'll take down Eve's node.
	shutdownAndAssert(net, t, eve)

	// Create an invoice for Alice that will populate the routing hints.
	invoice := &lnrpc.Invoice{
		Memo:    "routing hints",
		Value:   int64(chanAmt / 4),
		Private: true,
	}

	// Due to the way the channels were set up above, the channel between
	// Alice and Bob should be the only channel used as a routing hint.
	var predErr error
	var decoded *lnrpc.PayReq
	err = lntest.WaitPredicate(func() bool {
		resp, err := net.Alice.AddInvoice(ctxb, invoice)
		if err != nil {
			predErr = fmt.Errorf("unable to add invoice: %v", err)
			return false
		}

		// We'll decode the invoice's payment request to determine which
		// channels were used as routing hints.
		payReq := &lnrpc.PayReqString{resp.PaymentRequest}
		decoded, err = net.Alice.DecodePayReq(ctxb, payReq)
		if err != nil {
			predErr = fmt.Errorf("unable to decode payment "+
				"request: %v", err)
			return false
		}

		if len(decoded.RouteHints) != 1 {
			predErr = fmt.Errorf("expected one route hint, got %d",
				len(decoded.RouteHints))
			return false
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	hops := decoded.RouteHints[0].HopHints
	if len(hops) != 1 {
		t.Fatalf("expected one hop in route hint, got %d", len(hops))
	}
	chanID := hops[0].ChanId

	// We'll need the short channel ID of the channel between Alice and Bob
	// to make sure the routing hint is for this channel.
	listReq := &lnrpc.ListChannelsRequest{}
	listResp, err := net.Alice.ListChannels(ctxb, listReq)
	if err != nil {
		t.Fatalf("unable to retrieve alice's channels: %v", err)
	}

	var aliceBobChanID uint64
	for _, channel := range listResp.Channels {
		if channel.RemotePubkey == net.Bob.PubKeyStr {
			aliceBobChanID = channel.ChanId
		}
	}

	if aliceBobChanID == 0 {
		t.Fatalf("channel between alice and bob not found")
	}

	if chanID != aliceBobChanID {
		t.Fatalf("expected channel ID %d, got %d", aliceBobChanID,
			chanID)
	}

	// Now that we've confirmed the routing hints were added correctly, we
	// can close all the channels and shut down all the nodes created.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointBob, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointCarol, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointDave, false)

	// The channel between Alice and Eve should be force closed since Eve
	// is offline.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointEve, true)
}

// testMultiHopOverPrivateChannels tests that private channels can be used as
// intermediate hops in a route for payments.
func testMultiHopOverPrivateChannels(net *lntest.NetworkHarness, t *harnessTest) {
	// We'll test that multi-hop payments over private channels work as
	// intended. To do so, we'll create the following topology:
	//         private        public           private
	// Alice <--100k--> Bob <--100k--> Carol <--100k--> Dave

	ctxb := context.Background()
	timeout := time.Duration(15 * time.Second)
	const chanAmt = btcutil.Amount(100000)

	// First, we'll open a private channel between Alice and Bob with Alice
	// being the funder.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, chanAmt, 0, true,
	)

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err := net.Alice.WaitForNetworkChannelOpen(ctxb, chanPointAlice)
	if err != nil {
		t.Fatalf("alice didn't see the channel alice <-> bob before "+
			"timeout: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Bob.WaitForNetworkChannelOpen(ctxb, chanPointAlice)
	if err != nil {
		t.Fatalf("bob didn't see the channel alice <-> bob before "+
			"timeout: %v", err)
	}

	// Retrieve Alice's funding outpoint.
	txidHash, err := getChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// Next, we'll create Carol's node and open a public channel between
	// her and Bob with Bob being the funder.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create carol's node: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	if err := net.ConnectNodes(ctxb, net.Bob, carol); err != nil {
		t.Fatalf("unable to connect bob to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointBob := openChannelAndAssert(
		ctxt, t, net, net.Bob, carol, chanAmt, 0, false,
	)

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Bob.WaitForNetworkChannelOpen(ctxb, chanPointBob)
	if err != nil {
		t.Fatalf("bob didn't see the channel bob <-> carol before "+
			"timeout: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = carol.WaitForNetworkChannelOpen(ctxb, chanPointBob)
	if err != nil {
		t.Fatalf("carol didn't see the channel bob <-> carol before "+
			"timeout: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxb, chanPointBob)
	if err != nil {
		t.Fatalf("alice didn't see the channel bob <-> carol before "+
			"timeout: %v", err)
	}

	// Retrieve Bob's funding outpoint.
	txidHash, err = getChanPointFundingTxid(chanPointBob)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	bobChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	bobFundPoint := wire.OutPoint{
		Hash:  *bobChanTXID,
		Index: chanPointBob.OutputIndex,
	}

	// Next, we'll create Dave's node and open a private channel between him
	// and Carol with Carol being the funder.
	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create dave's node: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	if err := net.ConnectNodes(ctxb, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, carol, dave, chanAmt, 0, true,
	)

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = carol.WaitForNetworkChannelOpen(ctxb, chanPointCarol)
	if err != nil {
		t.Fatalf("carol didn't see the channel carol <-> dave before "+
			"timeout: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = dave.WaitForNetworkChannelOpen(ctxb, chanPointCarol)
	if err != nil {
		t.Fatalf("dave didn't see the channel carol <-> dave before "+
			"timeout: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = dave.WaitForNetworkChannelOpen(ctxb, chanPointBob)
	if err != nil {
		t.Fatalf("dave didn't see the channel bob <-> carol before "+
			"timeout: %v", err)
	}

	// Retrieve Carol's funding point.
	txidHash, err = getChanPointFundingTxid(chanPointCarol)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	carolFundPoint := wire.OutPoint{
		Hash:  *carolChanTXID,
		Index: chanPointCarol.OutputIndex,
	}

	// Now that all the channels are set up according to the topology from
	// above, we can proceed to test payments. We'll create an invoice for
	// Dave of 20k satoshis and pay it with Alice. Since there is no public
	// route from Alice to Dave, we'll need to use the private channel
	// between Carol and Dave as a routing hint encoded in the invoice.
	const paymentAmt = 20000

	// Create the invoice for Dave.
	invoice := &lnrpc.Invoice{
		Memo:    "two hopz!",
		Value:   paymentAmt,
		Private: true,
	}

	resp, err := dave.AddInvoice(ctxb, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice for dave: %v", err)
	}

	// Let Alice pay the invoice.
	payReqs := []string{resp.PaymentRequest}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(ctxt, net.Alice, payReqs, true)
	if err != nil {
		t.Fatalf("unable to send payments from alice to dave: %v", err)
	}

	// When asserting the amount of satoshis moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when opening
	// the channels.
	const baseFee = 1

	// Dave should have received 20k satoshis from Carol.
	assertAmountPaid(t, ctxb, "Carol(local) [private=>] Dave(remote)",
		dave, carolFundPoint, 0, paymentAmt)

	// Carol should have sent 20k satoshis to Dave.
	assertAmountPaid(t, ctxb, "Carol(local) [private=>] Dave(remote)",
		carol, carolFundPoint, paymentAmt, 0)

	// Carol should have received 20k satoshis + fee for one hop from Bob.
	assertAmountPaid(t, ctxb, "Bob(local) => Carol(remote)",
		carol, bobFundPoint, 0, paymentAmt+baseFee)

	// Bob should have sent 20k satoshis + fee for one hop to Carol.
	assertAmountPaid(t, ctxb, "Bob(local) => Carol(remote)",
		net.Bob, bobFundPoint, paymentAmt+baseFee, 0)

	// Bob should have received 20k satoshis + fee for two hops from Alice.
	assertAmountPaid(t, ctxb, "Alice(local) [private=>] Bob(remote)", net.Bob,
		aliceFundPoint, 0, paymentAmt+baseFee*2)

	// Alice should have sent 20k satoshis + fee for two hops to Bob.
	assertAmountPaid(t, ctxb, "Alice(local) [private=>] Bob(remote)", net.Alice,
		aliceFundPoint, paymentAmt+baseFee*2, 0)

	// At this point, the payment was successful. We can now close all the
	// channels and shutdown the nodes created throughout this test.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, chanPointBob, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)
}

func testInvoiceSubscriptions(net *lntest.NetworkHarness, t *harnessTest) {
	const chanAmt = btcutil.Amount(500000)
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 5)

	// Open a channel with 500k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, chanAmt, 0, false,
	)

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

	quit := make(chan struct{})
	updateSent := make(chan struct{})
	go func() {
		invoiceUpdate, err := bobInvoiceSubscription.Recv()
		select {
		case <-quit:
			// Received cancellation
			return
		default:
		}

		if err != nil {
			t.Fatalf("unable to recv invoice update: %v", err)
		}

		// The invoice update should exactly match the invoice created
		// above, but should now be settled and have SettleDate
		if !invoiceUpdate.Settled {
			t.Fatalf("invoice not settled but should be")
		}
		if invoiceUpdate.SettleDate == 0 {
			t.Fatalf("invoice should have non zero settle date, but doesn't")
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
		close(quit)
		t.Fatalf("channel not seen by alice before timeout: %v", err)
	}

	// With the assertion above set up, send a payment from Alice to Bob
	// which should finalize and settle the invoice.
	sendReq := &lnrpc.SendRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	resp, err := net.Alice.SendPaymentSync(ctxt, sendReq)
	if err != nil {
		close(quit)
		t.Fatalf("unable to send payment: %v", err)
	}
	if resp.PaymentError != "" {
		close(quit)
		t.Fatalf("error when attempting recv: %v", resp.PaymentError)
	}

	select {
	case <-time.After(time.Second * 10):
		close(quit)
		t.Fatalf("update not sent after 10 seconds")
	case <-updateSent: // Fall through on success
	}

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

// testBasicChannelCreation test multiple channel opening and closing.
func testBasicChannelCreation(net *lntest.NetworkHarness, t *harnessTest) {
	const (
		numChannels = 2
		timeout     = time.Duration(time.Second * 5)
		amount      = maxBtcFundingAmount
	)

	// Open the channel between Alice and Bob, asserting that the
	// channel has been properly open on-chain.
	chanPoints := make([]*lnrpc.ChannelPoint, numChannels)
	for i := 0; i < numChannels; i++ {
		ctx, _ := context.WithTimeout(context.Background(), timeout)
		chanPoints[i] = openChannelAndAssert(
			ctx, t, net, net.Alice, net.Bob, amount, 0, false,
		)
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
func testMaxPendingChannels(net *lntest.NetworkHarness, t *harnessTest) {
	maxPendingChannels := defaultMaxPendingChannels + 1
	amount := maxBtcFundingAmount

	timeout := time.Duration(time.Second * 10)
	ctx, _ := context.WithTimeout(context.Background(), timeout)

	// Create a new node (Carol) with greater number of max pending
	// channels.
	args := []string{
		fmt.Sprintf("--maxpendingchannels=%v", maxPendingChannels),
	}
	carol, err := net.NewNode("Carol", args)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

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
			0, false)
		if err != nil {
			t.Fatalf("unable to open channel: %v", err)
		}
		openStreams[i] = stream
	}

	// Carol exhausted available amount of pending channels, next open
	// channel request should cause ErrorGeneric to be sent back to Alice.
	ctx, _ = context.WithTimeout(context.Background(), timeout)
	_, err = net.OpenChannel(ctx, net.Alice, carol, amount, 0, false)
	if err == nil {
		t.Fatalf("error wasn't received")
	} else if grpc.Code(err) != lnwire.ErrMaxPendingChannels.ToGrpcCode() {
		t.Fatalf("not expected error was received: %v", err)
	}

	// For now our channels are in pending state, in order to not interfere
	// with other tests we should clean up - complete opening of the
	// channel and then close it.

	// Mine 6 blocks, then wait for node's to notify us that the channel has
	// been opened. The funding transactions should be found within the
	// first newly mined block. 6 blocks make sure the funding transaction
	// has enough confirmations to be announced publicly.
	block := mineBlocks(t, net, 6)[0]

	chanPoints := make([]*lnrpc.ChannelPoint, maxPendingChannels)
	for i, stream := range openStreams {
		ctxt, _ := context.WithTimeout(context.Background(), timeout)
		fundingChanPoint, err := net.WaitForChannelOpen(ctxt, stream)
		if err != nil {
			t.Fatalf("error while waiting for channel open: %v", err)
		}

		txidHash, err := getChanPointFundingTxid(fundingChanPoint)
		if err != nil {
			t.Fatalf("unable to get txid: %v", err)
		}
		fundingTxID, err := chainhash.NewHash(txidHash)
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

// waitForTxInMempool polls until finding one transaction in the provided
// miner's mempool. An error is returned if *one* transaction isn't found within
// the given timeout.
func waitForTxInMempool(miner *rpcclient.Client,
	timeout time.Duration) (*chainhash.Hash, error) {

	txs, err := waitForNTxsInMempool(miner, 1, timeout)
	if err != nil {
		return nil, err
	}

	return txs[0], err
}

// waitForNTxsInMempool polls until finding the desired number of transactions
// in the provided miner's mempool. An error is returned if this number is not
// met after the given timeout.
func waitForNTxsInMempool(miner *rpcclient.Client, n int,
	timeout time.Duration) ([]*chainhash.Hash, error) {

	breakTimeout := time.After(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	var err error
	var mempool []*chainhash.Hash
	for {
		select {
		case <-breakTimeout:
			return nil, fmt.Errorf("wanted %v, found %v txs "+
				"in mempool: %v", n, len(mempool), mempool)
		case <-ticker.C:
			mempool, err = miner.GetRawMempool()
			if err != nil {
				return nil, err
			}

			if len(mempool) == n {
				return mempool, nil
			}
		}
	}
}

// testFailingChannel tests that we will fail the channel by force closing ii
// in the case where a counterparty tries to settle an HTLC with the wrong
// preimage.
func testFailingChannel(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	const (
		timeout    = time.Duration(time.Second * 10)
		paymentAmt = 10000
		defaultCSV = 4
	)

	chanAmt := maxFundingAmount

	// We'll introduce Carol, which will settle any incoming invoice with a
	// totally unrelated preimage.
	carol, err := net.NewNode("Carol",
		[]string{"--debughtlc", "--hodl.bogus-settle"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	// Let Alice connect and open a channel to Carol,
	if err := net.ConnectNodes(ctxb, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect alice to carol: %v", err)
	}
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, carol, chanAmt, 0, false,
	)

	// With the channel open, we'll create a invoice for Carol that Alice
	// will attempt to pay.
	preimage := bytes.Repeat([]byte{byte(192)}, 32)
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: preimage,
		Value:     paymentAmt,
	}
	resp, err := carol.AddInvoice(ctxb, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}
	carolPayReqs := []string{resp.PaymentRequest}

	// Wait for Alice to receive the channel edge from the funding manager.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't see the alice->carol channel before "+
			"timeout: %v", err)
	}

	// Send the payment from Alice to Carol. We expect Carol to attempt to
	// settle this payment with the wrong preimage.
	err = completePaymentRequests(ctxb, net.Alice, carolPayReqs, false)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Since Alice detects that Carol is trying to trick her by providing a
	// fake preimage, she should fail and force close the channel.
	var predErr error
	err = lntest.WaitPredicate(func() bool {
		pendingChansRequest := &lnrpc.PendingChannelsRequest{}
		pendingChanResp, err := net.Alice.PendingChannels(ctxb,
			pendingChansRequest)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		n := len(pendingChanResp.WaitingCloseChannels)
		if n != 1 {
			predErr = fmt.Errorf("Expected to find %d channels "+
				"waiting close, found %d", 1, n)
			return false
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("%v", predErr)
	}

	// Mine a block to confirm the broadcasted commitment.
	block := mineBlocks(t, net, 1)[0]
	if len(block.Transactions) != 2 {
		t.Fatalf("transaction wasn't mined")
	}

	// The channel should now show up as force closed both for Alice and
	// Carol.
	err = lntest.WaitPredicate(func() bool {
		pendingChansRequest := &lnrpc.PendingChannelsRequest{}
		pendingChanResp, err := net.Alice.PendingChannels(ctxb,
			pendingChansRequest)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		n := len(pendingChanResp.WaitingCloseChannels)
		if n != 0 {
			predErr = fmt.Errorf("Expected to find %d channels "+
				"waiting close, found %d", 0, n)
			return false
		}
		n = len(pendingChanResp.PendingForceClosingChannels)
		if n != 1 {
			predErr = fmt.Errorf("expected to find %d channel "+
				"pending force close, found %d", 1, n)
			return false
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("%v", predErr)
	}

	err = lntest.WaitPredicate(func() bool {
		pendingChansRequest := &lnrpc.PendingChannelsRequest{}
		pendingChanResp, err := carol.PendingChannels(ctxb,
			pendingChansRequest)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		n := len(pendingChanResp.PendingForceClosingChannels)
		if n != 1 {
			predErr = fmt.Errorf("expected to find %d channel "+
				"pending force close, found %d", 1, n)
			return false
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("%v", predErr)
	}

	// Carol will use the correct preimage to resolve the HTLC on-chain.
	_, err = waitForTxInMempool(net.Miner.Node, 5*time.Second)
	if err != nil {
		t.Fatalf("unable to find Bob's breach tx in mempool: %v", err)
	}

	// Mine enough blocks for Alice to sweep her funds from the force
	// closed channel.
	_, err = net.Miner.Node.Generate(defaultCSV)
	if err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// Wait for the sweeping tx to be broadcast.
	_, err = waitForTxInMempool(net.Miner.Node, 5*time.Second)
	if err != nil {
		t.Fatalf("unable to find Bob's breach tx in mempool: %v", err)
	}

	// Mine the sweep.
	_, err = net.Miner.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// No pending channels should be left.
	err = lntest.WaitPredicate(func() bool {
		pendingChansRequest := &lnrpc.PendingChannelsRequest{}
		pendingChanResp, err := net.Alice.PendingChannels(ctxb,
			pendingChansRequest)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		n := len(pendingChanResp.PendingForceClosingChannels)
		if n != 0 {
			predErr = fmt.Errorf("expected to find %d channel "+
				"pending force close, found %d", 0, n)
			return false
		}
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("%v", predErr)
	}
}

// testRevokedCloseRetribution tests that Alice is able carry out
// retribution in the event that she fails immediately after detecting Bob's
// breach txn in the mempool.
func testRevokedCloseRetribution(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	const (
		timeout     = time.Duration(time.Second * 10)
		chanAmt     = maxBtcFundingAmount
		paymentAmt  = 10000
		numInvoices = 6
	)

	// In order to test Alice's response to an uncooperative channel
	// closure by Bob, we'll first open up a channel between them with a
	// 0.5 BTC value.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, chanAmt, 0, false,
	)

	// With the channel open, we'll create a few invoices for Bob that
	// Alice will pay to in order to advance the state of the channel.
	bobPayReqs := make([]string, numInvoices)
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

		bobPayReqs[i] = resp.PaymentRequest
	}

	// As we'll be querying the state of bob's channels frequently we'll
	// create a closure helper function for the purpose.
	getBobChanInfo := func() (*lnrpc.Channel, error) {
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

	// Send payments from Alice to Bob using 3 of Bob's payment hashes
	// generated above.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(ctxt, net.Alice, bobPayReqs[:numInvoices/2],
		true)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Next query for Bob's channel state, as we sent 3 payments of 10k
	// satoshis each, Bob should now see his balance as being 30k satoshis.
	var bobChan *lnrpc.Channel
	var predErr error
	err = lntest.WaitPredicate(func() bool {
		bChan, err := getBobChanInfo()
		if err != nil {
			t.Fatalf("unable to get bob's channel info: %v", err)
		}
		if bChan.LocalBalance != 30000 {
			predErr = fmt.Errorf("bob's balance is incorrect, "+
				"got %v, expected %v", bChan.LocalBalance,
				30000)
			return false
		}

		bobChan = bChan
		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("%v", predErr)
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
	if err := copyFile(bobTempDbFile, net.Bob.DBPath()); err != nil {
		t.Fatalf("unable to copy database files: %v", err)
	}

	// Finally, send payments from Alice to Bob, consuming Bob's remaining
	// payment hashes.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(ctxt, net.Alice, bobPayReqs[numInvoices/2:],
		true)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
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
		return os.Rename(bobTempDbFile, net.Bob.DBPath())
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
	var closeUpdates lnrpc.Lightning_CloseChannelClient
	force := true
	err = lntest.WaitPredicate(func() bool {
		closeUpdates, _, err = net.CloseChannel(ctxb, net.Bob, chanPoint, force)
		if err != nil {
			predErr = err
			return false
		}

		return true
	}, time.Second*10)
	if err != nil {
		t.Fatalf("unable to close channel: %v", predErr)
	}

	// Wait for Bob's breach transaction to show up in the mempool to ensure
	// that Alice's node has started waiting for confirmations.
	_, err = waitForTxInMempool(net.Miner.Node, 5*time.Second)
	if err != nil {
		t.Fatalf("unable to find Bob's breach tx in mempool: %v", err)
	}

	// Here, Alice sees Bob's breach transaction in the mempool, but is waiting
	// for it to confirm before continuing her retribution. We restart Alice to
	// ensure that she is persisting her retribution state and continues
	// watching for the breach transaction to confirm even after her node
	// restarts.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("unable to restart Alice's node: %v", err)
	}

	// Finally, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block.
	block := mineBlocks(t, net, 1)[0]

	breachTXID, err := net.WaitForChannelClose(ctxb, closeUpdates)
	if err != nil {
		t.Fatalf("error while waiting for channel close: %v", err)
	}
	assertTxInBlock(t, block, breachTXID)

	// Query the mempool for Alice's justice transaction, this should be
	// broadcast as Bob's contract breaching transaction gets confirmed
	// above.
	justiceTXID, err := waitForTxInMempool(net.Miner.Node, 5*time.Second)
	if err != nil {
		t.Fatalf("unable to find Alice's justice tx in mempool: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

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

	// We restart Alice here to ensure that she persists her retribution state
	// and successfully continues exacting retribution after restarting. At
	// this point, Alice has broadcast the justice transaction, but it hasn't
	// been confirmed yet; when Alice restarts, she should start waiting for
	// the justice transaction to confirm again.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("unable to restart Alice's node: %v", err)
	}

	// Now mine a block, this transaction should include Alice's justice
	// transaction which was just accepted into the mempool.
	block = mineBlocks(t, net, 1)[0]

	// The block should have exactly *two* transactions, one of which is
	// the justice transaction.
	if len(block.Transactions) != 2 {
		t.Fatalf("transaction wasn't mined")
	}
	justiceSha := block.Transactions[1].TxHash()
	if !bytes.Equal(justiceTx.Hash()[:], justiceSha[:]) {
		t.Fatalf("justice tx wasn't mined")
	}

	assertNodeNumChannels(t, ctxb, net.Alice, 0)
}

// testRevokedCloseRetributionZeroValueRemoteOutput tests that Alice is able
// carry out retribution in the event that she fails in state where the remote
// commitment output has zero-value.
func testRevokedCloseRetributionZeroValueRemoteOutput(net *lntest.NetworkHarness,
	t *harnessTest) {

	ctxb := context.Background()
	const (
		timeout     = time.Duration(time.Second * 10)
		chanAmt     = maxBtcFundingAmount
		paymentAmt  = 10000
		numInvoices = 6
	)

	// Since we'd like to test some multi-hop failure scenarios, we'll
	// introduce another node into our test network: Carol.
	carol, err := net.NewNode("Carol", []string{"--debughtlc", "--hodl.exit-settle"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	// We must let Alice have an open channel before she can send a node
	// announcement, so we open a channel with Carol,
	if err := net.ConnectNodes(ctxb, net.Alice, carol); err != nil {
		t.Fatalf("unable to connect alice to carol: %v", err)
	}

	// In order to test Alice's response to an uncooperative channel
	// closure by Carol, we'll first open up a channel between them with a
	// 0.5 BTC value.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, carol, chanAmt, 0, false,
	)

	// With the channel open, we'll create a few invoices for Carol that
	// Alice will pay to in order to advance the state of the channel.
	carolPayReqs := make([]string, numInvoices)
	for i := 0; i < numInvoices; i++ {
		preimage := bytes.Repeat([]byte{byte(192 - i)}, 32)
		invoice := &lnrpc.Invoice{
			Memo:      "testing",
			RPreimage: preimage,
			Value:     paymentAmt,
		}
		resp, err := carol.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		carolPayReqs[i] = resp.PaymentRequest
	}

	// As we'll be querying the state of Carols's channels frequently we'll
	// create a closure helper function for the purpose.
	getCarolChanInfo := func() (*lnrpc.Channel, error) {
		req := &lnrpc.ListChannelsRequest{}
		carolChannelInfo, err := carol.ListChannels(ctxb, req)
		if err != nil {
			return nil, err
		}
		if len(carolChannelInfo.Channels) != 1 {
			t.Fatalf("carol should only have a single channel, "+
				"instead he has %v", len(carolChannelInfo.Channels))
		}

		return carolChannelInfo.Channels[0], nil
	}

	// Wait for Alice to receive the channel edge from the funding manager.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't see the alice->carol channel before "+
			"timeout: %v", err)
	}

	// Next query for Carol's channel state, as we sent 0 payments, Carol
	// should now see her balance as being 0 satoshis.
	carolChan, err := getCarolChanInfo()
	if err != nil {
		t.Fatalf("unable to get carol's channel info: %v", err)
	}
	if carolChan.LocalBalance != 0 {
		t.Fatalf("carol's balance is incorrect, got %v, expected %v",
			carolChan.LocalBalance, 0)
	}

	// Grab Carol's current commitment height (update number), we'll later
	// revert her to this state after additional updates to force him to
	// broadcast this soon to be revoked state.
	carolStateNumPreCopy := carolChan.NumUpdates

	// Create a temporary file to house Carol's database state at this
	// particular point in history.
	carolTempDbPath, err := ioutil.TempDir("", "carol-past-state")
	if err != nil {
		t.Fatalf("unable to create temp db folder: %v", err)
	}
	carolTempDbFile := filepath.Join(carolTempDbPath, "channel.db")
	defer os.Remove(carolTempDbPath)

	// With the temporary file created, copy Carol's current state into the
	// temporary file we created above. Later after more updates, we'll
	// restore this state.
	if err := copyFile(carolTempDbFile, carol.DBPath()); err != nil {
		t.Fatalf("unable to copy database files: %v", err)
	}

	// Finally, send payments from Alice to Carol, consuming Carol's remaining
	// payment hashes.
	err = completePaymentRequests(ctxb, net.Alice, carolPayReqs, false)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	carolChan, err = getCarolChanInfo()
	if err != nil {
		t.Fatalf("unable to get carol chan info: %v", err)
	}

	// Now we shutdown Carol, copying over the his temporary database state
	// which has the *prior* channel state over his current most up to date
	// state. With this, we essentially force Carol to travel back in time
	// within the channel's history.
	if err = net.RestartNode(carol, func() error {
		return os.Rename(carolTempDbFile, carol.DBPath())
	}); err != nil {
		t.Fatalf("unable to restart node: %v", err)
	}

	// Now query for Carol's channel state, it should show that he's at a
	// state number in the past, not the *latest* state.
	carolChan, err = getCarolChanInfo()
	if err != nil {
		t.Fatalf("unable to get carol chan info: %v", err)
	}
	if carolChan.NumUpdates != carolStateNumPreCopy {
		t.Fatalf("db copy failed: %v", carolChan.NumUpdates)
	}

	// Now force Carol to execute a *force* channel closure by unilaterally
	// broadcasting his current channel state. This is actually the
	// commitment transaction of a prior *revoked* state, so he'll soon
	// feel the wrath of Alice's retribution.
	var (
		closeUpdates lnrpc.Lightning_CloseChannelClient
		closeTxId    *chainhash.Hash
		closeErr     error
		force        bool = true
	)
	err = lntest.WaitPredicate(func() bool {
		closeUpdates, closeTxId, closeErr = net.CloseChannel(
			ctxb, carol, chanPoint, force,
		)
		return closeErr == nil
	}, time.Second*15)
	if err != nil {
		t.Fatalf("unable to close channel: %v", closeErr)
	}

	// Query the mempool for the breaching closing transaction, this should
	// be broadcast by Carol when she force closes the channel above.
	txid, err := waitForTxInMempool(net.Miner.Node, 20*time.Second)
	if err != nil {
		t.Fatalf("unable to find Carol's force close tx in mempool: %v",
			err)
	}
	if *txid != *closeTxId {
		t.Fatalf("expected closeTx(%v) in mempool, instead found %v",
			closeTxId, txid)
	}

	// Finally, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block.
	block := mineBlocks(t, net, 1)[0]

	// Here, Alice receives a confirmation of Carol's breach transaction.
	// We restart Alice to ensure that she is persisting her retribution
	// state and continues exacting justice after her node restarts.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("unable to stop Alice's node: %v", err)
	}

	breachTXID, err := net.WaitForChannelClose(ctxb, closeUpdates)
	if err != nil {
		t.Fatalf("error while waiting for channel close: %v", err)
	}
	assertTxInBlock(t, block, breachTXID)

	// Query the mempool for Alice's justice transaction, this should be
	// broadcast as Carol's contract breaching transaction gets confirmed
	// above.
	justiceTXID, err := waitForTxInMempool(net.Miner.Node, 15*time.Second)
	if err != nil {
		t.Fatalf("unable to find Alice's justice tx in mempool: %v",
			err)
	}
	time.Sleep(100 * time.Millisecond)

	// Query for the mempool transaction found above. Then assert that all
	// the inputs of this transaction are spending outputs generated by
	// Carol's breach transaction above.
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

	// We restart Alice here to ensure that she persists her retribution state
	// and successfully continues exacting retribution after restarting. At
	// this point, Alice has broadcast the justice transaction, but it hasn't
	// been confirmed yet; when Alice restarts, she should start waiting for
	// the justice transaction to confirm again.
	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("unable to restart Alice's node: %v", err)
	}

	// Now mine a block, this transaction should include Alice's justice
	// transaction which was just accepted into the mempool.
	block = mineBlocks(t, net, 1)[0]

	// The block should have exactly *two* transactions, one of which is
	// the justice transaction.
	if len(block.Transactions) != 2 {
		t.Fatalf("transaction wasn't mined")
	}
	justiceSha := block.Transactions[1].TxHash()
	if !bytes.Equal(justiceTx.Hash()[:], justiceSha[:]) {
		t.Fatalf("justice tx wasn't mined")
	}

	assertNodeNumChannels(t, ctxb, net.Alice, 0)
}

// testRevokedCloseRetributionRemoteHodl tests that Dave properly responds to a
// channel breach made by the remote party, specifically in the case that the
// remote party breaches before settling extended HTLCs.
func testRevokedCloseRetributionRemoteHodl(net *lntest.NetworkHarness,
	t *harnessTest) {

	ctxb := context.Background()
	const (
		timeout     = time.Duration(time.Second * 10)
		chanAmt     = maxBtcFundingAmount
		pushAmt     = 200000
		paymentAmt  = 10000
		numInvoices = 6
	)

	// Since this test will result in the counterparty being left in a
	// weird state, we will introduce another node into our test network:
	// Carol.
	carol, err := net.NewNode("Carol", []string{"--debughtlc", "--hodl.exit-settle"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	// We'll also create a new node Dave, who will have a channel with
	// Carol, and also use similar settings so we can broadcast a commit
	// with active HTLCs.
	dave, err := net.NewNode("Dave", []string{"--debughtlc", "--hodl.exit-settle"})
	if err != nil {
		t.Fatalf("unable to create new dave node: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	// We must let Dave communicate with Carol before they are able to open
	// channel, so we connect Dave and Carol,
	if err := net.ConnectNodes(ctxb, dave, carol); err != nil {
		t.Fatalf("unable to connect dave to carol: %v", err)
	}

	// Before we make a channel, we'll load up Dave with some coins sent
	// directly from the miner.
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}

	// In order to test Dave's response to an uncooperative channel closure
	// by Carol, we'll first open up a channel between them with a
	// maxBtcFundingAmount (2^24) satoshis value.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, dave, carol, chanAmt, pushAmt, false,
	)

	// With the channel open, we'll create a few invoices for Carol that
	// Dave will pay to in order to advance the state of the channel.
	carolPayReqs := make([]string, numInvoices)
	for i := 0; i < numInvoices; i++ {
		preimage := bytes.Repeat([]byte{byte(192 - i)}, 32)
		invoice := &lnrpc.Invoice{
			Memo:      "testing",
			RPreimage: preimage,
			Value:     paymentAmt,
		}
		resp, err := carol.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		carolPayReqs[i] = resp.PaymentRequest
	}

	// As we'll be querying the state of Carol's channels frequently we'll
	// create a closure helper function for the purpose.
	getCarolChanInfo := func() (*lnrpc.Channel, error) {
		req := &lnrpc.ListChannelsRequest{}
		carolChannelInfo, err := carol.ListChannels(ctxb, req)
		if err != nil {
			return nil, err
		}
		if len(carolChannelInfo.Channels) != 1 {
			t.Fatalf("carol should only have a single channel, instead he has %v",
				len(carolChannelInfo.Channels))
		}

		return carolChannelInfo.Channels[0], nil
	}

	// We'll introduce a closure to validate that Carol's current balance
	// matches the given expected amount.
	checkCarolBalance := func(expectedAmt int64) {
		carolChan, err := getCarolChanInfo()
		if err != nil {
			t.Fatalf("unable to get carol's channel info: %v", err)
		}
		if carolChan.LocalBalance != expectedAmt {
			t.Fatalf("carol's balance is incorrect, "+
				"got %v, expected %v", carolChan.LocalBalance,
				expectedAmt)
		}
	}

	// We'll introduce another closure to validate that Carol's current
	// number of updates is at least as large as the provided minimum
	// number.
	checkCarolNumUpdatesAtLeast := func(minimum uint64) {
		carolChan, err := getCarolChanInfo()
		if err != nil {
			t.Fatalf("unable to get carol's channel info: %v", err)
		}
		if carolChan.NumUpdates < minimum {
			t.Fatalf("carol's numupdates is incorrect, want %v "+
				"to be at least %v", carolChan.NumUpdates,
				minimum)
		}
	}

	// Wait for Dave to receive the channel edge from the funding manager.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("dave didn't see the dave->carol channel before "+
			"timeout: %v", err)
	}

	// Ensure that carol's balance starts with the amount we pushed to her.
	checkCarolBalance(pushAmt)

	// Send payments from Dave to Carol using 3 of Carol's payment hashes
	// generated above.
	err = completePaymentRequests(
		ctxb, dave, carolPayReqs[:numInvoices/2], false,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// At this point, we'll also send over a set of HTLC's from Carol to
	// Dave. This ensures that the final revoked transaction has HTLC's in
	// both directions.
	davePayReqs := make([]string, numInvoices)
	for i := 0; i < numInvoices; i++ {
		preimage := bytes.Repeat([]byte{byte(199 - i)}, 32)
		invoice := &lnrpc.Invoice{
			Memo:      "testing",
			RPreimage: preimage,
			Value:     paymentAmt,
		}
		resp, err := dave.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		davePayReqs[i] = resp.PaymentRequest
	}

	// Send payments from Carol to Dave using 3 of Dave's payment hashes
	// generated above.
	err = completePaymentRequests(
		ctxb, carol, davePayReqs[:numInvoices/2], false,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Next query for Carol's channel state, as we sent 3 payments of 10k
	// satoshis each, however Carol should now see her balance as being
	// equal to the push amount in satoshis since she has not settled.
	carolChan, err := getCarolChanInfo()
	if err != nil {
		t.Fatalf("unable to get carol's channel info: %v", err)
	}

	// Grab Carol's current commitment height (update number), we'll later
	// revert her to this state after additional updates to force her to
	// broadcast this soon to be revoked state.
	carolStateNumPreCopy := carolChan.NumUpdates

	// Ensure that carol's balance still reflects the original amount we
	// pushed to her, minus the HTLCs she just sent to Dave.
	checkCarolBalance(pushAmt - 3*paymentAmt)

	// Since Carol has not settled, she should only see at least one update
	// to her channel.
	checkCarolNumUpdatesAtLeast(1)

	// Create a temporary file to house Carol's database state at this
	// particular point in history.
	carolTempDbPath, err := ioutil.TempDir("", "carol-past-state")
	if err != nil {
		t.Fatalf("unable to create temp db folder: %v", err)
	}
	carolTempDbFile := filepath.Join(carolTempDbPath, "channel.db")
	defer os.Remove(carolTempDbPath)

	// With the temporary file created, copy Carol's current state into the
	// temporary file we created above. Later after more updates, we'll
	// restore this state.
	if err := copyFile(carolTempDbFile, carol.DBPath()); err != nil {
		t.Fatalf("unable to copy database files: %v", err)
	}

	// Finally, send payments from Dave to Carol, consuming Carol's
	// remaining payment hashes.
	err = completePaymentRequests(
		ctxb, dave, carolPayReqs[numInvoices/2:], false,
	)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Ensure that carol's balance still shows the amount we originally
	// pushed to her (minus the HTLCs she sent to Bob), and that at least
	// one more update has occurred.
	time.Sleep(500 * time.Millisecond)
	checkCarolBalance(pushAmt - 3*paymentAmt)
	checkCarolNumUpdatesAtLeast(carolStateNumPreCopy + 1)

	// Now we shutdown Carol, copying over the her temporary database state
	// which has the *prior* channel state over her current most up to date
	// state. With this, we essentially force Carol to travel back in time
	// within the channel's history.
	if err = net.RestartNode(carol, func() error {
		return os.Rename(carolTempDbFile, carol.DBPath())
	}); err != nil {
		t.Fatalf("unable to restart node: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// Ensure that Carol's view of the channel is consistent with the state
	// of the channel just before it was snapshotted.
	checkCarolBalance(pushAmt - 3*paymentAmt)
	checkCarolNumUpdatesAtLeast(1)

	// Now query for Carol's channel state, it should show that she's at a
	// state number in the past, *not* the latest state.
	carolChan, err = getCarolChanInfo()
	if err != nil {
		t.Fatalf("unable to get carol chan info: %v", err)
	}
	if carolChan.NumUpdates != carolStateNumPreCopy {
		t.Fatalf("db copy failed: %v", carolChan.NumUpdates)
	}

	// Now force Carol to execute a *force* channel closure by unilaterally
	// broadcasting her current channel state. This is actually the
	// commitment transaction of a prior *revoked* state, so she'll soon
	// feel the wrath of Dave's retribution.
	force := true
	closeUpdates, closeTxId, err := net.CloseChannel(ctxb, carol,
		chanPoint, force)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}

	// Query the mempool for the breaching closing transaction, this should
	// be broadcast by Carol when she force closes the channel above.
	txid, err := waitForTxInMempool(net.Miner.Node, 20*time.Second)
	if err != nil {
		t.Fatalf("unable to find Carol's force close tx in mempool: %v",
			err)
	}
	if *txid != *closeTxId {
		t.Fatalf("expected closeTx(%v) in mempool, instead found %v",
			closeTxId, txid)
	}
	time.Sleep(200 * time.Millisecond)

	// Generate a single block to mine the breach transaction.
	block := mineBlocks(t, net, 1)[0]

	// Wait so Dave receives a confirmation of Carol's breach transaction.
	time.Sleep(200 * time.Millisecond)

	// We restart Dave to ensure that he is persisting his retribution
	// state and continues exacting justice after her node restarts.
	if err := net.RestartNode(dave, nil); err != nil {
		t.Fatalf("unable to stop Dave's node: %v", err)
	}

	// Finally, wait for the final close status update, then ensure that
	// the closing transaction was included in the block.
	breachTXID, err := net.WaitForChannelClose(ctxb, closeUpdates)
	if err != nil {
		t.Fatalf("error while waiting for channel close: %v", err)
	}
	if *breachTXID != *closeTxId {
		t.Fatalf("expected breach ID(%v) to be equal to close ID (%v)",
			breachTXID, closeTxId)
	}
	assertTxInBlock(t, block, breachTXID)

	// Query the mempool for Dave's justice transaction, this should be
	// broadcast as Carol's contract breaching transaction gets confirmed
	// above. Since Carol might have had the time to take some of the HTLC
	// outputs to the second level before Alice broadcasts her justice tx,
	// we'll search through the mempool for a tx that matches the number of
	// expected inputs in the justice tx.
	// TODO(halseth): change to deterministic check if/when only acting on
	// confirmed second level spends?
	var predErr error
	var justiceTxid *chainhash.Hash
	err = lntest.WaitPredicate(func() bool {
		mempool, err := net.Miner.Node.GetRawMempool()
		if err != nil {
			t.Fatalf("unable to get mempool from miner: %v", err)
		}

		for _, txid := range mempool {
			// Check that the justice tx has the appropriate number
			// of inputs.
			tx, err := net.Miner.Node.GetRawTransaction(txid)
			if err != nil {
				predErr = fmt.Errorf("unable to query for "+
					"txs: %v", err)
				return false
			}

			exNumInputs := 2 + numInvoices
			if len(tx.MsgTx().TxIn) == exNumInputs {
				justiceTxid = txid
				return true
			}

		}

		predErr = fmt.Errorf("justice tx not found")
		return false
	}, time.Second*15)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	justiceTx, err := net.Miner.Node.GetRawTransaction(justiceTxid)
	if err != nil {
		t.Fatalf("unable to query for justice tx: %v", err)
	}

	// isSecondLevelSpend checks that the passed secondLevelTxid is a
	// potentitial second level spend spending from the commit tx.
	isSecondLevelSpend := func(commitTxid, secondLevelTxid *chainhash.Hash) bool {
		secondLevel, err := net.Miner.Node.GetRawTransaction(
			secondLevelTxid)
		if err != nil {
			t.Fatalf("unable to query for tx: %v", err)
		}

		// A second level spend should have only one input, and one
		// output.
		if len(secondLevel.MsgTx().TxIn) != 1 {
			return false
		}
		if len(secondLevel.MsgTx().TxOut) != 1 {
			return false
		}

		// The sole input should be spending from the commit tx.
		txIn := secondLevel.MsgTx().TxIn[0]
		if !bytes.Equal(txIn.PreviousOutPoint.Hash[:], commitTxid[:]) {
			return false
		}

		return true
	}

	// Check that all the inputs of this transaction are spending outputs
	// generated by Carol's breach transaction above.
	for _, txIn := range justiceTx.MsgTx().TxIn {
		if bytes.Equal(txIn.PreviousOutPoint.Hash[:], breachTXID[:]) {
			continue
		}

		// If the justice tx is spending from an output that was not on
		// the breach tx, Carol might have had the time to take an
		// output to the second level. In that case, check that the
		// justice tx is spending this second level output.
		if isSecondLevelSpend(breachTXID, &txIn.PreviousOutPoint.Hash) {
			continue
		}
		t.Fatalf("justice tx not spending commitment utxo "+
			"instead is: %v", txIn.PreviousOutPoint)
	}
	time.Sleep(100 * time.Millisecond)

	// We restart Dave here to ensure that he persists he retribution state
	// and successfully continues exacting retribution after restarting. At
	// this point, Dave has broadcast the justice transaction, but it
	// hasn't been confirmed yet; when Dave restarts, he should start
	// waiting for the justice transaction to confirm again.
	if err := net.RestartNode(dave, nil); err != nil {
		t.Fatalf("unable to restart Dave's node: %v", err)
	}

	// Now mine a block, this transaction should include Dave's justice
	// transaction which was just accepted into the mempool.
	block = mineBlocks(t, net, 1)[0]
	assertTxInBlock(t, block, justiceTxid)

	// Dave should have no open channels.
	assertNodeNumChannels(t, ctxb, dave, 0)
}

// assertNodeNumChannels polls the provided node's list channels rpc until it
// reaches the desired number of total channels.
func assertNodeNumChannels(t *harnessTest, ctxb context.Context,
	node *lntest.HarnessNode, numChannels int) {

	// Poll alice for her list of channels.
	req := &lnrpc.ListChannelsRequest{}

	var predErr error
	pred := func() bool {
		chanInfo, err := node.ListChannels(ctxb, req)
		if err != nil {
			predErr = fmt.Errorf("unable to query for alice's "+
				"channels: %v", err)
			return false
		}

		// Return true if the query returned the expected number of
		// channels.
		num := len(chanInfo.Channels)
		if num != numChannels {
			predErr = fmt.Errorf("expected %v channels, got %v",
				numChannels, num)
			return false
		}
		return true
	}

	if err := lntest.WaitPredicate(pred, time.Second*15); err != nil {
		t.Fatalf("node has incorrect number of channels: %v", predErr)
	}
}

func testHtlcErrorPropagation(net *lntest.NetworkHarness, t *harnessTest) {
	// In this test we wish to exercise the daemon's correct parsing,
	// handling, and propagation of errors that occur while processing a
	// multi-hop payment.
	timeout := time.Duration(time.Second * 15)
	ctxb := context.Background()

	const chanAmt = maxBtcFundingAmount

	// First establish a channel with a capacity of 0.5 BTC between Alice
	// and Bob.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, chanAmt, 0, false,
	)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.Alice.WaitForNetworkChannelOpen(ctxt, chanPointAlice); err != nil {
		t.Fatalf("channel not seen by alice before timeout: %v", err)
	}

	commitFee := calcStaticFee(0)
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
		if aliceBal.Balance != int64(chanAmt-commitFee) {
			t.Fatalf("alice has an incorrect balance: expected %v got %v",
				int64(chanAmt-commitFee), aliceBal)
		}
		if bobBal.Balance != int64(chanAmt-commitFee) {
			t.Fatalf("bob has an incorrect balance: expected %v got %v",
				int64(chanAmt-commitFee), bobBal)
		}
	}

	// Since we'd like to test some multi-hop failure scenarios, we'll
	// introduce another node into our test network: Carol.
	carol, err := net.NewNode("Carol", nil)
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
	const bobChanAmt = maxBtcFundingAmount
	chanPointBob := openChannelAndAssert(
		ctxt, t, net, net.Bob, carol, chanAmt, 0, false,
	)

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

	// For the first scenario, we'll test the cancellation of an HTLC with
	// an unknown payment hash.
	// TODO(roasbeef): return failure response rather than failing entire
	// stream on payment error.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	sendReq := &lnrpc.SendRequest{
		PaymentHashString: hex.EncodeToString(bytes.Repeat([]byte("Z"), 32)),
		DestString:        hex.EncodeToString(carol.PubKey[:]),
		Amt:               payAmt,
	}
	resp, err := net.Alice.SendPaymentSync(ctxt, sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// The payment should have resulted in an error since we sent it with the
	// wrong payment hash.
	if resp.PaymentError == "" {
		t.Fatalf("payment should have been rejected due to invalid " +
			"payment hash")
	}
	expectedErrorCode := lnwire.CodeUnknownPaymentHash.String()
	if !strings.Contains(resp.PaymentError, expectedErrorCode) {
		// TODO(roasbeef): make into proper gRPC error code
		t.Fatalf("payment should have failed due to unknown payment hash, "+
			"instead failed due to: %v", resp.PaymentError)
	}

	// The balances of all parties should be the same as initially since
	// the HTLC was cancelled.
	assertBaseBalance()

	// Next, we'll test the case of a recognized payHash but, an incorrect
	// value on the extended HTLC.
	sendReq = &lnrpc.SendRequest{
		PaymentHashString: hex.EncodeToString(carolInvoice.RHash),
		DestString:        hex.EncodeToString(carol.PubKey[:]),
		Amt:               1000, // 10k satoshis are expected.
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	resp, err = net.Alice.SendPaymentSync(ctxt, sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// The payment should fail with an error since we sent 1k satoshis isn't of
	// 10k as was requested.
	if resp.PaymentError == "" {
		t.Fatalf("payment should have been rejected due to wrong " +
			"HTLC amount")
	}
	expectedErrorCode = lnwire.CodeIncorrectPaymentAmount.String()
	if !strings.Contains(resp.PaymentError, expectedErrorCode) {
		t.Fatalf("payment should have failed due to wrong amount, "+
			"instead failed due to: %v", resp.PaymentError)
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
	// There's a max payment amount, so we'll have to do this
	// incrementally.
	chanReserve := int64(chanAmt / 100)
	amtToSend := int64(chanAmt) - chanReserve - 20000
	amtSent := int64(0)
	for amtSent != amtToSend {
		// We'll send in chunks of the max payment amount. If we're
		// about to send too much, then we'll only send the amount
		// remaining.
		toSend := int64(maxPaymentMSat.ToSatoshis())
		if toSend+amtSent > amtToSend {
			toSend = amtToSend - amtSent
		}

		invoiceReq = &lnrpc.Invoice{
			Value: toSend,
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

		if resp, err := bobPayStream.Recv(); err != nil {
			t.Fatalf("payment stream has been closed: %v", err)
		} else if resp.PaymentError != "" {
			t.Fatalf("bob's payment failed: %v", resp.PaymentError)
		}

		amtSent += toSend
	}

	// At this point, Alice has 50mil satoshis on her side of the channel,
	// but Bob only has 10k available on his side of the channel. So a
	// payment from Alice to Carol worth 100k satoshis should fail.
	invoiceReq = &lnrpc.Invoice{
		Value: 100000,
	}
	carolInvoice3, err := carol.AddInvoice(ctxb, invoiceReq)
	if err != nil {
		t.Fatalf("unable to generate carol invoice: %v", err)
	}

	sendReq = &lnrpc.SendRequest{
		PaymentRequest: carolInvoice3.PaymentRequest,
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	resp, err = net.Alice.SendPaymentSync(ctxt, sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}
	if resp.PaymentError == "" {
		t.Fatalf("payment should fail due to insufficient "+
			"capacity: %v", err)
	} else if !strings.Contains(resp.PaymentError,
		lnwire.CodeTemporaryChannelFailure.String()) {
		t.Fatalf("payment should fail due to insufficient capacity, "+
			"instead: %v", resp.PaymentError)
	}

	// For our final test, we'll ensure that if a target link isn't
	// available for what ever reason then the payment fails accordingly.
	//
	// We'll attempt to complete the original invoice we created with Carol
	// above, but before we do so, Carol will go offline, resulting in a
	// failed payment.
	shutdownAndAssert(net, t, carol)

	// TODO(roasbeef): mission control
	time.Sleep(time.Second * 5)

	sendReq = &lnrpc.SendRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	resp, err = net.Alice.SendPaymentSync(ctxt, sendReq)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	if resp.PaymentError == "" {
		t.Fatalf("payment should have failed")
	}
	expectedErrorCode = lnwire.CodeUnknownNextPeer.String()
	if !strings.Contains(resp.PaymentError, expectedErrorCode) {
		t.Fatalf("payment should fail due to unknown hop, instead: %v",
			resp.PaymentError)
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

// subscribeGraphNotifications subscribes to channel graph updates and launches
// a goroutine that forwards these to the returned channel.
func subscribeGraphNotifications(t *harnessTest, ctxb context.Context,
	node *lntest.HarnessNode) (chan *lnrpc.GraphTopologyUpdate, chan struct{}) {
	// We'll first start by establishing a notification client which will
	// send us notifications upon detected changes in the channel graph.
	req := &lnrpc.GraphTopologySubscription{}
	topologyClient, err := node.SubscribeChannelGraph(ctxb, req)
	if err != nil {
		t.Fatalf("unable to create topology client: %v", err)
	}

	// We'll launch a goroutine that will be responsible for proxying all
	// notifications recv'd from the client into the channel below.
	quit := make(chan struct{})
	graphUpdates := make(chan *lnrpc.GraphTopologyUpdate, 20)
	go func() {
		for {
			select {
			case <-quit:
				return
			default:
				graphUpdate, err := topologyClient.Recv()
				select {
				case <-quit:
					return
				default:
				}

				if err == io.EOF {
					return
				} else if err != nil {
					t.Fatalf("unable to recv graph update: %v",
						err)
				}

				select {
				case graphUpdates <- graphUpdate:
				case <-quit:
					return
				}
			}
		}
	}()
	return graphUpdates, quit
}

func testGraphTopologyNotifications(net *lntest.NetworkHarness, t *harnessTest) {
	const chanAmt = maxBtcFundingAmount
	timeout := time.Duration(time.Second * 5)
	ctxb := context.Background()

	// Let Alice subscribe to graph notifications.
	graphUpdates, quit := subscribeGraphNotifications(t, ctxb, net.Alice)

	// Open a new channel between Alice and Bob.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, chanAmt, 0, false,
	)

	// The channel opening above should have triggered a few notifications
	// sent to the notification client. We'll expect two channel updates,
	// and two node announcements.
	const numExpectedUpdates = 4
	for i := 0; i < numExpectedUpdates; i++ {
		select {
		// Ensure that a new update for both created edges is properly
		// dispatched to our registered client.
		case graphUpdate := <-graphUpdates:

			if len(graphUpdate.ChannelUpdates) > 0 {
				chanUpdate := graphUpdate.ChannelUpdates[0]
				if chanUpdate.Capacity != int64(chanAmt) {
					t.Fatalf("channel capacities mismatch:"+
						" expected %v, got %v", chanAmt,
						btcutil.Amount(chanUpdate.Capacity))
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
			}

			if len(graphUpdate.NodeUpdates) > 0 {
				nodeUpdate := graphUpdate.NodeUpdates[0]
				switch nodeUpdate.IdentityKey {
				case net.Alice.PubKeyStr:
				case net.Bob.PubKeyStr:
				default:
					t.Fatalf("unknown node: %v",
						nodeUpdate.IdentityKey)
				}
			}
		case <-time.After(time.Second * 10):
			t.Fatalf("timeout waiting for graph notification %v", i)
		}
	}

	_, blockHeight, err := net.Miner.Node.GetBestBlock()
	if err != nil {
		t.Fatalf("unable to get current blockheight %v", err)
	}

	// Now we'll test that updates are properly sent after channels are closed
	// within the network.
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
		chanPointTxid, err := getChanPointFundingTxid(chanPoint)
		if err != nil {
			t.Fatalf("unable to get txid: %v", err)
		}
		closedChanTxid, err := getChanPointFundingTxid(closedChan.ChanPoint)
		if err != nil {
			t.Fatalf("unable to get txid: %v", err)
		}
		if !bytes.Equal(closedChanTxid, chanPointTxid) {
			t.Fatalf("channel point hash mismatch: expected %v, "+
				"got %v", chanPointTxid, closedChanTxid)
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
	// appears in the network, the proper notification is dispatched. Note
	// that a node that does not have any channels open is ignored, so first
	// we disconnect Alice and Bob, open a channel between Bob and Carol,
	// and finally connect Alice to Bob again.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.DisconnectNodes(ctxt, net.Alice, net.Bob); err != nil {
		t.Fatalf("unable to disconnect alice and bob: %v", err)
	}
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	if err := net.ConnectNodes(ctxb, net.Bob, carol); err != nil {
		t.Fatalf("unable to connect bob to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPoint = openChannelAndAssert(
		ctxt, t, net, net.Bob, carol, chanAmt, 0, false,
	)

	// Reconnect Alice and Bob. This should result in the nodes syncing up
	// their respective graph state, with the new addition being the
	// existence of Carol in the graph, and also the channel between Bob
	// and Carol. Note that we will also receive a node announcement from
	// Bob, since a node will update its node announcement after a new
	// channel is opened.
	if err := net.EnsureConnected(ctxb, net.Alice, net.Bob); err != nil {
		t.Fatalf("unable to connect alice to bob: %v", err)
	}

	// We should receive an update advertising the newly connected node,
	// Bob's new node announcement, and the channel between Bob and Carol.
	for i := 0; i < 3; i++ {
		select {
		case graphUpdate := <-graphUpdates:
			if len(graphUpdate.NodeUpdates) > 0 {
				nodeUpdate := graphUpdate.NodeUpdates[0]
				switch nodeUpdate.IdentityKey {
				case carol.PubKeyStr:
				case net.Bob.PubKeyStr:
				default:
					t.Fatalf("unknown node update pubey: %v",
						nodeUpdate.IdentityKey)
				}
			}

			if len(graphUpdate.ChannelUpdates) > 0 {
				chanUpdate := graphUpdate.ChannelUpdates[0]
				if chanUpdate.Capacity != int64(chanAmt) {
					t.Fatalf("channel capacities mismatch:"+
						" expected %v, got %v", chanAmt,
						btcutil.Amount(chanUpdate.Capacity))
				}
				switch chanUpdate.AdvertisingNode {
				case carol.PubKeyStr:
				case net.Bob.PubKeyStr:
				default:
					t.Fatalf("unknown advertising node: %v",
						chanUpdate.AdvertisingNode)
				}
				switch chanUpdate.ConnectingNode {
				case carol.PubKeyStr:
				case net.Bob.PubKeyStr:
				default:
					t.Fatalf("unknown connecting node: %v",
						chanUpdate.ConnectingNode)
				}
			}
		case <-time.After(time.Second * 10):
			t.Fatalf("timeout waiting for graph notification %v", i)
		}
	}

	// Close the channel between Bob and Carol.
	ctxt, _ = context.WithTimeout(context.Background(), timeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, chanPoint, false)

	close(quit)
}

// testNodeAnnouncement ensures that when a node is started with one or more
// external IP addresses specified on the command line, that those addresses
// announced to the network and reported in the network graph.
func testNodeAnnouncement(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	aliceUpdates, quit := subscribeGraphNotifications(t, ctxb, net.Alice)
	defer close(quit)

	advertisedAddrs := []string{
		"192.168.1.1:8333",
		"[2001:db8:85a3:8d3:1319:8a2e:370:7348]:8337",
	}

	var lndArgs []string
	for _, addr := range advertisedAddrs {
		lndArgs = append(lndArgs, "--externalip="+addr)
	}

	dave, err := net.NewNode("Dave", lndArgs)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	// We must let Dave have an open channel before he can send a node
	// announcement, so we open a channel with Bob,
	if err := net.ConnectNodes(ctxb, net.Bob, dave); err != nil {
		t.Fatalf("unable to connect bob to carol: %v", err)
	}

	timeout := time.Duration(time.Second * 5)
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Bob, dave, 1000000, 0, false,
	)

	// When Alice now connects with Dave, Alice will get his node
	// announcement.
	if err := net.ConnectNodes(ctxb, net.Alice, dave); err != nil {
		t.Fatalf("unable to connect bob to carol: %v", err)
	}

	assertAddrs := func(addrsFound []string, targetAddrs ...string) {
		addrs := make(map[string]struct{}, len(addrsFound))
		for _, addr := range addrsFound {
			addrs[addr] = struct{}{}
		}

		for _, addr := range targetAddrs {
			if _, ok := addrs[addr]; !ok {
				t.Fatalf("address %v not found in node "+
					"announcement", addr)
			}
		}
	}

	waitForAddrsInUpdate := func(graphUpdates <-chan *lnrpc.GraphTopologyUpdate,
		nodePubKey string, targetAddrs ...string) {

		for {
			select {
			case graphUpdate := <-graphUpdates:
				for _, update := range graphUpdate.NodeUpdates {
					if update.IdentityKey == nodePubKey {
						assertAddrs(
							update.Addresses,
							targetAddrs...,
						)
						return
					}
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("did not receive node ann update")
			}
		}
	}

	waitForAddrsInUpdate(aliceUpdates, dave.PubKeyStr, advertisedAddrs...)

	// Close the channel between Bob and Dave.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, chanPoint, false)
}

func testNodeSignVerify(net *lntest.NetworkHarness, t *harnessTest) {
	timeout := time.Duration(time.Second * 15)
	ctxb := context.Background()

	chanAmt := maxBtcFundingAmount
	pushAmt := btcutil.Amount(100000)

	// Create a channel between alice and bob.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	aliceBobCh := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, chanAmt, pushAmt, false,
	)

	aliceMsg := []byte("alice msg")

	// alice signs "alice msg" and sends her signature to bob.
	sigReq := &lnrpc.SignMessageRequest{Msg: aliceMsg}
	sigResp, err := net.Alice.SignMessage(ctxb, sigReq)
	if err != nil {
		t.Fatalf("SignMessage rpc call failed: %v", err)
	}
	aliceSig := sigResp.Signature

	// bob verifying alice's signature should succeed since alice and bob are
	// connected.
	verifyReq := &lnrpc.VerifyMessageRequest{Msg: aliceMsg, Signature: aliceSig}
	verifyResp, err := net.Bob.VerifyMessage(ctxb, verifyReq)
	if err != nil {
		t.Fatalf("VerifyMessage failed: %v", err)
	}
	if !verifyResp.Valid {
		t.Fatalf("alice's signature didn't validate")
	}
	if verifyResp.Pubkey != net.Alice.PubKeyStr {
		t.Fatalf("alice's signature doesn't contain alice's pubkey.")
	}

	// carol is a new node that is unconnected to alice or bob.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	carolMsg := []byte("carol msg")

	// carol signs "carol msg" and sends her signature to bob.
	sigReq = &lnrpc.SignMessageRequest{Msg: carolMsg}
	sigResp, err = carol.SignMessage(ctxb, sigReq)
	if err != nil {
		t.Fatalf("SignMessage rpc call failed: %v", err)
	}
	carolSig := sigResp.Signature

	// bob verifying carol's signature should fail since they are not connected.
	verifyReq = &lnrpc.VerifyMessageRequest{Msg: carolMsg, Signature: carolSig}
	verifyResp, err = net.Bob.VerifyMessage(ctxb, verifyReq)
	if err != nil {
		t.Fatalf("VerifyMessage failed: %v", err)
	}
	if verifyResp.Valid {
		t.Fatalf("carol's signature should not be valid")
	}
	if verifyResp.Pubkey != carol.PubKeyStr {
		t.Fatalf("carol's signature doesn't contain her pubkey")
	}

	// Close the channel between alice and bob.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, aliceBobCh, false)
}

// testAsyncPayments tests the performance of the async payments, and also
// checks that balances of both sides can't be become negative under stress
// payment strikes.
func testAsyncPayments(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// As we'll be querying the channels  state frequently we'll
	// create a closure helper function for the purpose.
	getChanInfo := func(node *lntest.HarnessNode) (*lnrpc.Channel, error) {
		req := &lnrpc.ListChannelsRequest{}
		channelInfo, err := node.ListChannels(ctxb, req)
		if err != nil {
			return nil, err
		}
		if len(channelInfo.Channels) != 1 {
			t.Fatalf("node should only have a single channel, "+
				"instead he has %v",
				len(channelInfo.Channels))
		}

		return channelInfo.Channels[0], nil
	}

	const (
		timeout    = time.Duration(time.Second * 5)
		paymentAmt = 100
	)

	// First establish a channel with a capacity equals to the overall
	// amount of payments, between Alice and Bob, at the end of the test
	// Alice should send all money from her side to Bob.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	channelCapacity := btcutil.Amount(paymentAmt * 2000)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, channelCapacity, 0, false,
	)

	info, err := getChanInfo(net.Alice)
	if err != nil {
		t.Fatalf("unable to get alice channel info: %v", err)
	}

	// Calculate the number of invoices. We will deplete the channel
	// all the way down to the channel reserve.
	chanReserve := channelCapacity / 100
	availableBalance := btcutil.Amount(info.LocalBalance) - chanReserve
	numInvoices := int(availableBalance / paymentAmt)

	bobAmt := int64(numInvoices * paymentAmt)
	aliceAmt := info.LocalBalance - bobAmt

	// Send one more payment in order to cause insufficient capacity error.
	numInvoices++

	// Initialize seed random in order to generate invoices.
	prand.Seed(time.Now().UnixNano())

	// With the channel open, we'll create invoices for Bob that Alice
	// will pay to in order to advance the state of the channel.
	bobPayReqs := make([]string, numInvoices)
	for i := 0; i < numInvoices; i++ {
		preimage := make([]byte, 32)
		_, err := rand.Read(preimage)
		if err != nil {
			t.Fatalf("unable to generate preimage: %v", err)
		}

		invoice := &lnrpc.Invoice{
			Memo:      "testing",
			RPreimage: preimage,
			Value:     paymentAmt,
		}
		resp, err := net.Bob.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		bobPayReqs[i] = resp.PaymentRequest
	}

	// Wait for Alice to receive the channel edge from the funding manager.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint)
	if err != nil {
		t.Fatalf("alice didn't see the alice->bob channel before "+
			"timeout: %v", err)
	}

	// Open up a payment stream to Alice that we'll use to send payment to
	// Bob. We also create a small helper function to send payments to Bob,
	// consuming the payment hashes we generated above.
	ctxt, _ = context.WithTimeout(ctxb, time.Minute)
	alicePayStream, err := net.Alice.SendPayment(ctxt)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}

	// Send payments from Alice to Bob using of Bob's payment hashes
	// generated above.
	now := time.Now()
	for i := 0; i < numInvoices; i++ {
		sendReq := &lnrpc.SendRequest{
			PaymentRequest: bobPayReqs[i],
		}

		if err := alicePayStream.Send(sendReq); err != nil {
			t.Fatalf("unable to send payment: "+
				"stream has been closed: %v", err)
		}
	}

	// We should receive one insufficient capacity error, because we sent
	// one more payment than we can actually handle with the current
	// channel capacity.
	errorReceived := false
	for i := 0; i < numInvoices; i++ {
		if resp, err := alicePayStream.Recv(); err != nil {
			t.Fatalf("payment stream have been closed: %v", err)
		} else if resp.PaymentError != "" {
			if errorReceived {
				t.Fatalf("redundant payment error: %v",
					resp.PaymentError)
			}

			errorReceived = true
			continue
		}
	}

	if !errorReceived {
		t.Fatalf("insufficient capacity error haven't been received")
	}

	// All payments have been sent, mark the finish time.
	timeTaken := time.Since(now)

	// Next query for Bob's and Alice's channel states, in order to confirm
	// that all payment have been successful transmitted.
	aliceChan, err := getChanInfo(net.Alice)
	if len(aliceChan.PendingHtlcs) != 0 {
		t.Fatalf("alice's pending htlcs is incorrect, got %v, "+
			"expected %v", len(aliceChan.PendingHtlcs), 0)
	}
	if err != nil {
		t.Fatalf("unable to get bob's channel info: %v", err)
	}
	if aliceChan.RemoteBalance != bobAmt {
		t.Fatalf("alice's remote balance is incorrect, got %v, "+
			"expected %v", aliceChan.RemoteBalance, bobAmt)
	}
	if aliceChan.LocalBalance != aliceAmt {
		t.Fatalf("alice's local balance is incorrect, got %v, "+
			"expected %v", aliceChan.LocalBalance, aliceAmt)
	}

	// Wait for Bob to receive revocation from Alice.
	time.Sleep(2 * time.Second)

	bobChan, err := getChanInfo(net.Bob)
	if err != nil {
		t.Fatalf("unable to get bob's channel info: %v", err)
	}
	if len(bobChan.PendingHtlcs) != 0 {
		t.Fatalf("bob's pending htlcs is incorrect, got %v, "+
			"expected %v", len(bobChan.PendingHtlcs), 0)
	}
	if bobChan.LocalBalance != bobAmt {
		t.Fatalf("bob's local balance is incorrect, got %v, expected"+
			" %v", bobChan.LocalBalance, bobAmt)
	}
	if bobChan.RemoteBalance != aliceAmt {
		t.Fatalf("bob's remote balance is incorrect, got %v, "+
			"expected %v", bobChan.RemoteBalance, aliceAmt)
	}

	t.Log("\tBenchmark info: Elapsed time: ", timeTaken)
	t.Log("\tBenchmark info: TPS: ", float64(numInvoices)/float64(timeTaken.Seconds()))

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

// testBidirectionalAsyncPayments tests that nodes are able to send the
// payments to each other in async manner without blocking.
func testBidirectionalAsyncPayments(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()

	// As we'll be querying the channels  state frequently we'll
	// create a closure helper function for the purpose.
	getChanInfo := func(node *lntest.HarnessNode) (*lnrpc.Channel, error) {
		req := &lnrpc.ListChannelsRequest{}
		channelInfo, err := node.ListChannels(ctxb, req)
		if err != nil {
			return nil, err
		}
		if len(channelInfo.Channels) != 1 {
			t.Fatalf("node should only have a single channel, "+
				"instead he has %v",
				len(channelInfo.Channels))
		}

		return channelInfo.Channels[0], nil
	}

	const (
		timeout    = time.Duration(time.Second * 5)
		paymentAmt = 1000
	)

	// First establish a channel with a capacity equals to the overall
	// amount of payments, between Alice and Bob, at the end of the test
	// Alice should send all money from her side to Bob.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, paymentAmt*2000,
		paymentAmt*1000, false,
	)

	info, err := getChanInfo(net.Alice)
	if err != nil {
		t.Fatalf("unable to get alice channel info: %v", err)
	}

	// Calculate the number of invoices.
	numInvoices := int(info.LocalBalance / paymentAmt)

	// Nodes should exchange the same amount of money and because of this
	// at the end balances should remain the same.
	aliceAmt := info.LocalBalance
	bobAmt := info.RemoteBalance

	// Initialize seed random in order to generate invoices.
	prand.Seed(time.Now().UnixNano())

	// With the channel open, we'll create invoices for Bob that Alice
	// will pay to in order to advance the state of the channel.
	bobPayReqs := make([]string, numInvoices)
	for i := 0; i < numInvoices; i++ {
		preimage := make([]byte, 32)
		_, err := rand.Read(preimage)
		if err != nil {
			t.Fatalf("unable to generate preimage: %v", err)
		}

		invoice := &lnrpc.Invoice{
			Memo:      "testing",
			RPreimage: preimage,
			Value:     paymentAmt,
		}
		resp, err := net.Bob.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		bobPayReqs[i] = resp.PaymentRequest
	}

	// With the channel open, we'll create invoices for Alice that Bob
	// will pay to in order to advance the state of the channel.
	alicePayReqs := make([]string, numInvoices)
	for i := 0; i < numInvoices; i++ {
		preimage := make([]byte, 32)
		_, err := rand.Read(preimage)
		if err != nil {
			t.Fatalf("unable to generate preimage: %v", err)
		}

		invoice := &lnrpc.Invoice{
			Memo:      "testing",
			RPreimage: preimage,
			Value:     paymentAmt,
		}
		resp, err := net.Alice.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		alicePayReqs[i] = resp.PaymentRequest
	}

	// Wait for Alice to receive the channel edge from the funding manager.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err = net.Alice.WaitForNetworkChannelOpen(ctxt, chanPoint); err != nil {
		t.Fatalf("alice didn't see the alice->bob channel before "+
			"timeout: %v", err)
	}
	if err = net.Bob.WaitForNetworkChannelOpen(ctxt, chanPoint); err != nil {
		t.Fatalf("bob didn't see the bob->alice channel before "+
			"timeout: %v", err)
	}

	// Open up a payment streams to Alice and to Bob, that we'll use to
	// send payment between nodes.
	alicePayStream, err := net.Alice.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}

	bobPayStream, err := net.Bob.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create payment stream for bob: %v", err)
	}

	// Send payments from Alice to Bob and from Bob to Alice in async
	// manner.
	for i := 0; i < numInvoices; i++ {
		aliceSendReq := &lnrpc.SendRequest{
			PaymentRequest: bobPayReqs[i],
		}

		bobSendReq := &lnrpc.SendRequest{
			PaymentRequest: alicePayReqs[i],
		}

		if err := alicePayStream.Send(aliceSendReq); err != nil {
			t.Fatalf("unable to send payment: "+
				"%v", err)
		}

		if err := bobPayStream.Send(bobSendReq); err != nil {
			t.Fatalf("unable to send payment: "+
				"%v", err)
		}
	}

	errChan := make(chan error)
	go func() {
		for i := 0; i < numInvoices; i++ {
			if resp, err := alicePayStream.Recv(); err != nil {
				errChan <- errors.Errorf("payment stream has"+
					" been closed: %v", err)
				return
			} else if resp.PaymentError != "" {
				errChan <- errors.Errorf("unable to send "+
					"payment from alice to bob: %v",
					resp.PaymentError)
				return
			}
		}
		errChan <- nil
	}()

	go func() {
		for i := 0; i < numInvoices; i++ {
			if resp, err := bobPayStream.Recv(); err != nil {
				errChan <- errors.Errorf("payment stream has"+
					" been closed: %v", err)
				return
			} else if resp.PaymentError != "" {
				errChan <- errors.Errorf("unable to send "+
					"payment from bob to alice: %v",
					resp.PaymentError)
				return
			}
		}
		errChan <- nil
	}()

	// Wait for Alice and Bob receive their payments, and throw and error
	// if something goes wrong.
	maxTime := 60 * time.Second
	for i := 0; i < 2; i++ {
		select {
		case err := <-errChan:
			if err != nil {
				t.Fatalf(err.Error())
			}
		case <-time.After(maxTime):
			t.Fatalf("waiting for payments to finish too long "+
				"(%v)", maxTime)
		}
	}

	// Wait for Alice and Bob to receive revocations messages, and update
	// states, i.e. balance info.
	time.Sleep(1 * time.Second)

	aliceInfo, err := getChanInfo(net.Alice)
	if err != nil {
		t.Fatalf("unable to get bob's channel info: %v", err)
	}
	if aliceInfo.RemoteBalance != bobAmt {
		t.Fatalf("alice's remote balance is incorrect, got %v, "+
			"expected %v", aliceInfo.RemoteBalance, bobAmt)
	}
	if aliceInfo.LocalBalance != aliceAmt {
		t.Fatalf("alice's local balance is incorrect, got %v, "+
			"expected %v", aliceInfo.LocalBalance, aliceAmt)
	}
	if len(aliceInfo.PendingHtlcs) != 0 {
		t.Fatalf("alice's pending htlcs is incorrect, got %v, "+
			"expected %v", len(aliceInfo.PendingHtlcs), 0)
	}

	// Next query for Bob's and Alice's channel states, in order to confirm
	// that all payment have been successful transmitted.
	bobInfo, err := getChanInfo(net.Bob)
	if err != nil {
		t.Fatalf("unable to get bob's channel info: %v", err)
	}

	if bobInfo.LocalBalance != bobAmt {
		t.Fatalf("bob's local balance is incorrect, got %v, expected"+
			" %v", bobInfo.LocalBalance, bobAmt)
	}
	if bobInfo.RemoteBalance != aliceAmt {
		t.Fatalf("bob's remote balance is incorrect, got %v, "+
			"expected %v", bobInfo.RemoteBalance, aliceAmt)
	}
	if len(bobInfo.PendingHtlcs) != 0 {
		t.Fatalf("bob's pending htlcs is incorrect, got %v, "+
			"expected %v", len(bobInfo.PendingHtlcs), 0)
	}

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPoint, false)
}

// assertActiveHtlcs makes sure all the passed nodes have the _exact_ HTLCs
// matching payHashes on _all_ their channels.
func assertActiveHtlcs(nodes []*lntest.HarnessNode, payHashes ...[]byte) error {
	req := &lnrpc.ListChannelsRequest{}
	ctxb := context.Background()
	for _, node := range nodes {
		nodeChans, err := node.ListChannels(ctxb, req)
		if err != nil {
			return fmt.Errorf("unable to get node chans: %v", err)
		}

		for _, channel := range nodeChans.Channels {
			// Record all payment hashes active for this channel.
			htlcHashes := make(map[string]struct{})
			for _, htlc := range channel.PendingHtlcs {
				_, ok := htlcHashes[string(htlc.HashLock)]
				if ok {
					return fmt.Errorf("duplicate HashLock")
				}
				htlcHashes[string(htlc.HashLock)] = struct{}{}
			}

			// Channel should have exactly the payHashes active.
			if len(payHashes) != len(htlcHashes) {
				return fmt.Errorf("node %x had %v htlcs active, "+
					"expected %v", node.PubKey[:],
					len(htlcHashes), len(payHashes))
			}

			// Make sure all the payHashes are active.
			for _, payHash := range payHashes {
				if _, ok := htlcHashes[string(payHash)]; ok {
					continue
				}
				return fmt.Errorf("node %x didn't have the "+
					"payHash %v active", node.PubKey[:],
					payHash)
			}
		}
	}

	return nil
}

func assertNumActiveHtlcsChanPoint(node *lntest.HarnessNode,
	chanPoint wire.OutPoint, numHtlcs int) bool {

	req := &lnrpc.ListChannelsRequest{}
	ctxb := context.Background()
	nodeChans, err := node.ListChannels(ctxb, req)
	if err != nil {
		return false
	}

	for _, channel := range nodeChans.Channels {
		if channel.ChannelPoint != chanPoint.String() {
			continue
		}

		return len(channel.PendingHtlcs) == numHtlcs
	}

	return false
}

func assertNumActiveHtlcs(nodes []*lntest.HarnessNode, numHtlcs int) bool {
	req := &lnrpc.ListChannelsRequest{}
	ctxb := context.Background()
	for _, node := range nodes {
		nodeChans, err := node.ListChannels(ctxb, req)
		if err != nil {
			return false
		}

		for _, channel := range nodeChans.Channels {
			if len(channel.PendingHtlcs) != numHtlcs {
				return false
			}
		}
	}

	return true
}

func assertSpendingTxInMempool(t *harnessTest, miner *rpcclient.Client,
	timeout time.Duration, chanPoint wire.OutPoint) {

	breakTimeout := time.After(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-breakTimeout:
			t.Fatalf("didn't find tx in mempool")
		case <-ticker.C:
			mempool, err := miner.GetRawMempool()
			if err != nil {
				t.Fatalf("unable to get mempool: %v", err)
			}

			if len(mempool) == 0 {
				continue
			}

			for _, txid := range mempool {
				tx, err := miner.GetRawTransaction(txid)
				if err != nil {
					t.Fatalf("unable to fetch tx: %v", err)
				}

				for _, txIn := range tx.MsgTx().TxIn {
					if txIn.PreviousOutPoint == chanPoint {
						return
					}
				}
			}
		}
	}
}

func createThreeHopHodlNetwork(t *harnessTest,
	net *lntest.NetworkHarness) (*lnrpc.ChannelPoint, *lnrpc.ChannelPoint, *lntest.HarnessNode) {

	// We'll start the test by creating a channel between Alice and Bob,
	// which will act as the first leg for out multi-hop HTLC.
	const chanAmt = 1000000
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 15)
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	aliceChanPoint := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, chanAmt, 0, false,
	)

	ctxt, _ = context.WithTimeout(ctxb, time.Second*15)
	err := net.Alice.WaitForNetworkChannelOpen(ctxt, aliceChanPoint)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, time.Second*15)
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, aliceChanPoint)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}

	// Next, we'll create a new node "carol" and have Bob connect to her.
	// In this test, we'll make carol always hold onto the HTLC, this way
	// it'll force Bob to go to chain to resolve the HTLC.
	carol, err := net.NewNode("Carol", []string{"--debughtlc", "--hodl.exit-settle"})
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	if err := net.ConnectNodes(ctxb, net.Bob, carol); err != nil {
		t.Fatalf("unable to connect bob to carol: %v", err)
	}

	// We'll then create a channel from Bob to Carol. After this channel is
	// open, our topology looks like:  A -> B -> C.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	bobChanPoint := openChannelAndAssert(
		ctxt, t, net, net.Bob, carol, chanAmt, 0, false,
	)
	ctxt, _ = context.WithTimeout(ctxb, time.Second*15)
	err = net.Bob.WaitForNetworkChannelOpen(ctxt, bobChanPoint)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, time.Second*15)
	err = carol.WaitForNetworkChannelOpen(ctxt, bobChanPoint)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, time.Second*15)
	err = net.Alice.WaitForNetworkChannelOpen(ctxt, bobChanPoint)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}

	return aliceChanPoint, bobChanPoint, carol
}

// testMultiHopHtlcLocalTimeout tests that in a multi-hop HTLC scenario, if the
// outgoing HTLC is about to time out, then we'll go to chain in order to claim
// it. Any dust HTLC's should be immediately cancelled backwards. Once the
// timeout has been reached, then we should sweep it on-chain, and cancel the
// HTLC backwards.
func testMultiHopHtlcLocalTimeout(net *lntest.NetworkHarness, t *harnessTest) {
	timeout := time.Duration(time.Second * 15)
	ctxb := context.Background()

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopHodlNetwork(t, net)

	// Clean up carol's node when the test finishes.
	defer shutdownAndAssert(net, t, carol)

	time.Sleep(time.Second * 1)

	// Now that our channels are set up, we'll send two HTLC's from Alice
	// to Carol. The first HTLC will be universally considered "dust",
	// while the second will be a proper fully valued HTLC.
	const (
		dustHtlcAmt    = btcutil.Amount(100)
		htlcAmt        = btcutil.Amount(30000)
		finalCltvDelta = 40
		csvDelay       = 4
	)
	alicePayStream, err := net.Alice.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}

	// We'll create two random payment hashes unknown to carol, then send
	// each of them by manually specifying the HTLC details.
	carolPubKey := carol.PubKey[:]
	dustPayHash := bytes.Repeat([]byte{1}, 32)
	payHash := bytes.Repeat([]byte{2}, 32)
	err = alicePayStream.Send(&lnrpc.SendRequest{
		Dest:           carolPubKey,
		Amt:            int64(dustHtlcAmt),
		PaymentHash:    dustPayHash,
		FinalCltvDelta: finalCltvDelta,
	})
	if err != nil {
		t.Fatalf("unable to send alice htlc: %v", err)
	}
	err = alicePayStream.Send(&lnrpc.SendRequest{
		Dest:           carolPubKey,
		Amt:            int64(htlcAmt),
		PaymentHash:    payHash,
		FinalCltvDelta: finalCltvDelta,
	})
	if err != nil {
		t.Fatalf("unable to send alice htlc: %v", err)
	}

	// Verify that all nodes in the path now have two HTLC's with the
	// proper parameters.
	var predErr error
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol}
	err = lntest.WaitPredicate(func() bool {
		predErr = assertActiveHtlcs(nodes, dustPayHash, payHash)
		if predErr != nil {
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// We'll now mine enough blocks to trigger Bob's broadcast of his
	// commitment transaction due to the fact that the HTLC is about to
	// timeout.
	numBlocks := uint32(finalCltvDelta - defaultBroadcastDelta)
	if _, err := net.Miner.Node.Generate(numBlocks); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// Bob's force close transaction should now be found in the mempool.
	txidHash, err := getChanPointFundingTxid(bobChanPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	bobFundingTxid, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	assertSpendingTxInMempool(
		t, net.Miner.Node, time.Second*10, wire.OutPoint{
			Hash:  *bobFundingTxid,
			Index: bobChanPoint.OutputIndex,
		},
	)

	// Mine a block to confirm the closing transaction.
	mineBlocks(t, net, 1)

	// At this point, Bob should have cancelled backwards the dust HTLC
	// that we sent earlier. This means Alice should now only have a single
	// HTLC on her channel.
	nodes = []*lntest.HarnessNode{net.Alice}
	err = lntest.WaitPredicate(func() bool {
		predErr = assertActiveHtlcs(nodes, payHash)
		if predErr != nil {
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// We'll mine csvDelay blocks in order to generate the sweep transaction
	// of Bob's funding output.
	if _, err := net.Miner.Node.Generate(csvDelay); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	_, err = waitForTxInMempool(net.Miner.Node, 10*time.Second)
	if err != nil {
		t.Fatalf("unable to find bob's funding output sweep tx: %v", err)
	}

	// We'll now mine the remaining blocks to cause the HTLC itself to
	// timeout.
	if _, err := net.Miner.Node.Generate(defaultBroadcastDelta - csvDelay); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// The second layer HTLC timeout transaction should now have been
	// broadcast on-chain.
	secondLayerHash, err := waitForTxInMempool(net.Miner.Node, time.Second*10)
	if err != nil {
		t.Fatalf("unable to find bob's second layer transaction")
	}

	// Bob's pending channel report should show that he has a commitment
	// output awaiting sweeping, and also that there's an outgoing HTLC
	// output pending.
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	pendingChanResp, err := net.Bob.PendingChannels(ctxb, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}

	if len(pendingChanResp.PendingForceClosingChannels) == 0 {
		t.Fatalf("bob should have pending for close chan but doesn't")
	}
	forceCloseChan := pendingChanResp.PendingForceClosingChannels[0]
	if forceCloseChan.LimboBalance == 0 {
		t.Fatalf("bob should have nonzero limbo balance instead "+
			"has: %v", forceCloseChan.LimboBalance)
	}
	if len(forceCloseChan.PendingHtlcs) == 0 {
		t.Fatalf("bob should have pending htlc but doesn't")
	}

	// Now we'll mine an additional block.
	block := mineBlocks(t, net, 1)[0]

	// The block should have confirmed Bob's second layer sweeping
	// transaction. Therefore, at this point, there should be no active
	// HTLC's on the commitment transaction from Alice -> Bob.
	assertTxInBlock(t, block, secondLayerHash)
	nodes = []*lntest.HarnessNode{net.Alice}
	err = lntest.WaitPredicate(func() bool {
		return assertNumActiveHtlcs(nodes, 0)
	}, time.Second*15)
	if err != nil {
		t.Fatalf("alice's channel still has active htlc's")
	}

	// At this point, Bob should show that the pending HTLC has advanced to
	// the second stage and is to be swept.
	pendingChanResp, err = net.Bob.PendingChannels(ctxb, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}
	forceCloseChan = pendingChanResp.PendingForceClosingChannels[0]
	if forceCloseChan.PendingHtlcs[0].Stage != 2 {
		t.Fatalf("bob's htlc should have advanced to the second stage: %v", err)
	}

	// We'll now mine four more blocks. After the 4th block, a transaction
	// sweeping the HTLC output should be broadcast.
	if _, err := net.Miner.Node.Generate(4); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}
	_, err = waitForTxInMempool(net.Miner.Node, time.Second*10)
	if err != nil {
		t.Fatalf("unable to find bob's sweeping transaction: %v", err)
	}

	// Next, we'll mine a final block that should confirm the second-layer
	// sweeping transaction.
	if _, err := net.Miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// Once this transaction has been confirmed, Bob should detect that he
	// no longer has any pending channels.
	err = lntest.WaitPredicate(func() bool {
		pendingChanResp, err = net.Bob.PendingChannels(ctxb, pendingChansRequest)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		if len(pendingChanResp.PendingForceClosingChannels) != 0 {
			predErr = fmt.Errorf("bob still has pending "+
				"channels but shouldn't: %v",
				spew.Sdump(pendingChanResp))
			return false
		}

		return true

	}, time.Second*15)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	ctxt, _ := context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, aliceChanPoint, false)
}

// testMultiHopReceiverChainClaim tests that in the multi-hop setting, if the
// receiver of an HTLC knows the preimage, but wasn't able to settle the HTLC
// off-chain, then it goes on chain to claim the HTLC. In this scenario, the
// node that sent the outgoing HTLC should extract the preimage from the sweep
// transaction, and finish settling the HTLC backwards into the route.
func testMultiHopReceiverChainClaim(net *lntest.NetworkHarness, t *harnessTest) {
	timeout := time.Duration(time.Second * 15)
	ctxb := context.Background()

	defaultCSV := uint32(4)

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopHodlNetwork(t, net)

	// Clean up carol's node when the test finishes.
	defer shutdownAndAssert(net, t, carol)

	// With the network active, we'll now add a new invoice at Carol's end.
	invoiceReq := &lnrpc.Invoice{
		Value: 100000,
	}
	carolInvoice, err := carol.AddInvoice(ctxb, invoiceReq)
	if err != nil {
		t.Fatalf("unable to generate carol invoice: %v", err)
	}

	// Now that we've created the invoice, we'll send a single payment from
	// Alice to Carol. We won't wait for the response however, as Carol
	// will not immediately settle the payment.
	alicePayStream, err := net.Alice.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}
	err = alicePayStream.Send(&lnrpc.SendRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
	})
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// At this point, all 3 nodes should now have an active channel with
	// the created HTLC pending on all of them.
	var predErr error
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol}
	err = lntest.WaitPredicate(func() bool {
		predErr = assertActiveHtlcs(nodes, carolInvoice.RHash)
		if predErr != nil {
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// Now we'll mine enough blocks to prompt carol to actually go to the
	// chain in order to sweep her HTLC since the value is high enough.
	// TODO(roasbeef): modify once go to chain policy changes
	numBlocks := uint32(defaultBitcoinTimeLockDelta - (2 * defaultBroadcastDelta))
	if _, err := net.Miner.Node.Generate(numBlocks); err != nil {
		t.Fatalf("unable to generate blocks")
	}

	// At this point, Carol should broadcast her active commitment
	// transaction in order to go to the chain and sweep her HTLC.
	txids, err := waitForNTxsInMempool(net.Miner.Node, 1, time.Second*20)
	if err != nil {
		t.Fatalf("expected transaction not found in mempool: %v", err)
	}

	txidHash, err := getChanPointFundingTxid(bobChanPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}

	bobFundingTxid, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}

	carolFundingPoint := wire.OutPoint{
		Hash:  *bobFundingTxid,
		Index: bobChanPoint.OutputIndex,
	}

	// The commitment transaction should be spending from the funding
	// transaction.
	commitHash := txids[0]
	tx, err := net.Miner.Node.GetRawTransaction(commitHash)
	if err != nil {
		t.Fatalf("unable to get txn: %v", err)
	}
	commitTx := tx.MsgTx()

	if commitTx.TxIn[0].PreviousOutPoint != carolFundingPoint {
		t.Fatalf("commit transaction not spending from expected "+
			"outpoint: %v", spew.Sdump(commitTx))
	}

	// Confirm the commitment.
	mineBlocks(t, net, 1)

	// After the force close transaction is mined, Carol should broadcast
	// her second level HTLC transaction. Bob will broadcast a sweep tx to
	// sweep his output in the channel with Carol. When Bob notices Carol's
	// second level transaction in the mempool, he will extract the
	// preimage and settle the HTLC back off-chain.
	secondLevelHashes, err := waitForNTxsInMempool(net.Miner.Node, 2,
		time.Second*15)
	if err != nil {
		t.Fatalf("transactions not found in mempool: %v", err)
	}

	// Carol's second level transaction should be spending from
	// the commitment transaction.
	var secondLevelHash *chainhash.Hash
	for _, txid := range secondLevelHashes {
		tx, err := net.Miner.Node.GetRawTransaction(txid)
		if err != nil {
			t.Fatalf("unable to get txn: %v", err)
		}

		if tx.MsgTx().TxIn[0].PreviousOutPoint.Hash == *commitHash {
			secondLevelHash = txid
		}
	}
	if secondLevelHash == nil {
		t.Fatalf("Carol's second level tx not found")
	}

	// We'll now mine an additional block which should confirm both the
	// second layer transactions.
	if _, err := net.Miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	time.Sleep(time.Second * 4)

	// TODO(roasbeef): assert bob pending state as well

	// Carol's pending channel report should now show two outputs under
	// limbo: her commitment output, as well as the second-layer claim
	// output.
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	pendingChanResp, err := carol.PendingChannels(ctxb, pendingChansRequest)
	if err != nil {
		t.Fatalf("unable to query for pending channels: %v", err)
	}

	if len(pendingChanResp.PendingForceClosingChannels) == 0 {
		t.Fatalf("carol should have pending for close chan but doesn't")
	}
	forceCloseChan := pendingChanResp.PendingForceClosingChannels[0]
	if forceCloseChan.LimboBalance == 0 {
		t.Fatalf("carol should have nonzero limbo balance instead "+
			"has: %v", forceCloseChan.LimboBalance)
	}

	// The pending HTLC carol has should also now be in stage 2.
	if len(forceCloseChan.PendingHtlcs) != 1 {
		t.Fatalf("carol should have pending htlc but doesn't")
	}
	if forceCloseChan.PendingHtlcs[0].Stage != 2 {
		t.Fatalf("carol's htlc should have advanced to the second "+
			"stage: %v", err)
	}

	// Once the second-level transaction confirmed, Bob should have
	// extracted the preimage from the chain, and sent it back to Alice,
	// clearing the HTLC off-chain.
	nodes = []*lntest.HarnessNode{net.Alice}
	err = lntest.WaitPredicate(func() bool {
		return assertNumActiveHtlcs(nodes, 0)
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", err)
	}

	// If we mine 4 additional blocks, then both outputs should now be
	// mature.
	if _, err := net.Miner.Node.Generate(defaultCSV); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// We should have a new transaction in the mempool.
	_, err = waitForTxInMempool(net.Miner.Node, time.Second*10)
	if err != nil {
		t.Fatalf("unable to find bob's sweeping transaction: %v", err)
	}

	// Finally, if we mine an additional block to confirm these two sweep
	// transactions, Carol should not show a pending channel in her report
	// afterwards.
	if _, err := net.Miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to mine block: %v", err)
	}
	err = lntest.WaitPredicate(func() bool {
		pendingChanResp, err = carol.PendingChannels(ctxb, pendingChansRequest)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending channels: %v", err)
			return false
		}
		if len(pendingChanResp.PendingForceClosingChannels) != 0 {
			predErr = fmt.Errorf("carol still has pending channels: %v",
				spew.Sdump(pendingChanResp))
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	// We'll close out the channel between Alice and Bob, then shutdown
	// carol to conclude the test.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, aliceChanPoint, false)
}

// testMultiHopLocalForceCloseOnChainHtlcTimeout tests that in a multi-hop HTLC
// scenario, if the node that extended the HTLC to the final node closes their
// commitment on-chain early, then it eventually recognizes this HTLC as one
// that's timed out. At this point, the node should timeout the HTLC, then
// cancel it backwards as normal.
func testMultiHopLocalForceCloseOnChainHtlcTimeout(net *lntest.NetworkHarness,
	t *harnessTest) {

	timeout := time.Duration(time.Second * 15)
	ctxb := context.Background()

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopHodlNetwork(t, net)

	// Clean up carol's node when the test finishes.
	defer shutdownAndAssert(net, t, carol)

	// With our channels set up, we'll then send a single HTLC from Alice
	// to Carol. As Carol is in hodl mode, she won't settle this HTLC which
	// opens up the base for out tests.
	const (
		csvDelay       = 4
		finalCltvDelta = 40
		htlcAmt        = btcutil.Amount(30000)
	)
	alicePayStream, err := net.Alice.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}

	// We'll now send a single HTLC across our multi-hop network.
	carolPubKey := carol.PubKey[:]
	payHash := bytes.Repeat([]byte{2}, 32)
	err = alicePayStream.Send(&lnrpc.SendRequest{
		Dest:           carolPubKey,
		Amt:            int64(htlcAmt),
		PaymentHash:    payHash,
		FinalCltvDelta: finalCltvDelta,
	})
	if err != nil {
		t.Fatalf("unable to send alice htlc: %v", err)
	}

	// Once the HTLC has cleared, all channels in our mini network should
	// have the it locked in.
	var predErr error
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol}
	err = lntest.WaitPredicate(func() bool {
		predErr = assertActiveHtlcs(nodes, payHash)
		if predErr != nil {
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", err)
	}

	// Now that all parties have the HTLC locked in, we'll immediately
	// force close the Bob -> Carol channel. This should trigger contract
	// resolution mode for both of them.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, bobChanPoint, true)

	// At this point, Bob should have a pending force close channel as he
	// just went to chain.
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	err = lntest.WaitPredicate(func() bool {
		pendingChanResp, err := net.Bob.PendingChannels(ctxb,
			pendingChansRequest)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		if len(pendingChanResp.PendingForceClosingChannels) == 0 {
			predErr = fmt.Errorf("bob should have pending for " +
				"close chan but doesn't")
			return false
		}

		forceCloseChan := pendingChanResp.PendingForceClosingChannels[0]
		if forceCloseChan.LimboBalance == 0 {
			predErr = fmt.Errorf("bob should have nonzero limbo "+
				"balance instead has: %v",
				forceCloseChan.LimboBalance)
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	// We'll mine csvDelay blocks in order to generate the sweep transaction
	// of Bob's funding output.
	if _, err := net.Miner.Node.Generate(csvDelay); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	_, err = waitForTxInMempool(net.Miner.Node, 10*time.Second)
	if err != nil {
		t.Fatalf("unable to find bob's funding output sweep tx: %v", err)
	}

	// We'll now mine enough blocks for the HTLC to expire. After this, Bob
	// should hand off the now expired HTLC output to the utxo nursery.
	if _, err := net.Miner.Node.Generate(finalCltvDelta - csvDelay - 1); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// Bob's pending channel report should show that he has a single HTLC
	// that's now in stage one.
	err = lntest.WaitPredicate(func() bool {
		pendingChanResp, err := net.Bob.PendingChannels(
			ctxb, pendingChansRequest,
		)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}

		if len(pendingChanResp.PendingForceClosingChannels) == 0 {
			predErr = fmt.Errorf("bob should have pending force " +
				"close chan but doesn't")
			return false
		}

		forceCloseChan := pendingChanResp.PendingForceClosingChannels[0]
		if len(forceCloseChan.PendingHtlcs) != 1 {
			predErr = fmt.Errorf("bob should have pending htlc " +
				"but doesn't")
			return false
		}
		if forceCloseChan.PendingHtlcs[0].Stage != 1 {
			predErr = fmt.Errorf("bob's htlc should have "+
				"advanced to the first stage: %v", err)
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("bob didn't hand off time-locked HTLC: %v", predErr)
	}

	// We should also now find a transaction in the mempool, as Bob should
	// have broadcast his second layer timeout transaction.
	timeoutTx, err := waitForTxInMempool(net.Miner.Node, 10*time.Second)
	if err != nil {
		t.Fatalf("unable to find bob's htlc timeout tx: %v", err)
	}

	// Next, we'll mine an additional block. This should serve to confirm
	// the second layer timeout transaction.
	block := mineBlocks(t, net, 1)[0]
	assertTxInBlock(t, block, timeoutTx)

	// With the second layer timeout transaction confirmed, Bob should have
	// cancelled backwards the HTLC that carol sent.
	nodes = []*lntest.HarnessNode{net.Alice}
	err = lntest.WaitPredicate(func() bool {
		return assertNumActiveHtlcs(nodes, 0)
	}, time.Second*15)
	if err != nil {
		t.Fatalf("alice's channel still has active htlc's")
	}

	// Additionally, Bob should now show that HTLC as being advanced to the
	// second stage.
	err = lntest.WaitPredicate(func() bool {
		pendingChanResp, err := net.Bob.PendingChannels(
			ctxb, pendingChansRequest,
		)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}

		if len(pendingChanResp.PendingForceClosingChannels) == 0 {
			predErr = fmt.Errorf("bob should have pending for " +
				"close chan but doesn't")
			return false
		}

		forceCloseChan := pendingChanResp.PendingForceClosingChannels[0]
		if len(forceCloseChan.PendingHtlcs) != 1 {
			predErr = fmt.Errorf("bob should have pending htlc " +
				"but doesn't")
			return false
		}
		if forceCloseChan.PendingHtlcs[0].Stage != 2 {
			predErr = fmt.Errorf("bob's htlc should have "+
				"advanced to the second stage: %v", err)
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("bob didn't hand off time-locked HTLC: %v", predErr)
	}

	// We'll now mine 4 additional blocks. This should be enough for Bob's
	// CSV timelock to expire and the sweeping transaction of the HTLC to be
	// broadcast.
	if _, err := net.Miner.Node.Generate(csvDelay); err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	sweepTx, err := waitForTxInMempool(net.Miner.Node, 10*time.Second)
	if err != nil {
		t.Fatalf("unable to find bob's htlc sweep tx: %v", err)
	}

	// We'll then mine a final block which should confirm this second layer
	// sweep transaction.
	block = mineBlocks(t, net, 1)[0]
	assertTxInBlock(t, block, sweepTx)

	// At this point, Bob should no longer show any channels as pending
	// close.
	err = lntest.WaitPredicate(func() bool {
		pendingChanResp, err := net.Bob.PendingChannels(
			ctxb, pendingChansRequest,
		)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		if len(pendingChanResp.PendingForceClosingChannels) != 0 {
			predErr = fmt.Errorf("bob still has pending channels "+
				"but shouldn't: %v", spew.Sdump(pendingChanResp))
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, aliceChanPoint, false)
}

// testMultiHopRemoteForceCloseOnChainHtlcTimeout tests that if we extend a
// multi-hop HTLC, and the final destination of the HTLC force closes the
// channel, then we properly timeout the HTLC on *their* commitment transaction
// once the timeout has expired. Once we sweep the transaction, we should also
// cancel back the initial HTLC.
func testMultiHopRemoteForceCloseOnChainHtlcTimeout(net *lntest.NetworkHarness,
	t *harnessTest) {

	timeout := time.Duration(time.Second * 15)
	ctxb := context.Background()

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopHodlNetwork(t, net)

	// Clean up carol's node when the test finishes.
	defer shutdownAndAssert(net, t, carol)

	// With our channels set up, we'll then send a single HTLC from Alice
	// to Carol. As Carol is in hodl mode, she won't settle this HTLC which
	// opens up the base for out tests.
	const (
		csvDelay       = 4
		finalCltvDelta = 40
		htlcAmt        = btcutil.Amount(30000)
	)
	alicePayStream, err := net.Alice.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}

	// We'll now send a single HTLC across our multi-hop network.
	carolPubKey := carol.PubKey[:]
	payHash := bytes.Repeat([]byte{2}, 32)
	err = alicePayStream.Send(&lnrpc.SendRequest{
		Dest:           carolPubKey,
		Amt:            int64(htlcAmt),
		PaymentHash:    payHash,
		FinalCltvDelta: finalCltvDelta,
	})
	if err != nil {
		t.Fatalf("unable to send alice htlc: %v", err)
	}

	// Once the HTLC has cleared, all the nodes in our mini network should
	// show that the HTLC has been locked in.
	var predErr error
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol}
	err = lntest.WaitPredicate(func() bool {
		predErr = assertActiveHtlcs(nodes, payHash)
		if predErr != nil {
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", predErr)
	}

	// At this point, we'll now instruct Carol to force close the
	// transaction. This will let us exercise that Bob is able to sweep the
	// expired HTLC on Carol's version of the commitment transaction.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, carol, bobChanPoint, true)

	// At this point, Bob should have a pending force close channel as
	// Carol has gone directly to chain.
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	err = lntest.WaitPredicate(func() bool {
		pendingChanResp, err := net.Bob.PendingChannels(
			ctxb, pendingChansRequest,
		)
		if err != nil {
			predErr = fmt.Errorf("unable to query for "+
				"pending channels: %v", err)
			return false
		}
		if len(pendingChanResp.PendingForceClosingChannels) == 0 {
			predErr = fmt.Errorf("bob should have pending " +
				"force close channels but doesn't")
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	// We'll mine csvDelay blocks in order to generate the sweep transaction
	// of Bob's funding output.
	if _, err := net.Miner.Node.Generate(csvDelay); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	_, err = waitForTxInMempool(net.Miner.Node, 10*time.Second)
	if err != nil {
		t.Fatalf("unable to find bob's funding output sweep tx: %v", err)
	}

	// Next, we'll mine enough blocks for the HTLC to expire. At this
	// point, Bob should hand off the output to his internal utxo nursery,
	// which will broadcast a sweep transaction.
	if _, err := net.Miner.Node.Generate(finalCltvDelta - csvDelay - 1); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	// If we check Bob's pending channel report, it should show that he has
	// a single HTLC that's now in the second stage, as skip the initial
	// first stage since this is a direct HTLC.
	err = lntest.WaitPredicate(func() bool {
		pendingChanResp, err := net.Bob.PendingChannels(
			ctxb, pendingChansRequest,
		)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}

		if len(pendingChanResp.PendingForceClosingChannels) == 0 {
			predErr = fmt.Errorf("bob should have pending for " +
				"close chan but doesn't")
			return false
		}

		forceCloseChan := pendingChanResp.PendingForceClosingChannels[0]
		if len(forceCloseChan.PendingHtlcs) != 1 {
			predErr = fmt.Errorf("bob should have pending htlc " +
				"but doesn't")
			return false
		}
		if forceCloseChan.PendingHtlcs[0].Stage != 2 {
			predErr = fmt.Errorf("bob's htlc should have "+
				"advanced to the second stage: %v", err)
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("bob didn't hand off time-locked HTLC: %v", predErr)
	}

	// Bob's sweeping transaction should now be found in the mempool at
	// this point.
	sweepTx, err := waitForTxInMempool(net.Miner.Node, time.Second*10)
	if err != nil {
		// If Bob's transaction isn't yet in the mempool, then due to
		// internal message passing and the low period between blocks
		// being mined, it may have been detected as a late
		// registration. As a result, we'll mine another block and
		// repeat the check. If it doesn't go through this time, then
		// we'll fail.
		if _, err := net.Miner.Node.Generate(1); err != nil {
			t.Fatalf("unable to generate block: %v", err)
		}
		sweepTx, err = waitForTxInMempool(net.Miner.Node, time.Second*10)
		if err != nil {
			t.Fatalf("unable to find bob's sweeping transaction: "+
				"%v", err)
		}
	}

	// If we mine an additional block, then this should confirm Bob's
	// transaction which sweeps the direct HTLC output.
	block := mineBlocks(t, net, 1)[0]
	assertTxInBlock(t, block, sweepTx)

	// Now that the sweeping transaction has been confirmed, Bob should
	// cancel back that HTLC. As a result, Alice should not know of any
	// active HTLC's.
	nodes = []*lntest.HarnessNode{net.Alice}
	err = lntest.WaitPredicate(func() bool {
		return assertNumActiveHtlcs(nodes, 0)
	}, time.Second*15)
	if err != nil {
		t.Fatalf("alice's channel still has active htlc's")
	}

	// Now we'll check Bob's pending channel report. Since this was Carol's
	// commitment, he doesn't have to wait for any CSV delays. As a result,
	// he should show no additional pending transactions.
	err = lntest.WaitPredicate(func() bool {
		pendingChanResp, err := net.Bob.PendingChannels(
			ctxb, pendingChansRequest,
		)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		if len(pendingChanResp.PendingForceClosingChannels) != 0 {
			predErr = fmt.Errorf("bob still has pending channels "+
				"but shouldn't: %v", spew.Sdump(pendingChanResp))
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf(predErr.Error())
	}

	// We'll close out the test by closing the channel from Alice to Bob,
	// and then shutting down the new node we created as its no longer
	// needed.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, aliceChanPoint, false)
}

// testMultiHopHtlcLocalChainClaim tests that in a multi-hop HTLC scenario, if
// we're forced to go to chain with an incoming HTLC, then when we find out the
// preimage via the witness beacon, we properly settle the HTLC on-chain in
// order to ensure we don't lose any funds.
func testMultiHopHtlcLocalChainClaim(net *lntest.NetworkHarness, t *harnessTest) {
	timeout := time.Duration(time.Second * 15)
	ctxb := context.Background()

	defaultCSV := uint32(4)

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopHodlNetwork(t, net)

	// Clean up carol's node when the test finishes.
	defer shutdownAndAssert(net, t, carol)

	// With the network active, we'll now add a new invoice at Carol's end.
	invoiceReq := &lnrpc.Invoice{
		Value: 100000,
	}
	carolInvoice, err := carol.AddInvoice(ctxb, invoiceReq)
	if err != nil {
		t.Fatalf("unable to generate carol invoice: %v", err)
	}

	// Now that we've created the invoice, we'll send a single payment from
	// Alice to Carol. We won't wait for the response however, as Carol
	// will not immediately settle the payment.
	alicePayStream, err := net.Alice.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}
	err = alicePayStream.Send(&lnrpc.SendRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
	})
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// We'll now wait until all 3 nodes have the HTLC as just sent fully
	// locked in.
	var predErr error
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol}
	err = lntest.WaitPredicate(func() bool {
		predErr = assertActiveHtlcs(nodes, carolInvoice.RHash)
		if predErr != nil {
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", err)
	}

	// At this point, Bob decides that he wants to exit the channel
	// immediately, so he force closes his commitment transaction.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, aliceChanPoint, true)

	// We'll now mine enough blocks so Carol decides that she needs to go
	// on-chain to claim the HTLC as Bob has been inactive.
	numBlocks := uint32(defaultBitcoinTimeLockDelta - (2 * defaultBroadcastDelta))
	if _, err := net.Miner.Node.Generate(numBlocks); err != nil {
		t.Fatalf("unable to generate blocks")
	}

	// Carol's commitment transaction should now be in the mempool.
	txids, err := waitForNTxsInMempool(net.Miner.Node, 1, time.Second*15)
	if err != nil {
		t.Fatalf("transactions not found in mempool: %v", err)
	}
	txidHash, err := getChanPointFundingTxid(bobChanPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	bobFundingTxid, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	carolFundingPoint := wire.OutPoint{
		Hash:  *bobFundingTxid,
		Index: bobChanPoint.OutputIndex,
	}

	// The tx should be spending from the funding transaction,
	commitHash := txids[0]
	tx1, err := net.Miner.Node.GetRawTransaction(commitHash)
	if err != nil {
		t.Fatalf("unable to get txn: %v", err)
	}
	if tx1.MsgTx().TxIn[0].PreviousOutPoint != carolFundingPoint {
		t.Fatalf("commit transaction not spending fundingtx: %v",
			spew.Sdump(tx1))
	}

	// Mine a block that should confirm the commit tx.
	block := mineBlocks(t, net, 1)[0]
	if len(block.Transactions) != 2 {
		t.Fatalf("expected 2 transactions in block, got %v",
			len(block.Transactions))
	}
	assertTxInBlock(t, block, commitHash)

	// After the force close transacion is mined, Carol should broadcast
	// her second level HTLC transacion. Bob will braodcast a sweep tx to
	// sweep his output in the channel with Carol. When Bob notices Carol's
	// second level transaction in the mempool, he will extract the
	// preimage and broadcast a second level tx to claim the HTLC in his
	// (already closed) channel with Alice.
	secondLevelHashes, err := waitForNTxsInMempool(net.Miner.Node, 3,
		time.Second*20)
	if err != nil {
		t.Fatalf("transactions not found in mempool: %v", err)
	}

	// Carol's second level transaction should be spending from
	// the commitment transaction.
	var secondLevelHash *chainhash.Hash
	for _, txid := range secondLevelHashes {
		tx, err := net.Miner.Node.GetRawTransaction(txid)
		if err != nil {
			t.Fatalf("unable to get txn: %v", err)
		}

		if tx.MsgTx().TxIn[0].PreviousOutPoint.Hash == *commitHash {
			secondLevelHash = txid
		}
	}
	if secondLevelHash == nil {
		t.Fatalf("Carol's second level tx not found")
	}

	// At this point, Bob should have broadcast his second layer success
	// transaction, and should have sent it to the nursery for incubation.
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	err = lntest.WaitPredicate(func() bool {
		pendingChanResp, err := net.Bob.PendingChannels(
			ctxb, pendingChansRequest,
		)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}

		if len(pendingChanResp.PendingForceClosingChannels) == 0 {
			predErr = fmt.Errorf("bob should have pending for " +
				"close chan but doesn't")
			return false
		}

		for _, forceCloseChan := range pendingChanResp.PendingForceClosingChannels {
			if forceCloseChan.Channel.LocalBalance != 0 {
				continue
			}

			if len(forceCloseChan.PendingHtlcs) != 1 {
				predErr = fmt.Errorf("bob should have pending htlc " +
					"but doesn't")
				return false
			}
			stage := forceCloseChan.PendingHtlcs[0].Stage
			if stage != 1 {
				predErr = fmt.Errorf("bob's htlc should have "+
					"advanced to the first stage but was "+
					"stage: %v", stage)
				return false
			}
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("bob didn't hand off time-locked HTLC: %v", predErr)
	}

	// We'll now mine a block which should confirm the two second layer
	// transactions and the commit sweep.
	block = mineBlocks(t, net, 1)[0]
	if len(block.Transactions) != 4 {
		t.Fatalf("expected 4 transactions in block, got %v",
			len(block.Transactions))
	}
	assertTxInBlock(t, block, secondLevelHash)

	// If we then mine 4 additional blocks, Bob and Carol should sweep the
	// outputs destined for them.
	if _, err := net.Miner.Node.Generate(defaultCSV); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	sweepTxs, err := waitForNTxsInMempool(net.Miner.Node, 2, time.Second*10)
	if err != nil {
		t.Fatalf("unable to find sweeping transactions: %v", err)
	}

	// At this point, Bob should detect that he has no pending channels
	// anymore, as this just resolved it by the confirmation of the sweep
	// transaction we detected above.
	block = mineBlocks(t, net, 1)[0]
	for _, sweepTx := range sweepTxs {
		assertTxInBlock(t, block, sweepTx)
	}

	err = lntest.WaitPredicate(func() bool {
		pendingChanResp, err := net.Bob.PendingChannels(
			ctxb, pendingChansRequest,
		)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		if len(pendingChanResp.PendingForceClosingChannels) != 0 {
			predErr = fmt.Errorf("bob still has pending channels "+
				"but shouldn't: %v", spew.Sdump(pendingChanResp))
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf(predErr.Error())
	}
}

// testMultiHopHtlcRemoteChainClaim tests that in the multi-hop HTLC scenario,
// if the remote party goes to chain while we have an incoming HTLC, then when
// we found out the preimage via the witness beacon, we properly settle the
// HTLC on-chain in order to ensure that we don't lose any funds.
func testMultiHopHtlcRemoteChainClaim(net *lntest.NetworkHarness, t *harnessTest) {
	timeout := time.Duration(time.Second * 15)
	ctxb := context.Background()

	defaultCSV := uint32(4)

	// First, we'll create a three hop network: Alice -> Bob -> Carol, with
	// Carol refusing to actually settle or directly cancel any HTLC's
	// self.
	aliceChanPoint, bobChanPoint, carol := createThreeHopHodlNetwork(t, net)

	// Clean up carol's node when the test finishes.
	defer shutdownAndAssert(net, t, carol)

	// With the network active, we'll now add a new invoice at Carol's end.
	invoiceReq := &lnrpc.Invoice{
		Value: 100000,
	}
	carolInvoice, err := carol.AddInvoice(ctxb, invoiceReq)
	if err != nil {
		t.Fatalf("unable to generate carol invoice: %v", err)
	}

	// Now that we've created the invoice, we'll send a single payment from
	// Alice to Carol. We won't wait for the response however, as Carol
	// will not immediately settle the payment.
	alicePayStream, err := net.Alice.SendPayment(ctxb)
	if err != nil {
		t.Fatalf("unable to create payment stream for alice: %v", err)
	}
	err = alicePayStream.Send(&lnrpc.SendRequest{
		PaymentRequest: carolInvoice.PaymentRequest,
	})
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// We'll now wait until all 3 nodes have the HTLC as just sent fully
	// locked in.
	var predErr error
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol}
	err = lntest.WaitPredicate(func() bool {
		predErr = assertActiveHtlcs(nodes, carolInvoice.RHash)
		if predErr != nil {
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", err)
	}

	// Next, Alice decides that she wants to exit the channel, so she'll
	// immediately force close the channel by broadcast her commitment
	// transaction.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, aliceChanPoint, true)

	// We'll now mine enough blocks so Carol decides that she needs to go
	// on-chain to claim the HTLC as Bob has been inactive.
	claimDelta := uint32(2 * defaultBroadcastDelta)
	numBlocks := uint32(defaultBitcoinTimeLockDelta - claimDelta)
	if _, err := net.Miner.Node.Generate(numBlocks); err != nil {
		t.Fatalf("unable to generate blocks")
	}

	// Carol's commitment transaction should now be in the mempool.
	txids, err := waitForNTxsInMempool(net.Miner.Node, 1, time.Second*15)
	if err != nil {
		t.Fatalf("transactions not found in mempool: %v", err)
	}
	txidHash, err := getChanPointFundingTxid(bobChanPoint)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	bobFundingTxid, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	carolFundingPoint := wire.OutPoint{
		Hash:  *bobFundingTxid,
		Index: bobChanPoint.OutputIndex,
	}

	// The transaction should be spending from the funding transaction
	commitHash := txids[0]
	tx1, err := net.Miner.Node.GetRawTransaction(commitHash)
	if err != nil {
		t.Fatalf("unable to get txn: %v", err)
	}
	if tx1.MsgTx().TxIn[0].PreviousOutPoint != carolFundingPoint {
		t.Fatalf("commit transaction not spending fundingtx: %v",
			spew.Sdump(tx1))
	}

	// Mine a block, which should contain the commitment.
	block := mineBlocks(t, net, 1)[0]
	if len(block.Transactions) != 2 {
		t.Fatalf("expected 2 transactions in block, got %v",
			len(block.Transactions))
	}
	assertTxInBlock(t, block, commitHash)

	// After the force close transacion is mined, Carol should broadcast
	// her second level HTLC transacion. Bob will braodcast a sweep tx to
	// sweep his output in the channel with Carol. When Bob notices Carol's
	// second level transaction in the mempool, he will extract the
	// preimage and broadcast a second level tx to claim the HTLC in his
	// (already closed) channel with Alice.
	secondLevelHashes, err := waitForNTxsInMempool(net.Miner.Node, 3,
		time.Second*20)
	if err != nil {
		t.Fatalf("transactions not found in mempool: %v", err)
	}

	// Carol's second level transaction should be spending from
	// the commitment transaction.
	var secondLevelHash *chainhash.Hash
	for _, txid := range secondLevelHashes {
		tx, err := net.Miner.Node.GetRawTransaction(txid)
		if err != nil {
			t.Fatalf("unable to get txn: %v", err)
		}

		if tx.MsgTx().TxIn[0].PreviousOutPoint.Hash == *commitHash {
			secondLevelHash = txid
		}
	}
	if secondLevelHash == nil {
		t.Fatalf("Carol's second level tx not found")
	}

	// We'll now mine a block which should confirm the two second layer
	// transactions and the commit sweep.
	block = mineBlocks(t, net, 1)[0]
	if len(block.Transactions) != 4 {
		t.Fatalf("expected 4 transactions in block, got %v",
			len(block.Transactions))
	}
	assertTxInBlock(t, block, secondLevelHash)

	// If we then mine 4 additional blocks, Bob should pull the output
	// destined for him.
	if _, err := net.Miner.Node.Generate(defaultCSV); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	_, err = waitForNTxsInMempool(net.Miner.Node, 1, time.Second*15)
	if err != nil {
		t.Fatalf("unable to find bob's sweeping transaction: %v", err)
	}

	// We'll now mine another block, this should confirm the sweep
	// transaction that Bob broadcast in the prior stage.
	if _, err := net.Miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// Now that the sweeping transaction has been confirmed, Bob should now
	// recognize that all contracts have been fully resolved, and show no
	// pending close channels.
	pendingChansRequest := &lnrpc.PendingChannelsRequest{}
	err = lntest.WaitPredicate(func() bool {
		pendingChanResp, err := net.Bob.PendingChannels(
			ctxb, pendingChansRequest,
		)
		if err != nil {
			predErr = fmt.Errorf("unable to query for pending "+
				"channels: %v", err)
			return false
		}
		if len(pendingChanResp.PendingForceClosingChannels) != 0 {
			predErr = fmt.Errorf("bob still has pending channels "+
				"but shouldn't: %v", spew.Sdump(pendingChanResp))
			return false
		}

		return true
	}, time.Second*15)
	if err != nil {
		t.Fatalf(predErr.Error())
	}
}

// testSwitchCircuitPersistence creates a multihop network to ensure the sender
// and intermediaries are persisting their open payment circuits. After
// forwarding a packet via an outgoing link, all are restarted, and expected to
// forward a response back from the receiver once back online.
//
// The general flow of this test:
//   1. Carol --> Dave --> Alice --> Bob  forward payment
//   2.        X        X         X  Bob  restart sender and intermediaries
//   3. Carol <-- Dave <-- Alice <-- Bob  expect settle to propagate
func testSwitchCircuitPersistence(net *lntest.NetworkHarness, t *harnessTest) {
	const chanAmt = btcutil.Amount(1000000)
	const pushAmt = btcutil.Amount(900000)

	ctxb := context.Background()
	timeout := time.Duration(time.Second * 15)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, chanAmt, pushAmt, false,
	)
	networkChans = append(networkChans, chanPointAlice)

	txidHash, err := getChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// As preliminary setup, we'll create two new nodes: Carol and Dave,
	// such that we now have a 4 ndoe, 3 channel topology. Dave will make
	// a channel with Alice, and Carol with Dave. After this setup, the
	// network topology should now look like:
	//     Carol -> Dave -> Alice -> Bob
	//
	// First, we'll create Dave and establish a channel to Alice.
	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	if err := net.ConnectNodes(ctxb, dave, net.Alice); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointDave := openChannelAndAssert(
		ctxt, t, net, dave, net.Alice, chanAmt, pushAmt, false,
	)
	networkChans = append(networkChans, chanPointDave)
	txidHash, err = getChanPointFundingTxid(chanPointDave)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	daveChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	daveFundPoint := wire.OutPoint{
		Hash:  *daveChanTXID,
		Index: chanPointDave.OutputIndex,
	}

	// Next, we'll create Carol and establish a channel to from her to
	// Dave. Carol is started in htlchodl mode so that we can disconnect the
	// intermediary hops before starting the settle.
	carol, err := net.NewNode("Carol", []string{"--debughtlc", "--hodl.exit-settle"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	if err := net.ConnectNodes(ctxb, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, carol, dave, chanAmt, pushAmt, false,
	)
	networkChans = append(networkChans, chanPointCarol)

	txidHash, err = getChanPointFundingTxid(chanPointCarol)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
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
			txidHash, err := getChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			txid, e := chainhash.NewHash(txidHash)
			if e != nil {
				t.Fatalf("unable to create sha hash: %v", e)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ = context.WithTimeout(ctxb, timeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, point, err)
			}
		}
	}

	// Create 5 invoices for Carol, which expect a payment from Bob for 1k
	// satoshis with a different preimage each time.
	const numPayments = 5
	const paymentAmt = 1000
	payReqs := make([]string, numPayments)
	for i := 0; i < numPayments; i++ {
		invoice := &lnrpc.Invoice{
			Memo:  "testing",
			Value: paymentAmt,
		}
		resp, err := carol.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		payReqs[i] = resp.PaymentRequest
	}

	// We'll wait for all parties to recognize the new channels within the
	// network.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPointDave)
	if err != nil {
		t.Fatalf("dave didn't advertise his channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointCarol)
	if err != nil {
		t.Fatalf("carol didn't advertise her channel in time: %v",
			err)
	}

	time.Sleep(time.Millisecond * 50)

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(ctxt, net.Bob, payReqs, false)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Wait until all nodes in the network have 5 outstanding htlcs.
	err = lntest.WaitPredicate(func() bool {
		return assertNumActiveHtlcs(nodes, numPayments)
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", err)
	}

	// Restart the intermediaries and the sender.
	if err := net.RestartNode(dave, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	if err := net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	if err := net.RestartNode(net.Bob, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Ensure all of the intermediate links are reconnected.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.EnsureConnected(ctxt, net.Alice, dave)
	if err != nil {
		t.Fatalf("unable to reconnect alice and dave: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.EnsureConnected(ctxt, net.Bob, net.Alice)
	if err != nil {
		t.Fatalf("unable to reconnect bob and alice: %v", err)
	}

	// Ensure all nodes in the network still have 5 outstanding htlcs.
	err = lntest.WaitPredicate(func() bool {
		return assertNumActiveHtlcs(nodes, numPayments)
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", err)
	}

	// Now restart carol without hodl mode, to settle back the outstanding
	// payments.
	carol.SetExtraArgs(nil)
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.EnsureConnected(ctxt, dave, carol)
	if err != nil {
		t.Fatalf("unable to reconnect dave and carol: %v", err)
	}

	// After the payments settle, there should be no active htlcs on any of
	// the nodes in the network.
	err = lntest.WaitPredicate(func() bool {
		return assertNumActiveHtlcs(nodes, 0)
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", err)
	}

	// When asserting the amount of satoshis moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.
	const baseFee = 1

	// At this point all the channels within our proto network should be
	// shifted by 5k satoshis in the direction of Carol, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Bob->Alice->David->Carol, order is Carol,
	// David, Alice, Bob.
	var amountPaid = int64(5000)
	assertAmountPaid(t, ctxb, "Dave(local) => Carol(remote)", carol,
		carolFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, ctxb, "Dave(local) => Carol(remote)", dave,
		carolFundPoint, amountPaid, int64(0))
	assertAmountPaid(t, ctxb, "Alice(local) => Dave(remote)", dave,
		daveFundPoint, int64(0), amountPaid+(baseFee*numPayments))
	assertAmountPaid(t, ctxb, "Alice(local) => Dave(remote)", net.Alice,
		daveFundPoint, amountPaid+(baseFee*numPayments), int64(0))
	assertAmountPaid(t, ctxb, "Bob(local) => Alice(remote)", net.Alice,
		aliceFundPoint, int64(0), amountPaid+((baseFee*numPayments)*2))
	assertAmountPaid(t, ctxb, "Bob(local) => Alice(remote)", net.Bob,
		aliceFundPoint, amountPaid+(baseFee*numPayments)*2, int64(0))

	// Lastly, we will send one more payment to ensure all channels are
	// still functioning properly.
	finalInvoice := &lnrpc.Invoice{
		Memo:  "testing",
		Value: paymentAmt,
	}
	resp, err := carol.AddInvoice(ctxb, finalInvoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	payReqs = []string{resp.PaymentRequest}

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(ctxt, net.Bob, payReqs, true)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	amountPaid = int64(6000)
	assertAmountPaid(t, ctxb, "Dave(local) => Carol(remote)", carol,
		carolFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, ctxb, "Dave(local) => Carol(remote)", dave,
		carolFundPoint, amountPaid, int64(0))
	assertAmountPaid(t, ctxb, "Alice(local) => Dave(remote)", dave,
		daveFundPoint, int64(0), amountPaid+(baseFee*(numPayments+1)))
	assertAmountPaid(t, ctxb, "Alice(local) => Dave(remote)", net.Alice,
		daveFundPoint, amountPaid+(baseFee*(numPayments+1)), int64(0))
	assertAmountPaid(t, ctxb, "Bob(local) => Alice(remote)", net.Alice,
		aliceFundPoint, int64(0), amountPaid+((baseFee*(numPayments+1))*2))
	assertAmountPaid(t, ctxb, "Bob(local) => Alice(remote)", net.Bob,
		aliceFundPoint, amountPaid+(baseFee*(numPayments+1))*2, int64(0))

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, dave, chanPointDave, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)
}

// testSwitchOfflineDelivery constructs a set of multihop payments, and tests
// that the returning payments are not lost if a peer on the backwards path is
// offline when the settle/fails are received. We expect the payments to be
// buffered in memory, and transmitted as soon as the disconnect link comes back
// online.
//
// The general flow of this test:
//   1. Carol --> Dave --> Alice --> Bob  forward payment
//   2. Carol --- Dave  X  Alice --- Bob  disconnect intermediaries
//   3. Carol --- Dave  X  Alice <-- Bob  settle last hop
//   4. Carol <-- Dave <-- Alice --- Bob  reconnect, expect settle to propagate
func testSwitchOfflineDelivery(net *lntest.NetworkHarness, t *harnessTest) {
	const chanAmt = btcutil.Amount(1000000)
	const pushAmt = btcutil.Amount(900000)

	ctxb := context.Background()
	timeout := time.Duration(time.Second * 15)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, chanAmt, pushAmt, false,
	)
	networkChans = append(networkChans, chanPointAlice)

	txidHash, err := getChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// As preliminary setup, we'll create two new nodes: Carol and Dave,
	// such that we now have a 4 ndoe, 3 channel topology. Dave will make
	// a channel with Alice, and Carol with Dave. After this setup, the
	// network topology should now look like:
	//     Carol -> Dave -> Alice -> Bob
	//
	// First, we'll create Dave and establish a channel to Alice.
	dave, err := net.NewNode("Dave", []string{"--unsafe-disconnect"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	if err := net.ConnectNodes(ctxb, dave, net.Alice); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointDave := openChannelAndAssert(
		ctxt, t, net, dave, net.Alice, chanAmt, pushAmt, false,
	)
	networkChans = append(networkChans, chanPointDave)
	txidHash, err = getChanPointFundingTxid(chanPointDave)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	daveChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	daveFundPoint := wire.OutPoint{
		Hash:  *daveChanTXID,
		Index: chanPointDave.OutputIndex,
	}

	// Next, we'll create Carol and establish a channel to from her to
	// Dave. Carol is started in htlchodl mode so that we can disconnect the
	// intermediary hops before starting the settle.
	carol, err := net.NewNode("Carol", []string{"--debughtlc", "--hodl.exit-settle"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	if err := net.ConnectNodes(ctxb, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, carol, dave, chanAmt, pushAmt, false,
	)
	networkChans = append(networkChans, chanPointCarol)

	txidHash, err = getChanPointFundingTxid(chanPointCarol)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
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
			txidHash, err := getChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			txid, e := chainhash.NewHash(txidHash)
			if e != nil {
				t.Fatalf("unable to create sha hash: %v", e)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ = context.WithTimeout(ctxb, timeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, point, err)
			}
		}
	}

	// Create 5 invoices for Carol, which expect a payment from Bob for 1k
	// satoshis with a different preimage each time.
	const numPayments = 5
	const paymentAmt = 1000
	payReqs := make([]string, numPayments)
	for i := 0; i < numPayments; i++ {
		invoice := &lnrpc.Invoice{
			Memo:  "testing",
			Value: paymentAmt,
		}
		resp, err := carol.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		payReqs[i] = resp.PaymentRequest
	}

	// We'll wait for all parties to recognize the new channels within the
	// network.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPointDave)
	if err != nil {
		t.Fatalf("dave didn't advertise his channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointCarol)
	if err != nil {
		t.Fatalf("carol didn't advertise her channel in time: %v",
			err)
	}

	time.Sleep(time.Millisecond * 50)

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(ctxt, net.Bob, payReqs, false)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Wait for all of the payments to reach Carol.
	err = lntest.WaitPredicate(func() bool {
		return assertNumActiveHtlcs(nodes, numPayments)
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", err)
	}

	// First, disconnect Dave and Alice so that their link is broken.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.DisconnectNodes(ctxt, dave, net.Alice); err != nil {
		t.Fatalf("unable to disconnect alice from dave: %v", err)
	}

	// Then, reconnect them to ensure Dave doesn't just fail back the htlc.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.ConnectNodes(ctxt, dave, net.Alice); err != nil {
		t.Fatalf("unable to reconnect alice to dave: %v", err)
	}

	// Wait to ensure that the payment remain are not failed back after
	// reconnecting. All node should report the number payments initiated
	// for the duration of the interval.
	err = lntest.WaitInvariant(func() bool {
		return assertNumActiveHtlcs(nodes, numPayments)
	}, time.Second*2)
	if err != nil {
		t.Fatalf("htlc change: %v", err)
	}

	// Now, disconnect Dave from Alice again before settling back the
	// payment.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.DisconnectNodes(ctxt, dave, net.Alice); err != nil {
		t.Fatalf("unable to disconnect alice from dave: %v", err)
	}

	// Now restart carol without hodl mode, to settle back the outstanding
	// payments.
	carol.SetExtraArgs(nil)
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Wait for Carol to report no outstanding htlcs.
	carolNode := []*lntest.HarnessNode{carol}
	err = lntest.WaitPredicate(func() bool {
		return assertNumActiveHtlcs(carolNode, 0)
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", err)
	}

	// Now that the settles have reached Dave, reconnect him with Alice,
	// allowing the settles to return to the sender.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.EnsureConnected(ctxt, dave, net.Alice); err != nil {
		t.Fatalf("unable to reconnect alice to dave: %v", err)
	}

	// Wait until all outstanding htlcs in the network have been settled.
	err = lntest.WaitPredicate(func() bool {
		return assertNumActiveHtlcs(nodes, 0)
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", err)
	}

	// When asserting the amount of satoshis moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.
	const baseFee = 1

	// At this point all the channels within our proto network should be
	// shifted by 5k satoshis in the direction of Carol, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Bob->Alice->David->Carol, order is Carol,
	// David, Alice, Bob.
	var amountPaid = int64(5000)
	assertAmountPaid(t, ctxb, "Dave(local) => Carol(remote)", carol,
		carolFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, ctxb, "Dave(local) => Carol(remote)", dave,
		carolFundPoint, amountPaid, int64(0))
	assertAmountPaid(t, ctxb, "Alice(local) => Dave(remote)", dave,
		daveFundPoint, int64(0), amountPaid+(baseFee*numPayments))
	assertAmountPaid(t, ctxb, "Alice(local) => Dave(remote)", net.Alice,
		daveFundPoint, amountPaid+(baseFee*numPayments), int64(0))
	assertAmountPaid(t, ctxb, "Bob(local) => Alice(remote)", net.Alice,
		aliceFundPoint, int64(0), amountPaid+((baseFee*numPayments)*2))
	assertAmountPaid(t, ctxb, "Bob(local) => Alice(remote)", net.Bob,
		aliceFundPoint, amountPaid+(baseFee*numPayments)*2, int64(0))

	// Lastly, we will send one more payment to ensure all channels are
	// still functioning properly.
	finalInvoice := &lnrpc.Invoice{
		Memo:  "testing",
		Value: paymentAmt,
	}
	resp, err := carol.AddInvoice(ctxb, finalInvoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	payReqs = []string{resp.PaymentRequest}

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(ctxt, net.Bob, payReqs, true)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	amountPaid = int64(6000)
	assertAmountPaid(t, ctxb, "Dave(local) => Carol(remote)", carol,
		carolFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, ctxb, "Dave(local) => Carol(remote)", dave,
		carolFundPoint, amountPaid, int64(0))
	assertAmountPaid(t, ctxb, "Alice(local) => Dave(remote)", dave,
		daveFundPoint, int64(0), amountPaid+(baseFee*(numPayments+1)))
	assertAmountPaid(t, ctxb, "Alice(local) => Dave(remote)", net.Alice,
		daveFundPoint, amountPaid+(baseFee*(numPayments+1)), int64(0))
	assertAmountPaid(t, ctxb, "Bob(local) => Alice(remote)", net.Alice,
		aliceFundPoint, int64(0), amountPaid+((baseFee*(numPayments+1))*2))
	assertAmountPaid(t, ctxb, "Bob(local) => Alice(remote)", net.Bob,
		aliceFundPoint, amountPaid+(baseFee*(numPayments+1))*2, int64(0))

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, dave, chanPointDave, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)
}

// testSwitchOfflineDeliveryPersistence constructs a set of multihop payments,
// and tests that the returning payments are not lost if a peer on the backwards
// path is offline when the settle/fails are received AND the peer buffering the
// responses is completely restarts. We expect the payments to be reloaded from
// disk, and transmitted as soon as the intermediaries are reconnected.
//
// The general flow of this test:
//   1. Carol --> Dave --> Alice --> Bob  forward payment
//   2. Carol --- Dave  X  Alice --- Bob  disconnect intermediaries
//   3. Carol --- Dave  X  Alice <-- Bob  settle last hop
//   4. Carol --- Dave  X         X  Bob  restart Alice
//   5. Carol <-- Dave <-- Alice --- Bob  expect settle to propagate
func testSwitchOfflineDeliveryPersistence(net *lntest.NetworkHarness, t *harnessTest) {
	const chanAmt = btcutil.Amount(1000000)
	const pushAmt = btcutil.Amount(900000)

	ctxb := context.Background()
	timeout := time.Duration(time.Second * 15)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, chanAmt, pushAmt, false,
	)
	networkChans = append(networkChans, chanPointAlice)

	txidHash, err := getChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// As preliminary setup, we'll create two new nodes: Carol and Dave,
	// such that we now have a 4 ndoe, 3 channel topology. Dave will make
	// a channel with Alice, and Carol with Dave. After this setup, the
	// network topology should now look like:
	//     Carol -> Dave -> Alice -> Bob
	//
	// First, we'll create Dave and establish a channel to Alice.
	dave, err := net.NewNode("Dave", []string{"--unsafe-disconnect"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	if err := net.ConnectNodes(ctxb, dave, net.Alice); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointDave := openChannelAndAssert(
		ctxt, t, net, dave, net.Alice, chanAmt, pushAmt, false,
	)
	networkChans = append(networkChans, chanPointDave)
	txidHash, err = getChanPointFundingTxid(chanPointDave)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	daveChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	daveFundPoint := wire.OutPoint{
		Hash:  *daveChanTXID,
		Index: chanPointDave.OutputIndex,
	}

	// Next, we'll create Carol and establish a channel to from her to
	// Dave. Carol is started in htlchodl mode so that we can disconnect the
	// intermediary hops before starting the settle.
	carol, err := net.NewNode("Carol", []string{"--debughtlc", "--hodl.exit-settle"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	if err := net.ConnectNodes(ctxb, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, carol, dave, chanAmt, pushAmt, false,
	)
	networkChans = append(networkChans, chanPointCarol)

	txidHash, err = getChanPointFundingTxid(chanPointCarol)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
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
			txidHash, err := getChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			txid, e := chainhash.NewHash(txidHash)
			if e != nil {
				t.Fatalf("unable to create sha hash: %v", e)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ = context.WithTimeout(ctxb, timeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, point, err)
			}
		}
	}

	// Create 5 invoices for Carol, which expect a payment from Bob for 1k
	// satoshis with a different preimage each time.
	const numPayments = 5
	const paymentAmt = 1000
	payReqs := make([]string, numPayments)
	for i := 0; i < numPayments; i++ {
		invoice := &lnrpc.Invoice{
			Memo:  "testing",
			Value: paymentAmt,
		}
		resp, err := carol.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		payReqs[i] = resp.PaymentRequest
	}

	// We'll wait for all parties to recognize the new channels within the
	// network.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPointDave)
	if err != nil {
		t.Fatalf("dave didn't advertise his channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointCarol)
	if err != nil {
		t.Fatalf("carol didn't advertise her channel in time: %v",
			err)
	}

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(ctxt, net.Bob, payReqs, false)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	err = lntest.WaitPredicate(func() bool {
		return assertNumActiveHtlcs(nodes, numPayments)
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", err)
	}

	// Disconnect the two intermediaries, Alice and Dave, by shutting down
	// Alice.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.StopNode(net.Alice); err != nil {
		t.Fatalf("unable to shutdown alice: %v", err)
	}

	// Now restart carol without hodl mode, to settle back the outstanding
	// payments.
	carol.SetExtraArgs(nil)
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Make Carol and Dave are reconnected before waiting for the htlcs to
	// clear.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.EnsureConnected(ctxt, dave, carol)
	if err != nil {
		t.Fatalf("unable to reconnect dave and carol: %v", err)
	}

	// Wait for Carol to report no outstanding htlcs, and also for Dav to
	// receive all the settles from Carol.
	carolNode := []*lntest.HarnessNode{carol}
	err = lntest.WaitPredicate(func() bool {
		return assertNumActiveHtlcs(carolNode, 0) &&
			assertNumActiveHtlcsChanPoint(dave, carolFundPoint, 0)
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", err)
	}

	// Finally, restart dave who received the settles, but was unable to
	// deliver them to Alice since they were disconnected.
	if err := net.RestartNode(dave, nil); err != nil {
		t.Fatalf("unable to restart dave: %v", err)
	}
	if err = net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("unable to restart alice: %v", err)
	}

	// Force Dave and Alice to reconnect before waiting for the htlcs to
	// clear.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.EnsureConnected(ctxt, dave, net.Alice)
	if err != nil {
		t.Fatalf("unable to reconnect dave and carol: %v", err)
	}

	// After reconnection succeeds, the settles should be propagated all
	// the way back to the sender. All nodes should report no active htlcs.
	err = lntest.WaitPredicate(func() bool {
		return assertNumActiveHtlcs(nodes, 0)
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", err)
	}

	// When asserting the amount of satoshis moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.
	const baseFee = 1

	// At this point all the channels within our proto network should be
	// shifted by 5k satoshis in the direction of Carol, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Bob->Alice->David->Carol, order is Carol,
	// David, Alice, Bob.
	var amountPaid = int64(5000)
	assertAmountPaid(t, ctxb, "Dave(local) => Carol(remote)", carol,
		carolFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, ctxb, "Dave(local) => Carol(remote)", dave,
		carolFundPoint, amountPaid, int64(0))
	assertAmountPaid(t, ctxb, "Alice(local) => Dave(remote)", dave,
		daveFundPoint, int64(0), amountPaid+(baseFee*numPayments))
	assertAmountPaid(t, ctxb, "Alice(local) => Dave(remote)", net.Alice,
		daveFundPoint, amountPaid+(baseFee*numPayments), int64(0))
	assertAmountPaid(t, ctxb, "Bob(local) => Alice(remote)", net.Alice,
		aliceFundPoint, int64(0), amountPaid+((baseFee*numPayments)*2))
	assertAmountPaid(t, ctxb, "Bob(local) => Alice(remote)", net.Bob,
		aliceFundPoint, amountPaid+(baseFee*numPayments)*2, int64(0))

	// Lastly, we will send one more payment to ensure all channels are
	// still functioning properly.
	finalInvoice := &lnrpc.Invoice{
		Memo:  "testing",
		Value: paymentAmt,
	}
	resp, err := carol.AddInvoice(ctxb, finalInvoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	payReqs = []string{resp.PaymentRequest}

	// Before completing the final payment request, ensure that the
	// connection between Dave and Carol has been healed.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.EnsureConnected(ctxt, dave, carol)
	if err != nil {
		t.Fatalf("unable to reconnect dave and carol: %v", err)
	}

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(ctxt, net.Bob, payReqs, true)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	amountPaid = int64(6000)
	assertAmountPaid(t, ctxb, "Dave(local) => Carol(remote)", carol,
		carolFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, ctxb, "Dave(local) => Carol(remote)", dave,
		carolFundPoint, amountPaid, int64(0))
	assertAmountPaid(t, ctxb, "Alice(local) => Dave(remote)", dave,
		daveFundPoint, int64(0), amountPaid+(baseFee*(numPayments+1)))
	assertAmountPaid(t, ctxb, "Alice(local) => Dave(remote)", net.Alice,
		daveFundPoint, amountPaid+(baseFee*(numPayments+1)), int64(0))
	assertAmountPaid(t, ctxb, "Bob(local) => Alice(remote)", net.Alice,
		aliceFundPoint, int64(0), amountPaid+((baseFee*(numPayments+1))*2))
	assertAmountPaid(t, ctxb, "Bob(local) => Alice(remote)", net.Bob,
		aliceFundPoint, amountPaid+(baseFee*(numPayments+1))*2, int64(0))

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, dave, chanPointDave, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)
}

// testSwitchOfflineDeliveryOutgoingOffline constructs a set of multihop payments,
// and tests that the returning payments are not lost if a peer on the backwards
// path is offline when the settle/fails are received AND the peer buffering the
// responses is completely restarts. We expect the payments to be reloaded from
// disk, and transmitted as soon as the intermediaries are reconnected.
//
// The general flow of this test:
//   1. Carol --> Dave --> Alice --> Bob  forward payment
//   2. Carol --- Dave  X  Alice --- Bob  disconnect intermediaries
//   3. Carol --- Dave  X  Alice <-- Bob  settle last hop
//   4. Carol --- Dave  X         X       shutdown Bob, restart Alice
//   5. Carol <-- Dave <-- Alice  X       expect settle to propagate
func testSwitchOfflineDeliveryOutgoingOffline(
	net *lntest.NetworkHarness, t *harnessTest) {

	const chanAmt = btcutil.Amount(1000000)
	const pushAmt = btcutil.Amount(900000)

	ctxb := context.Background()
	timeout := time.Duration(time.Second * 15)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, chanAmt, pushAmt, false,
	)
	networkChans = append(networkChans, chanPointAlice)

	txidHash, err := getChanPointFundingTxid(chanPointAlice)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	aliceChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	aliceFundPoint := wire.OutPoint{
		Hash:  *aliceChanTXID,
		Index: chanPointAlice.OutputIndex,
	}

	// As preliminary setup, we'll create two new nodes: Carol and Dave,
	// such that we now have a 4 ndoe, 3 channel topology. Dave will make
	// a channel with Alice, and Carol with Dave. After this setup, the
	// network topology should now look like:
	//     Carol -> Dave -> Alice -> Bob
	//
	// First, we'll create Dave and establish a channel to Alice.
	dave, err := net.NewNode("Dave", []string{"--unsafe-disconnect"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	if err := net.ConnectNodes(ctxb, dave, net.Alice); err != nil {
		t.Fatalf("unable to connect dave to alice: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, dave)
	if err != nil {
		t.Fatalf("unable to send coins to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointDave := openChannelAndAssert(
		ctxt, t, net, dave, net.Alice, chanAmt, pushAmt, false,
	)
	networkChans = append(networkChans, chanPointDave)
	txidHash, err = getChanPointFundingTxid(chanPointDave)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	daveChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	daveFundPoint := wire.OutPoint{
		Hash:  *daveChanTXID,
		Index: chanPointDave.OutputIndex,
	}

	// Next, we'll create Carol and establish a channel to from her to
	// Dave. Carol is started in htlchodl mode so that we can disconnect the
	// intermediary hops before starting the settle.
	carol, err := net.NewNode("Carol", []string{"--debughtlc", "--hodl.exit-settle"})
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	if err := net.ConnectNodes(ctxb, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, carol, dave, chanAmt, pushAmt, false,
	)
	networkChans = append(networkChans, chanPointCarol)

	txidHash, err = getChanPointFundingTxid(chanPointCarol)
	if err != nil {
		t.Fatalf("unable to get txid: %v", err)
	}
	carolChanTXID, err := chainhash.NewHash(txidHash)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
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
			txidHash, err := getChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			txid, e := chainhash.NewHash(txidHash)
			if e != nil {
				t.Fatalf("unable to create sha hash: %v", e)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ = context.WithTimeout(ctxb, timeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, point, err)
			}
		}
	}

	// Create 5 invoices for Carol, which expect a payment from Bob for 1k
	// satoshis with a different preimage each time.
	const numPayments = 5
	const paymentAmt = 1000
	payReqs := make([]string, numPayments)
	for i := 0; i < numPayments; i++ {
		invoice := &lnrpc.Invoice{
			Memo:  "testing",
			Value: paymentAmt,
		}
		resp, err := carol.AddInvoice(ctxb, invoice)
		if err != nil {
			t.Fatalf("unable to add invoice: %v", err)
		}

		payReqs[i] = resp.PaymentRequest
	}

	// We'll wait for all parties to recognize the new channels within the
	// network.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = dave.WaitForNetworkChannelOpen(ctxt, chanPointDave)
	if err != nil {
		t.Fatalf("dave didn't advertise his channel: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = carol.WaitForNetworkChannelOpen(ctxt, chanPointCarol)
	if err != nil {
		t.Fatalf("carol didn't advertise her channel in time: %v",
			err)
	}

	// Using Carol as the source, pay to the 5 invoices from Bob created
	// above.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = completePaymentRequests(ctxt, net.Bob, payReqs, false)
	if err != nil {
		t.Fatalf("unable to send payments: %v", err)
	}

	// Wait for all payments to reach Carol.
	err = lntest.WaitPredicate(func() bool {
		return assertNumActiveHtlcs(nodes, numPayments)
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", err)
	}

	// Disconnect the two intermediaries, Alice and Dave, so that when carol
	// restarts, the response will be held by Dave.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.StopNode(net.Alice); err != nil {
		t.Fatalf("unable to shutdown alice: %v", err)
	}

	// Now restart carol without hodl mode, to settle back the outstanding
	// payments.
	carol.SetExtraArgs(nil)
	if err := net.RestartNode(carol, nil); err != nil {
		t.Fatalf("Node restart failed: %v", err)
	}

	// Wait for Carol to report no outstanding htlcs.
	carolNode := []*lntest.HarnessNode{carol}
	err = lntest.WaitPredicate(func() bool {
		return assertNumActiveHtlcs(carolNode, 0) &&
			assertNumActiveHtlcsChanPoint(dave, carolFundPoint, 0)
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", err)
	}

	// Now check that the total amount was transferred from Dave to Carol.
	// The amount transferred should be exactly equal to the invoice total
	// payment amount, 5k satsohis.
	const amountPaid = int64(5000)
	assertAmountPaid(t, ctxb, "Dave(local) => Carol(remote)", carol,
		carolFundPoint, int64(0), amountPaid)
	assertAmountPaid(t, ctxb, "Dave(local) => Carol(remote)", dave,
		carolFundPoint, amountPaid, int64(0))

	// Shutdown carol and leave her offline for the rest of the test. This
	// is critical, as we wish to see if Dave can propragate settles even if
	// the outgoing link is never revived.
	shutdownAndAssert(net, t, carol)

	// Now restart Dave, ensuring he is both persisting the settles, and is
	// able to reforward them to Alice after recovering from a restart.
	if err := net.RestartNode(dave, nil); err != nil {
		t.Fatalf("unable to restart dave: %v", err)
	}
	if err = net.RestartNode(net.Alice, nil); err != nil {
		t.Fatalf("unable to restart alice: %v", err)
	}

	// Ensure that Dave is reconnected to Alice before waiting for the htlcs
	// to clear.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.EnsureConnected(ctxt, dave, net.Alice)
	if err != nil {
		t.Fatalf("unable to reconnect alice and dave: %v", err)
	}

	// Since Carol has been shutdown permanently, we will wait until all
	// other nodes in the network report no active htlcs.
	nodesMinusCarol := []*lntest.HarnessNode{net.Bob, net.Alice, dave}
	err = lntest.WaitPredicate(func() bool {
		return assertNumActiveHtlcs(nodesMinusCarol, 0)
	}, time.Second*15)
	if err != nil {
		t.Fatalf("htlc mismatch: %v", err)
	}

	// When asserting the amount of satoshis moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.
	const baseFee = 1

	// At this point, all channels (minus Carol, who is shutdown) should
	// show a shift of 5k satoshis towards Carol.  The order of asserts
	// corresponds to increasing of time is needed to embed the HTLC in
	// commitment transaction, in channel Bob->Alice->David, order is David,
	// Alice, Bob.
	assertAmountPaid(t, ctxb, "Alice(local) => Dave(remote)", dave,
		daveFundPoint, int64(0), amountPaid+(baseFee*numPayments))
	assertAmountPaid(t, ctxb, "Alice(local) => Dave(remote)", net.Alice,
		daveFundPoint, amountPaid+(baseFee*numPayments), int64(0))
	assertAmountPaid(t, ctxb, "Bob(local) => Alice(remote)", net.Alice,
		aliceFundPoint, int64(0), amountPaid+((baseFee*numPayments)*2))
	assertAmountPaid(t, ctxb, "Bob(local) => Alice(remote)", net.Bob,
		aliceFundPoint, amountPaid+(baseFee*numPayments)*2, int64(0))

	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, dave, chanPointDave, false)
}

// computeFee calculates the payment fee as specified in BOLT07
func computeFee(baseFee, feeRate, amt lnwire.MilliSatoshi) lnwire.MilliSatoshi {
	return baseFee + amt*feeRate/1000000
}

// testQueryRoutes checks the response of queryroutes.
// We'll create the following network topology:
//      Alice --> Bob --> Carol --> Dave
// and query the daemon for routes from Alice to Dave.
func testQueryRoutes(net *lntest.NetworkHarness, t *harnessTest) {
	const chanAmt = btcutil.Amount(100000)
	ctxb := context.Background()
	timeout := time.Duration(time.Second * 5)
	var networkChans []*lnrpc.ChannelPoint

	// Open a channel between Alice and Bob.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointAlice := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, chanAmt, 0, false,
	)
	networkChans = append(networkChans, chanPointAlice)

	// Create Carol and establish a channel from Bob.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, carol)

	if err := net.ConnectNodes(ctxb, carol, net.Bob); err != nil {
		t.Fatalf("unable to connect carol to bob: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, net.Bob)
	if err != nil {
		t.Fatalf("unable to send coins to bob: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointBob := openChannelAndAssert(
		ctxt, t, net, net.Bob, carol, chanAmt, 0, false,
	)
	networkChans = append(networkChans, chanPointBob)

	// Create Dave and establish a channel from Carol.
	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create new nodes: %v", err)
	}
	defer shutdownAndAssert(net, t, dave)

	if err := net.ConnectNodes(ctxb, dave, carol); err != nil {
		t.Fatalf("unable to connect dave to carol: %v", err)
	}
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointCarol := openChannelAndAssert(
		ctxt, t, net, carol, dave, chanAmt, 0, false,
	)
	networkChans = append(networkChans, chanPointCarol)

	// Wait for all nodes to have seen all channels.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol, dave}
	nodeNames := []string{"Alice", "Bob", "Carol", "Dave"}
	for _, chanPoint := range networkChans {
		for i, node := range nodes {
			txidHash, err := getChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			txid, e := chainhash.NewHash(txidHash)
			if e != nil {
				t.Fatalf("unable to create sha hash: %v", e)
			}
			point := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ = context.WithTimeout(ctxb, timeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("%s(%d): timeout waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, point, err)
			}
		}
	}

	// Query for routes to pay from Alice to Dave.
	const paymentAmt = 1000
	routesReq := &lnrpc.QueryRoutesRequest{
		PubKey:    dave.PubKeyStr,
		Amt:       paymentAmt,
		NumRoutes: 1,
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	routesRes, err := net.Alice.QueryRoutes(ctxt, routesReq)
	if err != nil {
		t.Fatalf("unable to get route: %v", err)
	}

	const mSat = 1000
	feePerHopMSat := computeFee(1000, 1, paymentAmt*mSat)

	for i, route := range routesRes.Routes {
		expectedTotalFeesMSat :=
			lnwire.MilliSatoshi(len(route.Hops)-1) * feePerHopMSat
		expectedTotalAmtMSat := (paymentAmt * mSat) + expectedTotalFeesMSat

		if route.TotalFees != route.TotalFeesMsat/mSat {
			t.Fatalf("route %v: total fees %v (msat) does not "+
				"round down to %v (sat)",
				i, route.TotalFeesMsat, route.TotalFees)
		}
		if route.TotalFeesMsat != int64(expectedTotalFeesMSat) {
			t.Fatalf("route %v: total fees in msat expected %v got %v",
				i, expectedTotalFeesMSat, route.TotalFeesMsat)
		}

		if route.TotalAmt != route.TotalAmtMsat/mSat {
			t.Fatalf("route %v: total amt %v (msat) does not "+
				"round down to %v (sat)",
				i, route.TotalAmtMsat, route.TotalAmt)
		}
		if route.TotalAmtMsat != int64(expectedTotalAmtMSat) {
			t.Fatalf("route %v: total amt in msat expected %v got %v",
				i, expectedTotalAmtMSat, route.TotalAmtMsat)
		}

		// For all hops except the last, we check that fee equals feePerHop
		// and amount to foward deducts feePerHop on each hop.
		expectedAmtToForwardMSat := expectedTotalAmtMSat
		for j, hop := range route.Hops[:len(route.Hops)-1] {
			expectedAmtToForwardMSat -= feePerHopMSat

			if hop.Fee != hop.FeeMsat/mSat {
				t.Fatalf("route %v hop %v: fee %v (msat) does not "+
					"round down to %v (sat)",
					i, j, hop.FeeMsat, hop.Fee)
			}
			if hop.FeeMsat != int64(feePerHopMSat) {
				t.Fatalf("route %v hop %v: fee in msat expected %v got %v",
					i, j, feePerHopMSat, hop.FeeMsat)
			}

			if hop.AmtToForward != hop.AmtToForwardMsat/mSat {
				t.Fatalf("route %v hop %v: amt to forward %v (msat) does not "+
					"round down to %v (sat)",
					i, j, hop.AmtToForwardMsat, hop.AmtToForward)
			}
			if hop.AmtToForwardMsat != int64(expectedAmtToForwardMSat) {
				t.Fatalf("route %v hop %v: amt to forward in msat "+
					"expected %v got %v",
					i, j, expectedAmtToForwardMSat, hop.AmtToForwardMsat)
			}
		}
		// Last hop should have zero fee and amount to foward should equal
		// payment amount.
		hop := route.Hops[len(route.Hops)-1]

		if hop.Fee != 0 || hop.FeeMsat != 0 {
			t.Fatalf("route %v hop %v: fee expected 0 got %v (sat) %v (msat)",
				i, len(route.Hops)-1, hop.Fee, hop.FeeMsat)
		}

		if hop.AmtToForward != hop.AmtToForwardMsat/mSat {
			t.Fatalf("route %v hop %v: amt to forward %v (msat) does not "+
				"round down to %v (sat)",
				i, len(route.Hops)-1, hop.AmtToForwardMsat, hop.AmtToForward)
		}
		if hop.AmtToForwardMsat != paymentAmt*mSat {
			t.Fatalf("route %v hop %v: amt to forward in msat "+
				"expected %v got %v",
				i, len(route.Hops)-1, paymentAmt*mSat, hop.AmtToForwardMsat)
		}
	}

	// We clean up the test case by closing channels that were created for
	// the duration of the tests.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAlice, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, chanPointBob, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarol, false)
}

// testRouteFeeCutoff tests that we are able to prevent querying routes and
// sending payments that incur a fee higher than the fee limit.
func testRouteFeeCutoff(net *lntest.NetworkHarness, t *harnessTest) {
	// For this test, we'll create the following topology:
	//
	//              --- Bob ---
	//            /             \
	// Alice ----                 ---- Dave
	//            \             /
	//              -- Carol --
	//
	// Alice will attempt to send payments to Dave that should not incur a
	// fee greater than the fee limit expressed as a percentage of the
	// amount and as a fixed amount of satoshis.

	ctxb := context.Background()
	timeout := time.Duration(time.Second * 15)

	const chanAmt = btcutil.Amount(100000)

	// Open a channel between Alice and Bob.
	ctxt, _ := context.WithTimeout(ctxb, timeout)
	chanPointAliceBob := openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob, chanAmt, 0, false,
	)

	// Create Carol's node and open a channel between her and Alice with
	// Alice being the funder.
	carol, err := net.NewNode("Carol", nil)
	if err != nil {
		t.Fatalf("unable to create carol's node: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.ConnectNodes(ctxt, carol, net.Alice); err != nil {
		t.Fatalf("unable to connect carol to alice: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	err = net.SendCoins(ctxt, btcutil.SatoshiPerBitcoin, carol)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointAliceCarol := openChannelAndAssert(
		ctxt, t, net, net.Alice, carol, chanAmt, 0, false,
	)

	// Create Dave's node and open a channel between him and Bob with Bob
	// being the funder.
	dave, err := net.NewNode("Dave", nil)
	if err != nil {
		t.Fatalf("unable to create dave's node: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.ConnectNodes(ctxt, dave, net.Bob); err != nil {
		t.Fatalf("unable to connect dave to bob: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointBobDave := openChannelAndAssert(
		ctxt, t, net, net.Bob, dave, chanAmt, 0, false,
	)

	// Open a channel between Carol and Dave.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if err := net.ConnectNodes(ctxt, carol, dave); err != nil {
		t.Fatalf("unable to connect carol to dave: %v", err)
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	chanPointCarolDave := openChannelAndAssert(
		ctxt, t, net, carol, dave, chanAmt, 0, false,
	)

	// Now that all the channels were set up, we'll wait for all the nodes
	// to have seen all the channels.
	nodes := []*lntest.HarnessNode{net.Alice, net.Bob, carol, dave}
	nodeNames := []string{"alice", "bob", "carol", "dave"}
	networkChans := []*lnrpc.ChannelPoint{
		chanPointAliceBob, chanPointAliceCarol, chanPointBobDave,
		chanPointCarolDave,
	}
	for _, chanPoint := range networkChans {
		for i, node := range nodes {
			txidHash, err := getChanPointFundingTxid(chanPoint)
			if err != nil {
				t.Fatalf("unable to get txid: %v", err)
			}
			txid, e := chainhash.NewHash(txidHash)
			if e != nil {
				t.Fatalf("unable to create sha hash: %v", e)
			}
			outpoint := wire.OutPoint{
				Hash:  *txid,
				Index: chanPoint.OutputIndex,
			}

			ctxt, _ := context.WithTimeout(ctxb, timeout)
			err = node.WaitForNetworkChannelOpen(ctxt, chanPoint)
			if err != nil {
				t.Fatalf("%s(%d) timed out waiting for "+
					"channel(%s) open: %v", nodeNames[i],
					node.NodeID, outpoint, err)
			}
		}
	}

	// The payments should only be succesful across the route:
	//	Alice -> Bob -> Dave
	// Therefore, we'll update the fee policy on Carol's side for the
	// channel between her and Dave to invalidate the route:
	//	Alice -> Carol -> Dave
	const feeBase = 1e+6
	baseFee := int64(10000)
	feeRate := int64(5)
	timeLockDelta := uint32(144)

	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMsat:      baseFee,
		FeeRateMilliMsat: feeBase * feeRate,
		TimeLockDelta:    timeLockDelta,
	}

	updateFeeReq := &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   baseFee,
		FeeRate:       float64(feeRate),
		TimeLockDelta: timeLockDelta,
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: chanPointCarolDave,
		},
	}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	if _, err := carol.UpdateChannelPolicy(ctxt, updateFeeReq); err != nil {
		t.Fatalf("unable to update chan policy: %v", err)
	}

	// Wait for Alice to receive the channel update from Carol.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	aliceUpdates, aQuit := subscribeGraphNotifications(t, ctxt, net.Alice)
	defer close(aQuit)
	waitForChannelUpdate(
		t, aliceUpdates, carol.PubKeyStr, expectedPolicy,
		chanPointCarolDave,
	)

	// We'll also need the channel IDs for Bob's channels in order to
	// confirm the route of the payments.
	listReq := &lnrpc.ListChannelsRequest{}
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	listResp, err := net.Bob.ListChannels(ctxt, listReq)
	if err != nil {
		t.Fatalf("unable to retrieve bob's channels: %v", err)
	}

	var aliceBobChanID, bobDaveChanID uint64
	for _, channel := range listResp.Channels {
		switch channel.RemotePubkey {
		case net.Alice.PubKeyStr:
			aliceBobChanID = channel.ChanId
		case dave.PubKeyStr:
			bobDaveChanID = channel.ChanId
		}
	}

	if aliceBobChanID == 0 {
		t.Fatalf("channel between alice and bob not found")
	}
	if bobDaveChanID == 0 {
		t.Fatalf("channel between bob and dave not found")
	}
	hopChanIDs := []uint64{aliceBobChanID, bobDaveChanID}

	// checkRoute is a helper closure to ensure the route contains the
	// correct intermediate hops.
	checkRoute := func(route *lnrpc.Route) {
		if len(route.Hops) != 2 {
			t.Fatalf("expected two hops, got %d", len(route.Hops))
		}

		for i, hop := range route.Hops {
			if hop.ChanId != hopChanIDs[i] {
				t.Fatalf("expected chan id %d, got %d",
					hop.ChanId)
			}
		}
	}

	// We'll be attempting to send two payments from Alice to Dave. One will
	// have a fee cutoff expressed as a percentage of the amount and the
	// other will have it expressed as a fixed amount of satoshis.
	const paymentAmt = 100
	carolFee := computeFee(lnwire.MilliSatoshi(baseFee), 1, paymentAmt)

	// testFeeCutoff is a helper closure that will ensure the different
	// types of fee limits work as intended when querying routes and sending
	// payments.
	testFeeCutoff := func(feeLimit *lnrpc.FeeLimit) {
		queryRoutesReq := &lnrpc.QueryRoutesRequest{
			PubKey:    dave.PubKeyStr,
			Amt:       paymentAmt,
			FeeLimit:  feeLimit,
			NumRoutes: 2,
		}
		ctxt, _ = context.WithTimeout(ctxb, timeout)
		routesResp, err := net.Alice.QueryRoutes(ctxt, queryRoutesReq)
		if err != nil {
			t.Fatalf("unable to get routes: %v", err)
		}

		if len(routesResp.Routes) != 1 {
			t.Fatalf("expected one route, got %d",
				len(routesResp.Routes))
		}

		checkRoute(routesResp.Routes[0])

		invoice := &lnrpc.Invoice{Value: paymentAmt}
		ctxt, _ = context.WithTimeout(ctxb, timeout)
		invoiceResp, err := dave.AddInvoice(ctxt, invoice)
		if err != nil {
			t.Fatalf("unable to create invoice: %v", err)
		}

		sendReq := &lnrpc.SendRequest{
			PaymentRequest: invoiceResp.PaymentRequest,
			FeeLimit:       feeLimit,
		}
		ctxt, _ = context.WithTimeout(ctxb, timeout)
		paymentResp, err := net.Alice.SendPaymentSync(ctxt, sendReq)
		if err != nil {
			t.Fatalf("unable to send payment: %v", err)
		}
		if paymentResp.PaymentError != "" {
			t.Fatalf("unable to send payment: %v",
				paymentResp.PaymentError)
		}

		checkRoute(paymentResp.PaymentRoute)
	}

	// We'll start off using percentages first. Since the fee along the
	// route using Carol as an intermediate hop is 10% of the payment's
	// amount, we'll use a lower percentage in order to invalid that route.
	feeLimitPercent := &lnrpc.FeeLimit{
		&lnrpc.FeeLimit_Percent{baseFee/1000 - 1},
	}
	testFeeCutoff(feeLimitPercent)

	// Now we'll test using fixed fee limit amounts. Since we computed the
	// fee for the route using Carol as an intermediate hop earlier, we can
	// use a smaller value in order to invalidate that route.
	feeLimitFixed := &lnrpc.FeeLimit{
		&lnrpc.FeeLimit_Fixed{int64(carolFee.ToSatoshis()) - 1},
	}
	testFeeCutoff(feeLimitFixed)

	// Once we're done, close the channels and shut down the nodes created
	// throughout this test.
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAliceBob, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Alice, chanPointAliceCarol, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, net.Bob, chanPointBobDave, false)
	ctxt, _ = context.WithTimeout(ctxb, timeout)
	closeChannelAndAssert(ctxt, t, net, carol, chanPointCarolDave, false)

	if err := net.ShutdownNode(carol); err != nil {
		t.Fatalf("unable to shut down carol: %v", err)
	}
	if err := net.ShutdownNode(dave); err != nil {
		t.Fatalf("unable to shut down dave: %v", err)
	}
}

type testCase struct {
	name string
	test func(net *lntest.NetworkHarness, t *harnessTest)
}

var testsCases = []*testCase{
	{
		name: "onchain fund recovery",
		test: testOnchainFundRecovery,
	},
	{
		name: "basic funding flow",
		test: testBasicChannelFunding,
	},
	{
		name: "update channel policy",
		test: testUpdateChannelPolicy,
	},
	{
		name: "open channel reorg test",
		test: testOpenChannelAfterReorg,
	},
	{
		name: "disconnecting target peer",
		test: testDisconnectingTargetPeer,
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
		name: "sphinx replay persistence",
		test: testSphinxReplayPersistence,
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
		name: "single-hop send to route",
		test: testSingleHopSendToRoute,
	},
	{
		name: "multi-hop send to route",
		test: testMultiHopSendToRoute,
	},
	{
		name: "send to route error propagation",
		test: testSendToRouteErrorPropagation,
	},
	{
		name: "private channels",
		test: testPrivateChannels,
	},
	{
		name: "invoice routing hints",
		test: testInvoiceRoutingHints,
	},
	{
		name: "multi-hop payments over private channels",
		test: testMultiHopOverPrivateChannels,
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
		name: "node announcement",
		test: testNodeAnnouncement,
	},
	{
		name: "node sign verify",
		test: testNodeSignVerify,
	},
	{
		name: "async payments benchmark",
		test: testAsyncPayments,
	},
	{
		name: "async bidirectional payments",
		test: testBidirectionalAsyncPayments,
	},
	{
		// bob: outgoing our commit timeout
		// carol: incoming their commit watch and see timeout
		name: "test multi-hop htlc local force close immediate expiry",
		test: testMultiHopHtlcLocalTimeout,
	},
	{
		// bob: outgoing watch and see, they sweep on chain
		// carol: incoming our commit, know preimage
		name: "test multi-hop htlc receiver chain claim",
		test: testMultiHopReceiverChainClaim,
	},
	{
		// bob: outgoing our commit watch and see timeout
		// carol: incoming their commit watch and see timeout
		name: "test multi-hop local force close on-chain htlc timeout",
		test: testMultiHopLocalForceCloseOnChainHtlcTimeout,
	},
	{
		// bob: outgoing their commit watch and see timeout
		// carol: incoming our commit watch and see timeout
		name: "test multi-hop remote force close on-chain htlc timeout",
		test: testMultiHopRemoteForceCloseOnChainHtlcTimeout,
	},
	{
		// bob: outgoing our commit watch and see, they sweep on chain
		// bob: incoming our commit watch and learn preimage
		// carol: incoming their commit know preimage
		name: "test multi-hop htlc local chain claim",
		test: testMultiHopHtlcLocalChainClaim,
	},
	{
		// bob: outgoing their commit watch and see, they sweep on chain
		// bob: incoming their commit watch and learn preimage
		// carol: incoming our commit know preimage
		name: "test multi-hop htlc remote chain claim",
		test: testMultiHopHtlcRemoteChainClaim,
	},
	{
		name: "switch circuit persistence",
		test: testSwitchCircuitPersistence,
	},
	{
		name: "switch offline delivery",
		test: testSwitchOfflineDelivery,
	},
	{
		name: "switch offline delivery persistence",
		test: testSwitchOfflineDeliveryPersistence,
	},
	{
		name: "switch offline delivery outgoing offline",
		test: testSwitchOfflineDeliveryOutgoingOffline,
	},
	{
		// TODO(roasbeef): test always needs to be last as Bob's state
		// is borked since we trick him into attempting to cheat Alice?
		name: "revoked uncooperative close retribution",
		test: testRevokedCloseRetribution,
	},
	{
		name: "failing link",
		test: testFailingChannel,
	},
	{
		name: "revoked uncooperative close retribution zero value remote output",
		test: testRevokedCloseRetributionZeroValueRemoteOutput,
	},
	{
		name: "revoked uncooperative close retribution remote hodl",
		test: testRevokedCloseRetributionRemoteHodl,
	},
	{
		name: "query routes",
		test: testQueryRoutes,
	},
	{
		name: "route fee cutoff",
		test: testRouteFeeCutoff,
	},
}

// TestLightningNetworkDaemon performs a series of integration tests amongst a
// programmatically driven network of lnd nodes.
func TestLightningNetworkDaemon(t *testing.T) {
	ht := newHarnessTest(t)

	var lndHarness *lntest.NetworkHarness

	// First create an instance of the btcd's rpctest.Harness. This will be
	// used to fund the wallets of the nodes within the test network and to
	// drive blockchain related events within the network. Revert the default
	// setting of accepting non-standard transactions on simnet to reject them.
	// Transactions on the lightning network should always be standard to get
	// better guarantees of getting included in to blocks.
	args := []string{"--rejectnonstd"}
	handlers := &rpcclient.NotificationHandlers{
		OnTxAccepted: func(hash *chainhash.Hash, amt btcutil.Amount) {
			lndHarness.OnTxAccepted(hash)
		},
	}
	btcdHarness, err := rpctest.New(harnessNetParams, handlers, args)
	if err != nil {
		ht.Fatalf("unable to create mining node: %v", err)
	}
	defer btcdHarness.TearDown()

	// First create the network harness to gain access to its
	// 'OnTxAccepted' call back.
	lndHarness, err = lntest.NewNetworkHarness(btcdHarness)
	if err != nil {
		ht.Fatalf("unable to create lightning network harness: %v", err)
	}
	defer lndHarness.TearDownAll()

	// Spawn a new goroutine to watch for any fatal errors that any of the
	// running lnd processes encounter. If an error occurs, then the test
	// case should naturally as a result and we log the server error here to
	// help debug.
	go func() {
		for {
			select {
			case err, more := <-lndHarness.ProcessErrors():
				if !more {
					return
				}
				ht.Logf("lnd finished with error (stderr):\n%v", err)
			}
		}
	}()

	// Turn off the btcd rpc logging, otherwise it will lead to panic.
	// TODO(andrew.shvv|roasbeef) Remove the hack after re-work the way the log
	// rotator os work.
	rpcclient.UseLogger(btclog.Disabled)

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
	if err = lndHarness.SetUp(nil); err != nil {
		ht.Fatalf("unable to set up test lightning network: %v", err)
	}

	t.Logf("Running %v integration tests", len(testsCases))
	for _, testCase := range testsCases {
		logLine := fmt.Sprintf("STARTING ============ %v ============\n",
			testCase.name)
		if err := lndHarness.Alice.AddToLog(logLine); err != nil {
			t.Fatalf("unable to add to log: %v", err)
		}
		if err := lndHarness.Bob.AddToLog(logLine); err != nil {
			t.Fatalf("unable to add to log: %v", err)
		}

		success := t.Run(testCase.name, func(t1 *testing.T) {
			ht := newHarnessTest(t1)
			ht.RunTestCase(testCase, lndHarness)
		})

		// Stop at the first failure. Mimic behavior of original test
		// framework.
		if !success {
			break
		}
	}
}
