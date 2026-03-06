//go:build integration

package itest

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// testRemoteGraph verifies that a node configured with a remote graph source
// can discover channels and nodes via that remote source, maintain its own
// private channels independently, and route payments using the combined graph
// view.
func testRemoteGraph(ht *lntest.HarnessTest) {
	var (
		ctx          = context.Background()
		descGraphReq = &lnrpc.ChannelGraphRequest{
			IncludeUnannounced: true,
		}
	)

	// Set up a network:
	// Alice <- Bob <- Carol
	_, nodes := ht.CreateSimpleNetwork(
		[][]string{nil, nil, nil}, lntest.OpenChannelParams{
			Amt: btcutil.Amount(100000),
		},
	)
	carol, bob, alice := nodes[0], nodes[1], nodes[2]

	// assertDescGraph is a helper that asserts a node's graph contains the
	// expected number of edges and the expected set of nodes.
	assertDescGraph := func(node *node.HarnessNode, numEdges int,
		nodes ...*node.HarnessNode) {

		err := wait.NoError(func() error {
			resp := node.RPC.DescribeGraph(descGraphReq)
			if len(resp.Edges) != numEdges {
				return fmt.Errorf("expected %d edges, got "+
					"%d", numEdges, len(resp.Edges))
			}

			knownNodes := map[string]bool{
				// A node always knows about itself.
				node.PubKeyStr: true,
			}
			for _, n := range resp.Nodes {
				knownNodes[n.PubKey] = true
			}

			expectedCount := len(nodes) + 1
			if len(knownNodes) != expectedCount {
				return fmt.Errorf("expected %d nodes, "+
					"got %d", expectedCount,
					len(knownNodes))
			}

			for _, n := range nodes {
				if !knownNodes[n.PubKeyStr] {
					return fmt.Errorf("node %s not "+
						"found in graph",
						n.PubKeyStr)
				}
			}

			return nil
		}, wait.DefaultTimeout)
		require.NoError(ht.T, err)
	}

	// Alice should know about Alice, Bob and Carol along with the 2 public
	// channels.
	assertDescGraph(alice, 2, bob, carol)

	// Create graph provider node, Greg. Don't connect it to any nodes yet.
	greg := ht.NewNode("Greg", nil)

	// Greg should just know about himself. No channels yet.
	assertDescGraph(greg, 0)

	// Create Zane, a node that uses Greg as its remote graph source.
	// Gossip sync is disabled so Zane relies entirely on Greg for network
	// topology.
	zane := ht.NewNode("Zane", []string{
		"--gossip.no-sync",
		"--remotegraph.enable",
		"--caches.rpc-graph-cache-duration=0",
		fmt.Sprintf(
			"--remotegraph.rpchost=localhost:%d",
			greg.Cfg.RPCPort,
		),
		fmt.Sprintf(
			"--remotegraph.tlscertpath=%s",
			greg.Cfg.TLSCertPath,
		),
		fmt.Sprintf(
			"--remotegraph.macaroonpath=%s",
			greg.Cfg.AdminMacPath,
		),
	})

	// Zane should know about itself and Greg (from the remote graph) but
	// no channels yet.
	assertDescGraph(zane, 0, greg)

	// Connect Zane to Carol. Even though they're now peers, Zane should
	// not sync gossip from Carol (--gossip.no-sync), so its graph view
	// should not change.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, zane)
	ht.EnsureConnected(zane, carol)
	assertDescGraph(zane, 0, greg)

	// Open a private channel between Zane and Carol. This channel is only
	// known locally to Zane and Carol and should not appear in Greg's
	// graph.
	chanPointZane := ht.OpenChannel(
		zane, carol, lntest.OpenChannelParams{
			Private: true,
			Amt:     btcutil.Amount(100000),
		},
	)

	// Zane should now know about itself, Greg, and Carol along with the
	// one private channel.
	assertDescGraph(zane, 1, greg, carol)

	// Connect Greg to Bob so Greg can sync the public network topology.
	ht.EnsureConnected(greg, bob)

	// Wait until Greg has discovered Carol (meaning gossip sync completed).
	err := wait.NoError(func() error {
		info, err := greg.RPC.LN.GetNodeInfo(
			ctx, &lnrpc.NodeInfoRequest{
				PubKey: carol.PubKeyStr,
			},
		)
		if err != nil {
			return err
		}

		if len(info.Node.Addresses) == 0 {
			return fmt.Errorf("greg has not yet synced carol's " +
				"node announcement")
		}

		return nil
	}, wait.DefaultTimeout)
	require.NoError(ht.T, err)

	// Greg should know about the 2 public channels and all public nodes.
	// It does NOT know about Zane since Zane's only channel is private.
	assertDescGraph(greg, 2, alice, bob, carol)

	// Zane uses Greg as its remote graph source, so it should see
	// everything Greg sees PLUS its own private channel with Carol.
	assertDescGraph(zane, 3, alice, bob, carol, greg)

	// Verify Zane can route a payment through the remote graph. Alice
	// creates an invoice, and Zane pays it via Carol -> Bob -> Alice.
	invoice := alice.RPC.AddInvoice(&lnrpc.Invoice{Value: 100})
	ht.CompletePaymentRequests(zane, []string{invoice.PaymentRequest})

	// Clean up: close Zane's private channel and mine blocks so both
	// sides clear the link node.
	ht.CloseChannel(zane, chanPointZane)
	ht.MineBlocks(6)

	// Disconnect Zane from Carol so Carol won't auto-reconnect.
	ht.DisconnectNodes(carol, zane)

	// Restart Zane (bootstrap disabled by default in test harness). It
	// should have no peers since it has no channels and no bootstrap.
	ht.RestartNode(zane)
	err = wait.Invariant(func() bool {
		peerResp := zane.RPC.ListPeers()

		return len(peerResp.Peers) == 0
	}, time.Second*5)
	require.NoError(ht.T, err)

	// Now enable peer bootstrapping and restart. Zane should discover
	// peers from the remote graph and connect to them.
	zane.Cfg.WithPeerBootstrap = true
	ht.RestartNode(zane)

	err = wait.NoError(func() error {
		peerResp := zane.RPC.ListPeers()
		if len(peerResp.Peers) == 0 {
			return fmt.Errorf("zane has no peers yet")
		}

		return nil
	}, wait.DefaultTimeout)
	require.NoError(ht.T, err)
}
