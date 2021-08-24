package itest

import (
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
	wumboNode := net.NewNode(
		t.t, "wumbo", []string{"--protocol.wumbo-channels"},
	)
	defer shutdownAndAssert(net, t, wumboNode)
	miniNode := net.NewNode(t.t, "mini", nil)
	defer shutdownAndAssert(net, t, miniNode)

	// We'll send coins to the wumbo node, as it'll be the one imitating
	// the channel funding.
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, wumboNode)

	// Next we'll connect both nodes, then attempt to make a wumbo channel
	// funding request to the mini node we created above. The wumbo request
	// should fail as the node isn't advertising wumbo channels.
	net.EnsureConnected(t.t, wumboNode, miniNode)

	chanAmt := funding.MaxBtcFundingAmount + 1
	_, err := net.OpenChannel(
		wumboNode, miniNode, lntest.OpenChannelParams{
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
	wumboNode2 := net.NewNode(
		t.t, "wumbo2", []string{"--protocol.wumbo-channels"},
	)
	defer shutdownAndAssert(net, t, wumboNode2)

	// Creating a wumbo channel between these two nodes should succeed.
	net.EnsureConnected(t.t, wumboNode, wumboNode2)
	chanPoint := openChannelAndAssert(
		t, net, wumboNode, wumboNode2,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	closeChannelAndAssert(t, net, wumboNode, chanPoint, false)
}
