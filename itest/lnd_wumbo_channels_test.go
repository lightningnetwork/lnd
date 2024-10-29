package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// testWumboChannels tests that only a node that signals wumbo channel
// acceptances will allow a wumbo channel to be created. Additionally, if a
// node is running with mini channels only enabled, then they should reject any
// inbound wumbo channel requests.
func testWumboChannels(ht *lntest.HarnessTest) {
	// With all the channel types exercised, we'll now make sure the wumbo
	// signalling support works properly.
	//
	// We'll make two new nodes, with one of them signalling support for
	// wumbo channels while the other doesn't.
	wumboNode := ht.NewNode("wumbo", []string{"--protocol.wumbo-channels"})
	miniNode := ht.NewNode("mini", nil)

	// We'll send coins to the wumbo node, as it'll be the one imitating
	// the channel funding.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, wumboNode)

	// Next we'll connect both nodes, then attempt to make a wumbo channel
	// funding request to the mini node we created above. The wumbo request
	// should fail as the node isn't advertising wumbo channels.
	ht.EnsureConnected(wumboNode, miniNode)

	chanAmt := funding.MaxBtcFundingAmount + 1
	// The test should indicate a failure due to the channel being too
	// large.
	ht.OpenChannelAssertErr(
		wumboNode, miniNode, lntest.OpenChannelParams{Amt: chanAmt},
		lnwallet.ErrChanTooLarge(chanAmt, funding.MaxBtcFundingAmount),
	)

	// We'll now make another wumbo node to accept our wumbo channel
	// funding.
	wumboNode2 := ht.NewNode(
		"wumbo2", []string{"--protocol.wumbo-channels"},
	)

	// Creating a wumbo channel between these two nodes should succeed.
	ht.EnsureConnected(wumboNode, wumboNode2)
	ht.OpenChannel(
		wumboNode, wumboNode2, lntest.OpenChannelParams{Amt: chanAmt},
	)
}
