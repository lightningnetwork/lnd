package main

import (
	"bytes"
	"fmt"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/context"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/roasbeef/btcd/rpctest"
	"github.com/roasbeef/btcd/wire"
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

	// First establish a channel between Alice and Bob.
	openReq := &lnrpc.OpenChannelRequest{
		// TODO(roasbeef): should pass actual id instead, will fail if
		// more connections added for Alice.
		TargetPeerId:        1,
		LocalFundingAmount:  btcutil.SatoshiPerBitcoin / 2,
		RemoteFundingAmount: 0,
		NumConfs:            1,
	}
	time.Sleep(time.Millisecond * 500)
	respStream, err := net.AliceClient.OpenChannel(ctxb, openReq)
	if err != nil {
		t.Fatalf("unable to open channel between alice and bob: %v", err)
	}

	// Mine a block, the funding txid should be included, and both nodes should
	// be aware of the channel.
	// TODO(roasbeef): replace sleep with something more robust
	time.Sleep(time.Second * 1)
	blockHash, err := net.Miner.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	resp, err := respStream.Recv()
	if err != nil {
		t.Fatalf("unable to read rpc resp: %v", err)
	}
	block, err := net.Miner.Node.GetBlock(blockHash[0])
	if err != nil {
		t.Fatalf("unable to get block: %v", err)
	}
	if len(block.Transactions()) < 2 {
		t.Fatalf("funding transaction not included")
	}

	fundingTxID, _ := wire.NewShaHash(resp.ChannelPoint.FundingTxid)
	fundingTxIDStr := fundingTxID.String()
	assertTxInBlock(block, fundingTxID, t)

	// TODO(roasbeef): remove and use "listchannels" command after
	// implemented.
	req := &lnrpc.ListPeersRequest{}
	alicePeerInfo, err := net.AliceClient.ListPeers(ctxb, req)
	if err != nil {
		t.Fatalf("unable to list alice peers: %v", err)
	}
	bobPeerInfo, err := net.BobClient.ListPeers(ctxb, req)
	if err != nil {
		t.Fatalf("unable to list bob peers: %v", err)
	}

	// The channel should be listed in the peer information returned by
	// both peers.
	aliceTxID := alicePeerInfo.Peers[0].Channels[0].ChannelPoint
	bobTxID := bobPeerInfo.Peers[0].Channels[0].ChannelPoint
	if !strings.Contains(bobTxID, fundingTxIDStr) {
		t.Fatalf("alice's channel not found")
	}
	if !strings.Contains(aliceTxID, fundingTxIDStr) {
		t.Fatalf("bob's channel not found")
	}

	// Initiate a close from Alice's side. After mining a block, the closing
	// transaction should be included, and both nodes should have forgotten
	// about the channel.
	closeReq := &lnrpc.CloseChannelRequest{
		ChannelPoint: resp.ChannelPoint,
	}
	closeRespStream, err := net.AliceClient.CloseChannel(ctxb, closeReq)
	if err != nil {
		t.Fatalf("unable to close channel: %v", err)
	}
	time.Sleep(time.Second * 1)
	blockHash, err = net.Miner.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	block, err = net.Miner.Node.GetBlock(blockHash[0])
	if err != nil {
		t.Fatalf("unable to get block: %v", err)
	}
	closeResp, err := closeRespStream.Recv()
	if err != nil {
		t.Fatalf("unable to read rpc resp: %v", err)
	}

	closingTxID, _ := wire.NewShaHash(closeResp.ClosingTxid)
	assertTxInBlock(block, closingTxID, t)
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
