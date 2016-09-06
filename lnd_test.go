package main

import (
	"bytes"
	"fmt"
	"runtime/debug"
	"testing"

	"golang.org/x/net/context"

	"github.com/roasbeef/btcd/rpctest"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcrpcclient"
	"github.com/roasbeef/btcutil"
)

type lndTestCase func(net *networkHarness, t *testing.T)

func assertTxInBlock(block *btcutil.Block, txid *wire.ShaHash, t *testing.T) {
	for _, tx := range block.Transactions() {
		if bytes.Equal(txid[:], tx.Sha()[:]) {
			return
		}
	}

	t.Fatalf("funding tx was not included in block")
}

// testBasicChannelFunding performs a test excercising expected behavior from a
// basic funding workflow. The test creates a new channel between Alice and
// Bob, then immediately closes the channel after asserting some expected post
// conditions. Finally, the chain itelf is checked to ensure the closing
// transaction was mined.
func testBasicChannelFunding(net *networkHarness, t *testing.T) {
	ctxb := context.Background()

	// First establish a channel ween with a capacity of 0.5 BTC between
	// Alice and Bob.
	chanAmt := btcutil.Amount(btcutil.SatoshiPerBitcoin / 2)
	chanOpenUpdate, err := net.OpenChannel(ctxb, net.AliceClient, net.BobClient, chanAmt, 1)
	if err != nil {
		t.Fatalf("unable to open channel: %v", err)
	}

	// Mine a block, then wait for Alice's node to notify us that the
	// channel has been opened. The funding transaction should be found
	// within the newly mined block.
	blockHash, err := net.Miner.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	block, err := net.Miner.Node.GetBlock(blockHash[0])
	if err != nil {
		t.Fatalf("unable to get block: %v", err)
	}
	fundingChanPoint, err := net.WaitForChannelOpen(chanOpenUpdate)
	if err != nil {
		t.Fatalf("error while waiting for channeel open: %v", err)
	}
	fundingTxID, err := wire.NewShaHash(fundingChanPoint.FundingTxid)
	if err != nil {
		t.Fatalf("unable to create sha hash: %v", err)
	}
	assertTxInBlock(block, fundingTxID, t)

	// The channel should be listed in the peer information returned by
	// both peers.
	chanPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: fundingChanPoint.OutputIndex,
	}
	err = net.AssertChannelExists(ctxb, net.AliceClient, &chanPoint)
	if err != nil {
		t.Fatalf("unable to assert channel existence: %v", err)
	}

	// Initiate a close from Alice's side.
	closeUpdates, err := net.CloseChannel(ctxb, net.AliceClient, fundingChanPoint, false)
	if err != nil {
		t.Fatalf("unable to clsoe channel: %v", err)
	}

	// Finally, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block.
	blockHash, err = net.Miner.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	block, err = net.Miner.Node.GetBlock(blockHash[0])
	if err != nil {
		t.Fatalf("unable to get block: %v", err)
	}

	closingTxid, err := net.WaitForChannelClose(closeUpdates)
	if err != nil {
		t.Fatalf("error while waiting for channel close: %v", err)
	}
	assertTxInBlock(block, closingTxid, t)
}

var lndTestCases = map[string]lndTestCase{
	"basic funding flow": testBasicChannelFunding,
}

// TestLightningNetworkDaemon performs a series of integration tests amongst a
// programatically driven network of lnd nodes.
func TestLightningNetworkDaemon(t *testing.T) {
	var btcdHarness *rpctest.Harness
	var lightningNetwork *networkHarness
	var currentTest string
	var err error

	defer func() {
		// If one of the integration tests caused a panic within the main
		// goroutine, then tear down all the harnesses in order to avoid
		// any leaked processes.
		if r := recover(); r != nil {
			fmt.Println("recovering from test panic: ", r)
			if err := btcdHarness.TearDown(); err != nil {
				fmt.Println("unable to tear btcd harnesses: ", err)
			}
			if err := lightningNetwork.TearDownAll(); err != nil {
				fmt.Println("unable to tear lnd harnesses: ", err)
			}
			t.Fatalf("test %v panicked: %s", currentTest, debug.Stack())
		}
	}()

	// First create the network harness to gain access to its
	// 'OnTxAccepted' call back.
	lightningNetwork, err = newNetworkHarness(nil)
	if err != nil {
		t.Fatalf("unable to create lightning network harness: %v", err)
	}
	defer lightningNetwork.TearDownAll()

	handlers := &btcrpcclient.NotificationHandlers{
		OnTxAccepted: lightningNetwork.OnTxAccepted,
	}

	// First create an intance of the btcd's rpctest.Harness. This will be
	// used to fund the wallets of the nodes within the test network and to
	// drive blockchain related events within the network.
	btcdHarness, err = rpctest.New(harnessNetParams, handlers, nil)
	if err != nil {
		t.Fatalf("unable to create mining node: %v", err)
	}
	defer btcdHarness.TearDown()
	if err = btcdHarness.SetUp(true, 50); err != nil {
		t.Fatalf("unable to set up mining node: %v", err)
	}
	if err := btcdHarness.Node.NotifyNewTransactions(false); err != nil {
		t.Fatalf("unable to request transaction notifications: %v", err)
	}

	// With the btcd harness created, we can now complete the
	// initialization of the network.
	if err := lightningNetwork.InitializeSeedNodes(btcdHarness); err != nil {
		t.Fatalf("unable to initialize seed nodes: %v", err)
	}
	if err = lightningNetwork.SetUp(); err != nil {
		t.Fatalf("unable to set up test lightning network: %v", err)
	}

	t.Logf("Running %v integration tests", len(lndTestCases))
	for testName, lnTest := range lndTestCases {
		t.Logf("Executing test %v", testName)

		currentTest = testName
		lnTest(lightningNetwork, t)
	}
}
