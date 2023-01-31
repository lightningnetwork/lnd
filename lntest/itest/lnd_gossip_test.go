package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lntemp"
)

// testGossipIsolatedGraphs creates a network with 2 isolated graphs which are
// connected via a single node and asserts that the graphs are synced across
// all nodes. The topology is as follows:
//
//	Alice <=> Bob
//	           |
//	          Eve
//	           |
//	Carol <=> Dave
//
// The test asserts that Eve is aware of all the channels.
func testGossipIsolatedGraphs(ht *lntemp.HarnessTest) {
	// Get Alice and Bob, then restart them with the new extra args.
	alice, bob := ht.Alice, ht.Bob
	ht.RestartNodeWithExtraArgs(alice, []string{
		"--maxpendingchannels=2",
	})
	ht.RestartNodeWithExtraArgs(bob, []string{
		"--maxpendingchannels=2",
	})

	// Create two nodes Carol and Dave.
	carol := ht.NewNode("Carol", []string{
		"--maxpendingchannels=2",
	})
	dave := ht.NewNode("Dave", []string{
		"--maxpendingchannels=2",
	})

	// Create a new Eve to test the gossip.
	eve := ht.NewNode("Eve", nil)

	// Fund coins for Carol and Dave.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, dave)

	// Make the connection happen.
	ht.EnsureConnected(alice, bob)
	ht.EnsureConnected(carol, dave)

	// Create four channels,
	// 1. Alice -> Bob
	// 2. Bob -> Alice
	// 3. Carol -> Dave
	// 4. Dave -> Carol
	reqs := []*lntemp.OpenChannelRequest{
		{
			Local:  alice,
			Remote: bob,
			Param:  lntemp.OpenChannelParams{Amt: 100_000},
		},
		{
			Local:  bob,
			Remote: alice,
			Param:  lntemp.OpenChannelParams{Amt: 100_000},
		},
		{
			Local:  carol,
			Remote: dave,
			Param:  lntemp.OpenChannelParams{Amt: 200_000},
		},
		{
			Local:  dave,
			Remote: carol,
			Param:  lntemp.OpenChannelParams{Amt: 200_000},
		},
	}

	// Open all the channels in parallel.
	chanPoints := ht.OpenMultiChannelsAsync(reqs)

	// Connect Eve to Bob and Dave so she will hear the channels from them.
	ht.EnsureConnected(bob, eve)
	ht.EnsureConnected(dave, eve)

	// Check that the graphs are synced across all nodes.
	//
	// TODO(yy): also check that Alice and Carol have heard all the
	// channels. Atm they don't know the full graph due to the syncers not
	// sending the channel updates.
	for _, cp := range chanPoints {
		// Eve should see all the channels from Bob and Dave.
		ht.AssertTopologyChannelOpen(eve, cp)
	}
}
