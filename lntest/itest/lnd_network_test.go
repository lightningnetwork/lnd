package itest

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testNetworkConnectionTimeout checks that the connectiontimeout is taking
// effect. It creates a node with a small connection timeout value, and connects
// it to a non-routable IP address.
func testNetworkConnectionTimeout(net *lntest.NetworkHarness, t *harnessTest) {
	var (
		ctxt, _ = context.WithTimeout(
			context.Background(), defaultTimeout,
		)
		// testPub is a random public key for testing only.
		testPub = "0332bda7da70fefe4b6ab92f53b3c4f4ee7999" +
			"f312284a8e89c8670bb3f67dbee2"
		// testHost is a non-routable IP address. It's used to cause a
		// connection timeout.
		testHost = "10.255.255.255"
	)

	// First, test the global timeout settings.
	// Create Carol with a connection timeout of 1 millisecond.
	carol := net.NewNode(t.t, "Carol", []string{"--connectiontimeout=1ms"})
	defer shutdownAndAssert(net, t, carol)

	// Try to connect Carol to a non-routable IP address, which should give
	// us a timeout error.
	req := &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: testPub,
			Host:   testHost,
		},
	}
	assertTimeoutError(ctxt, t, carol, req)

	// Second, test timeout on the connect peer request.
	// Create Dave with the default timeout setting.
	dave := net.NewNode(t.t, "Dave", nil)
	defer shutdownAndAssert(net, t, dave)

	// Try to connect Dave to a non-routable IP address, using a timeout
	// value of 1ms, which should give us a timeout error immediately.
	req = &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: testPub,
			Host:   testHost,
		},
		Timeout: 1,
	}
	assertTimeoutError(ctxt, t, dave, req)
}

// testReconnectAfterIPChange verifies that if a persistent inbound node changes
// its listening address then it's peer will still be able to reconnect to it.
func testReconnectAfterIPChange(net *lntest.NetworkHarness, t *harnessTest) {
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
	charlie := net.NewNode(t.t, "Charlie", nil)
	defer shutdownAndAssert(net, t, charlie)

	// We derive two ports for Dave, and we initialise his node with
	// these ports advertised as `--externalip` arguments.
	ip1 := lntest.NextAvailablePort()
	ip2 := lntest.NextAvailablePort()

	advertisedAddrs := []string{
		fmt.Sprintf("127.0.0.1:%d", ip1),
		fmt.Sprintf("127.0.0.1:%d", ip2),
	}

	var daveArgs []string
	for _, addr := range advertisedAddrs {
		daveArgs = append(daveArgs, "--externalip="+addr)
	}

	// withP2PPort is a helper closure used to set the P2P port that a node
	// should use.
	var withP2PPort = func(port int) lntest.NodeOption {
		return func(cfg *lntest.BaseNodeConfig) {
			cfg.P2PPort = port
		}
	}

	// Create a new node, Dave, and ensure that his initial P2P port is
	// ip1 derived above.
	dave := net.NewNode(t.t, "Dave", daveArgs, withP2PPort(ip1))
	defer shutdownAndAssert(net, t, dave)

	// Subscribe to graph notifications from Charlie so that we can tell
	// when he receives Dave's NodeAnnouncements.
	ctxb := context.Background()
	charlieSub := subscribeGraphNotifications(ctxb, t, charlie)
	defer close(charlieSub.quit)

	// Connect Alice to Dave and Charlie.
	net.ConnectNodes(t.t, net.Alice, dave)
	net.ConnectNodes(t.t, net.Alice, charlie)

	// We'll then go ahead and open a channel between Alice and Dave. This
	// ensures that Charlie receives the node announcement from Alice as
	// part of the announcement broadcast.
	chanPoint := openChannelAndAssert(
		t, net, net.Alice, dave, lntest.OpenChannelParams{
			Amt: 1000000,
		},
	)
	defer closeChannelAndAssert(t, net, net.Alice, chanPoint, false)

	// waitForNodeAnnouncement is a closure used to wait on the given graph
	// subscription for a node announcement from a node with the given
	// public key. It also waits for the node announcement that advertises
	// a particular set of addresses.
	waitForNodeAnnouncement := func(graphSub graphSubscription,
		nodePubKey string, addrs []string) {

		for {
			select {
			case graphUpdate := <-graphSub.updateChan:
			nextUpdate:
				for _, update := range graphUpdate.NodeUpdates {
					if update.IdentityKey != nodePubKey {
						continue
					}

					addrMap := make(map[string]bool)
					for _, addr := range update.NodeAddresses {
						addrMap[addr.GetAddr()] = true
					}

					for _, addr := range addrs {
						if !addrMap[addr] {
							continue nextUpdate
						}
					}

					return
				}

			case err := <-graphSub.errChan:
				t.Fatalf("unable to recv graph update: %v", err)

			case <-time.After(defaultTimeout):
				t.Fatalf("did not receive node ann update")
			}
		}
	}

	// Wait for Charlie to receive Dave's initial NodeAnnouncement.
	waitForNodeAnnouncement(charlieSub, dave.PubKeyStr, advertisedAddrs)

	// Now create a persistent connection between Charlie and Bob with no
	// channels. Charlie is the outbound node and Bob is the inbound node.
	net.ConnectNodesPerm(t.t, charlie, dave)

	// Assert that Dave and Charlie are connected
	assertConnected(t, dave, charlie)

	// Change Dave's P2P port to the second IP address that he advertised
	// and restart his node.
	dave.Cfg.P2PPort = ip2
	err := net.RestartNode(dave, nil)
	require.NoError(t.t, err)

	// assert that Dave and Charlie reconnect successfully after Dave
	// changes to his second advertised address.
	assertConnected(t, dave, charlie)

	// Next we test the case where Dave changes his listening address to one
	// that was not listed in his original advertised addresses. The desired
	// behaviour is that Charlie will update his connection requests to Dave
	// when he receives the Node Announcement from Dave with his updated
	// address.

	// Change Dave's listening port and restart.
	dave.Cfg.P2PPort = lntest.NextAvailablePort()
	dave.Cfg.ExtraArgs = []string{
		fmt.Sprintf(
			"--externalip=127.0.0.1:%d", dave.Cfg.P2PPort,
		),
	}
	err = net.RestartNode(dave, nil)
	require.NoError(t.t, err)

	// Show that Charlie does receive Dave's new listening address in
	// a Node Announcement.
	waitForNodeAnnouncement(
		charlieSub, dave.PubKeyStr,
		[]string{fmt.Sprintf("127.0.0.1:%d", dave.Cfg.P2PPort)},
	)

	// assert that Dave and Charlie do reconnect after Dave changes his P2P
	// address to one not listed in Dave's original advertised list of
	// addresses.
	assertConnected(t, dave, charlie)
}

// assertTimeoutError asserts that a connection timeout error is raised. A
// context with a default timeout is used to make the request. If our customized
// connection timeout is less than the default, we won't see the request context
// times out, instead a network connection timeout will be returned.
func assertTimeoutError(ctxt context.Context, t *harnessTest,
	node *lntest.HarnessNode, req *lnrpc.ConnectPeerRequest) {

	t.t.Helper()

	err := connect(ctxt, node, req)

	// a DeadlineExceeded error will appear in the context if the above
	// ctxtTimeout value is reached.
	require.NoError(t.t, ctxt.Err(), "context time out")

	// Check that the network returns a timeout error.
	require.Containsf(
		t.t, err.Error(), "i/o timeout",
		"expected to get a timeout error, instead got: %v", err,
	)
}

func connect(ctxt context.Context, node *lntest.HarnessNode,
	req *lnrpc.ConnectPeerRequest) error {

	syncTimeout := time.After(15 * time.Second)
	ticker := time.NewTicker(time.Millisecond * 100)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			_, err := node.ConnectPeer(ctxt, req)
			// If there's no error, return nil
			if err == nil {
				return err
			}
			// If the error is no ErrServerNotActive, return it.
			// Otherwise, we will retry until timeout.
			if !strings.Contains(err.Error(),
				lnd.ErrServerNotActive.Error()) {

				return err
			}
		case <-syncTimeout:
			return fmt.Errorf("chain backend did not " +
				"finish syncing")
		}
	}
	return nil
}
