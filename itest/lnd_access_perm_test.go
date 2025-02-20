package itest

import (
	"strconv"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
)

// testAccessPerm tests that the number of restricted slots is upheld when
// connecting to the server from a restrictedd peer.
func testAccessPerm(ht *lntest.HarnessTest) {
	args := []string{
		"--minbackoff=1m",
		"--maxbackoff=1m",
		"--num-restricted-slots=5",
	}

	alice := ht.NewNodeWithCoins("Alice", args)
	bob := ht.NewNodeWithCoins("Bob", args)
	ht.ConnectNodes(alice, bob)

	// Open a confirmed channel to Bob. Bob will have protected access.
	chanPoint1 := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	defer ht.CloseChannel(alice, chanPoint1)

	// Open and close channel to Carol. Carol will have protected access.
	carol := ht.NewNodeWithCoins("Carol", args)
	ht.ConnectNodes(alice, carol)

	chanPoint2 := ht.OpenChannel(
		alice, carol, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	ht.CloseChannel(alice, chanPoint2)

	// Make a pending channel with Dave.
	dave := ht.NewNodeWithCoins("Dave", args)
	ht.ConnectNodes(alice, dave)

	ht.OpenChannelAssertStream(
		dave, alice, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Disconnect Bob, Carol, and Dave.
	ht.DisconnectNodes(alice, bob)
	ht.AssertNotConnected(alice, bob)

	ht.DisconnectNodes(alice, carol)
	ht.AssertNotConnected(alice, carol)

	ht.DisconnectNodes(alice, dave)
	ht.AssertNotConnected(alice, dave)

	// Connect 5 times to Alice. All of these connections should be
	// successful.
	for i := 0; i < 5; i++ {
		peer := ht.NewNode("Peer"+strconv.Itoa(i), args)
		ht.ConnectNodes(peer, alice)
		ht.AssertConnected(peer, alice)
	}

	// Connect an additional time to Alice. This should fail.
	failedPeer := ht.NewNode("FailedPeer", args)
	req := &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: alice.RPC.GetInfo().IdentityPubkey,
			Host:   alice.Cfg.P2PAddr(),
		},
	}
	failedPeer.RPC.ConnectPeer(req)
	ht.AssertNotConnected(failedPeer, alice)

	// Connect nodes and assert access status.
	ht.ConnectNodes(alice, bob)
	ht.AssertConnected(alice, bob)

	ht.ConnectNodes(alice, carol)
	ht.AssertConnected(alice, carol)

	ht.ConnectNodes(alice, dave)
	ht.AssertConnected(alice, dave)

	ht.MineBlocksAndAssertNumTxes(1, 1)
}
