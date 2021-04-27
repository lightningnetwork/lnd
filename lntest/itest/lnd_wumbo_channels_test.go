package itest

import (
	"context"
	"strings"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lntest"
)

// testWumboChannels tests that only a node that signals wumbo channel
// acceptances will allow a wumbo channel to be created. Additionally, if a
// node is running with mini channels only enabled, then they should reject any
// inbound wumbo channel requests.
func testWumboChannels(net *lntest.NetworkHarness, t *harnessTest) {
	// With all the channel types exercised, we'll now make sure the wumbo
	// signalling support works properly.
	//
	// We'll make two new nodes, with one of them signalling support for
	// wumbo channels while the other doesn't.
	wumboNode, err := net.NewNode(
		"wumbo", []string{"--protocol.wumbo-channels"},
	)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	defer shutdownAndAssert(net, t, wumboNode)
	miniNode, err := net.NewNode("mini", nil)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	defer shutdownAndAssert(net, t, miniNode)

	// We'll send coins to the wumbo node, as it'll be the one imitating
	// the channel funding.
	ctxb := context.Background()
	err = net.SendCoins(ctxb, btcutil.SatoshiPerBitcoin, wumboNode)
	if err != nil {
		t.Fatalf("unable to send coins to carol: %v", err)
	}

	// Next we'll connect both nodes, then attempt to make a wumbo channel
	// funding request to the mini node we created above. The wumbo request
	// should fail as the node isn't advertising wumbo channels.
	err = net.EnsureConnected(ctxb, wumboNode, miniNode)
	if err != nil {
		t.Fatalf("unable to connect peers: %v", err)
	}

	chanAmt := funding.MaxBtcFundingAmount + 1
	_, err = net.OpenChannel(
		ctxb, wumboNode, miniNode, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	if err == nil {
		t.Fatalf("expected wumbo channel funding to fail")
	}

	// The test should indicate a failure due to the channel being too
	// large.
	if !strings.Contains(err.Error(), "exceeds maximum chan size") {
		t.Fatalf("channel should be rejected due to size, instead "+
			"error was: %v", err)
	}

	// We'll now make another wumbo node to accept our wumbo channel
	// funding.
	wumboNode2, err := net.NewNode(
		"wumbo2", []string{"--protocol.wumbo-channels"},
	)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	defer shutdownAndAssert(net, t, wumboNode2)

	// Creating a wumbo channel between these two nodes should succeed.
	err = net.EnsureConnected(ctxb, wumboNode, wumboNode2)
	if err != nil {
		t.Fatalf("unable to connect peers: %v", err)
	}
	chanPoint := openChannelAndAssert(
		ctxb, t, net, wumboNode, wumboNode2,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	closeChannelAndAssert(ctxb, t, net, wumboNode, chanPoint, false)
}
