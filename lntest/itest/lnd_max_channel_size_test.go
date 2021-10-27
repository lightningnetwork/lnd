//go:build rpctest
// +build rpctest

package itest

import (
	"fmt"

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
	defer ht.Shutdown(wumboNode)
	miniNode := ht.NewNode("mini", nil)
	defer ht.Shutdown(miniNode)

	// We'll send coins to the wumbo node, as it'll be the one imitating
	// the channel funding.
	ht.SendCoins(btcutil.SatoshiPerBitcoin, wumboNode)

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
	defer ht.Shutdown(wumboNode2)

	// Creating a wumbo channel between these two nodes should succeed.
	ht.EnsureConnected(wumboNode, wumboNode2)
	chanPoint := ht.OpenChannel(
		wumboNode, wumboNode2, lntest.OpenChannelParams{Amt: chanAmt},
	)
	ht.CloseChannel(wumboNode, chanPoint, false)
}

// testMaxChannelSize tests that lnd handles --maxchansize parameter correctly.
// Wumbo nodes should enforce a default soft limit of 10 BTC by default. This
// limit can be adjusted with --maxchansize config option
func testMaxChannelSize(ht *lntest.HarnessTest) {
	// We'll make two new nodes, both wumbo but with the default limit on
	// maximum channel size (10 BTC)
	wumboNode := ht.NewNode("wumbo", []string{"--protocol.wumbo-channels"})
	defer ht.Shutdown(wumboNode)

	wumboNode2 := ht.NewNode("wumbo2", []string{"--protocol.wumbo-channels"})
	defer ht.Shutdown(wumboNode2)

	// We'll send 11 BTC to the wumbo node so it can test the wumbo soft
	// limit.
	ht.SendCoins(11*btcutil.SatoshiPerBitcoin, wumboNode)

	// Next we'll connect both nodes, then attempt to make a wumbo channel
	// funding request, which should fail as it exceeds the default wumbo
	// soft limit of 10 BTC.
	ht.EnsureConnected(wumboNode, wumboNode2)

	chanAmt := funding.MaxBtcFundingAmountWumbo + 1
	// The test should show failure due to the channel exceeding our max
	// size.
	expectedErr := lnwallet.ErrChanTooLarge(
		chanAmt, funding.MaxBtcFundingAmountWumbo,
	)
	ht.OpenChannelAssertErr(
		wumboNode, wumboNode2,
		lntest.OpenChannelParams{Amt: chanAmt}, expectedErr,
	)

	// We'll now make another wumbo node with appropriate maximum channel
	// size to accept our wumbo channel funding.
	wumboNode3 := ht.NewNode(
		"wumbo3", []string{
			"--protocol.wumbo-channels",
			fmt.Sprintf(
				"--maxchansize=%v",
				int64(funding.MaxBtcFundingAmountWumbo+1),
			),
		},
	)
	defer ht.Shutdown(wumboNode3)

	// Creating a wumbo channel between these two nodes should succeed.
	ht.EnsureConnected(wumboNode, wumboNode3)
	chanPoint := ht.OpenChannel(
		wumboNode, wumboNode3, lntest.OpenChannelParams{Amt: chanAmt},
	)

	ht.CloseChannel(wumboNode, chanPoint, false)
}
