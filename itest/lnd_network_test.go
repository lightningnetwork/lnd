package itest

import (
	"fmt"
	"net"
	"time"

	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// testNetworkConnectionTimeout checks that the connectiontimeout is taking
// effect. It creates a node with a small connection timeout value, and
// connects it to a non-routable IP address.
func testNetworkConnectionTimeout(ht *lntest.HarnessTest) {
	var (
		// testPub is a random public key for testing only.
		testPub = "0332bda7da70fefe4b6ab92f53b3c4f4ee7999" +
			"f312284a8e89c8670bb3f67dbee2"

		// testHost is a reachable IP address with an unreachable port
		// that's used for testing only.
		testHost = "lightning.engineering:81"
	)

	// First, test the global timeout settings.
	// Create Carol with a connection timeout of 1 millisecond.
	carol := ht.NewNode("Carol", []string{"--connectiontimeout=1ms"})

	// Try to connect Carol to a non-routable IP address, which should give
	// us a timeout error.
	req := &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: testPub,
			Host:   testHost,
		},
	}

	// assertTimeoutError asserts that a connection timeout error is
	// raised. A context with a default timeout is used to make the
	// request. If our customized connection timeout is less than the
	// default, we won't see the request context times out, instead a
	// network connection timeout will be returned.
	assertTimeoutError := func(hn *node.HarnessNode,
		req *lnrpc.ConnectPeerRequest) {

		err := hn.RPC.ConnectPeerAssertErr(req)

		// Check that the network returns a timeout error.
		require.Containsf(ht, err.Error(), "i/o timeout",
			"expected to get a timeout error, instead got: %v", err)
	}

	assertTimeoutError(carol, req)

	// Second, test timeout on the connect peer request.
	// Create Dave with the default timeout setting.
	dave := ht.NewNode("Dave", nil)

	// Try to connect Dave to a non-routable IP address, using a timeout
	// value of 1ms, which should give us a timeout error immediately.
	req = &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: testPub,
			Host:   testHost,
		},
		Timeout: 1,
	}
	assertTimeoutError(dave, req)
}

// testReconnectAfterIPChange verifies that if a persistent inbound node changes
// its listening address then it's peer will still be able to reconnect to it.
func testReconnectAfterIPChange(ht *lntest.HarnessTest) {
	// In this test, the following network will be set up. A single
	// dash line represents a peer connection and a double dash line
	// represents a channel.
	// Charlie will create a connection to Dave so that Dave is the inbound
	// peer. This will be made a persistent connection for Charlie so that
	// Charlie will attempt to reconnect to Dave if Dave restarts.
	// A channel will be opened between Dave and Alice to ensure that any
	// NodeAnnouncements that Dave sends will reach Alice.
	// The connection between Alice and Charlie ensures that Charlie
	// receives all of Dave's NodeAnnouncements.
	// The desired behaviour is that if Dave changes his P2P IP address then
	// Charlie should still be able to reconnect to him.
	//
	//    /------- Charlie <-----\
	//    |                      |
	//    v                      |
	//   Dave <===============> Alice

	// The first thing we will test is the case where Dave advertises two
	// external IP addresses and then switches from the first one listed
	// to the second one listed. The desired behaviour is that Charlie will
	// attempt both of Dave's advertised addresses when attempting to
	// reconnect.

	// Create a new node, Charlie.
	charlie := ht.NewNode("Charlie", nil)

	// We derive an extra port for Dave, and we initialise his node with
	// the port advertised as `--externalip` arguments.
	ip2 := node.NextAvailablePort()

	// Create a new node, Dave, which will initialize a P2P port for him.
	daveArgs := []string{fmt.Sprintf("--externalip=127.0.0.1:%d", ip2)}
	dave := ht.NewNode("Dave", daveArgs)

	// We now have two ports, the initial P2P port from creating the node,
	// and the `externalip` specified above.
	advertisedAddrs := []string{
		fmt.Sprintf("127.0.0.1:%d", dave.Cfg.P2PPort),
		fmt.Sprintf("127.0.0.1:%d", ip2),
	}

	// Connect Alice to Dave and Charlie.
	alice := ht.Alice
	ht.ConnectNodes(alice, dave)
	ht.ConnectNodes(alice, charlie)

	// We'll then go ahead and open a channel between Alice and Dave. This
	// ensures that Charlie receives the node announcement from Alice as
	// part of the announcement broadcast.
	chanPoint := ht.OpenChannel(
		alice, dave, lntest.OpenChannelParams{Amt: 1000000},
	)

	// waitForNodeAnnouncement is a closure used to wait on the given graph
	// subscription for a node announcement from a node with the given
	// public key. It also waits for the node announcement that advertises
	// a particular set of addresses.
	waitForNodeAnnouncement := func(nodePubKey string, addrs []string) {
		err := wait.NoError(func() error {
			// Expect to have at least 1 node announcement now.
			updates := ht.AssertNumNodeAnns(charlie, nodePubKey, 1)

			// Get latest node update from the node.
			update := updates[len(updates)-1]

			addrMap := make(map[string]bool)
			for _, addr := range update.NodeAddresses {
				addrMap[addr.GetAddr()] = true
			}

			// Check that our wanted addresses can be found from
			// the node update.
			for _, addr := range addrs {
				if !addrMap[addr] {
					return fmt.Errorf("address %s not "+
						"found", addr)
				}
			}

			return nil
		}, defaultTimeout)
		require.NoError(ht, err, "timeout checking node ann")
	}

	// Wait for Charlie to receive Dave's initial NodeAnnouncement.
	waitForNodeAnnouncement(dave.PubKeyStr, advertisedAddrs)

	// Now create a persistent connection between Charlie and Dave with no
	// channels. Charlie is the outbound node and Dave is the inbound node.
	ht.ConnectNodesPerm(charlie, dave)

	// Change Dave's P2P port to the second IP address that he advertised
	// and restart his node.
	dave.Cfg.P2PPort = ip2
	ht.RestartNode(dave)

	// assert that Dave and Charlie reconnect successfully after Dave
	// changes to his second advertised address.
	ht.AssertConnected(dave, charlie)

	// Next we test the case where Dave changes his listening address to one
	// that was not listed in his original advertised addresses. The desired
	// behaviour is that Charlie will update his connection requests to Dave
	// when he receives the Node Announcement from Dave with his updated
	// address.

	// Change Dave's listening port and restart.
	dave.Cfg.P2PPort = node.NextAvailablePort()
	dave.Cfg.ExtraArgs = []string{
		fmt.Sprintf(
			"--externalip=127.0.0.1:%d", dave.Cfg.P2PPort,
		),
	}
	ht.RestartNode(dave)

	// Show that Charlie does receive Dave's new listening address in
	// a Node Announcement.
	waitForNodeAnnouncement(
		dave.PubKeyStr,
		[]string{fmt.Sprintf("127.0.0.1:%d", dave.Cfg.P2PPort)},
	)

	// assert that Dave and Charlie do reconnect after Dave changes his P2P
	// address to one not listed in Dave's original advertised list of
	// addresses.
	ht.AssertConnected(dave, charlie)

	// Finally, close the channel.
	ht.CloseChannel(alice, chanPoint)
}

// testAddPeerConfig tests that the "--addpeer" config flag successfully adds
// a new peer.
func testAddPeerConfig(ht *lntest.HarnessTest) {
	alice := ht.Alice
	info := alice.RPC.GetInfo()

	alicePeerAddress := info.Uris[0]

	// Create a new node (Carol) with Alice as a peer.
	args := []string{fmt.Sprintf("--addpeer=%v", alicePeerAddress)}
	carol := ht.NewNode("Carol", args)

	// TODO(yy): remove this once the peer conn race is fixed.
	time.Sleep(1 * time.Second)

	ht.EnsureConnected(alice, carol)

	// If we list Carol's peers, Alice should already be
	// listed as one, since we specified her using the
	// addpeer flag.
	listPeersResp := carol.RPC.ListPeers()

	parsedPeerAddr, err := lncfg.ParseLNAddressString(
		alicePeerAddress, "9735", net.ResolveTCPAddr,
	)
	require.NoError(ht, err)

	parsedKeyStr := fmt.Sprintf(
		"%x", parsedPeerAddr.IdentityKey.SerializeCompressed(),
	)

	require.Equal(ht, parsedKeyStr, listPeersResp.Peers[0].PubKey)
}
