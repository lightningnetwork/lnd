//go:build kvdb_etcd
// +build kvdb_etcd

package itest

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/cluster"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/port"
	"github.com/stretchr/testify/require"
)

func assertLeader(ht *lntest.HarnessTest, observer cluster.LeaderElector,
	expected string) {

	leader, err := observer.Leader(ht.Context())
	require.NoError(ht, err, "Unable to query leader")
	require.Equalf(ht, expected, leader,
		"Leader should be '%v', got: '%v'", expected, leader)
}

// testEtcdFailover tests that in a cluster setup where two LND nodes form a
// single cluster (sharing the same identity) one can hand over the leader role
// to the other (failing over after graceful shutdown or forceful abort).
func testEtcdFailover(ht *lntest.HarnessTest) {
	testCases := []struct {
		name string
		kill bool
	}{{
		name: "failover after shutdown",
		kill: false,
	}, {
		name: "failover after abort",
		kill: true,
	}}

	for _, test := range testCases {
		test := test

		success := ht.Run(test.name, func(t1 *testing.T) {
			st := ht.Subtest(t1)
			testEtcdFailoverCase(st, test.kill)
		})
		if !success {
			return
		}
	}
}

func testEtcdFailoverCase(ht *lntest.HarnessTest, kill bool) {
	etcdCfg, cleanup, err := kvdb.StartEtcdTestBackend(
		ht.T.TempDir(), uint16(port.NextAvailablePort()),
		uint16(port.NextAvailablePort()), "",
	)
	require.NoError(ht, err, "Failed to start etcd instance")
	defer cleanup()

	alice := ht.NewNode("Alice", nil)

	// Give Alice some coins to fund the channel.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, alice)

	// Make leader election session TTL 5 sec to make the test run fast.
	const leaderSessionTTL = 5

	observer, err := cluster.MakeLeaderElector(
		ht.Context(), cluster.EtcdLeaderElector, "observer",
		lncfg.DefaultEtcdElectionPrefix, leaderSessionTTL, etcdCfg,
	)
	require.NoError(ht, err, "Cannot start election observer")

	password := []byte("the quick brown fox jumps the lazy dog")
	stateless := false
	cluster := true

	carol1, _, _ := ht.NewNodeWithSeedEtcd(
		"Carol-1", etcdCfg, password, stateless, cluster,
		leaderSessionTTL,
	)
	info1 := carol1.RPC.GetInfo()
	ht.ConnectNodes(carol1, alice)

	// Open a channel with 100k satoshis between Carol and Alice with Alice
	// being the sole funder of the channel.
	chanAmt := btcutil.Amount(100_000)
	ht.OpenChannel(alice, carol1, lntest.OpenChannelParams{Amt: chanAmt})

	// At this point Carol-1 is the elected leader, while Carol-2 will wait
	// to become the leader when Carol-1 stops.
	carol2 := ht.NewNodeEtcd(
		"Carol-2", etcdCfg, password, cluster, leaderSessionTTL,
	)
	assertLeader(ht, observer, "Carol-1")

	amt := btcutil.Amount(1000)
	payReqs, _, _ := ht.CreatePayReqs(carol1, amt, 2)
	ht.CompletePaymentRequests(alice, []string{payReqs[0]})

	// Shut down or kill Carol-1 and wait for Carol-2 to become the leader.
	failoverTimeout := time.Duration(2*leaderSessionTTL) * time.Second
	if kill {
		ht.KillNode(carol1)
	} else {
		ht.Shutdown(carol1)
	}

	err = carol2.WaitUntilLeader(failoverTimeout)
	require.NoError(ht, err, "Waiting for Carol-2 to become the leader "+
		"failed")
	assertLeader(ht, observer, "Carol-2")

	req := &lnrpc.UnlockWalletRequest{WalletPassword: password}
	err = carol2.Unlock(req)
	require.NoError(ht, err, "Unlocking Carol-2 failed")

	// Make sure Carol-1 and Carol-2 have the same identity.
	info2 := carol2.RPC.GetInfo()
	require.Equal(ht, info1.IdentityPubkey, info2.IdentityPubkey,
		"Carol-1 and Carol-2 must have the same identity")

	// Make sure the nodes are connected before moving forward. Otherwise
	// we may get a link not found error.
	ht.AssertConnected(alice, carol2)

	// Now let Alice pay the second invoice but this time we expect Carol-2
	// to receive the payment.
	ht.CompletePaymentRequests(alice, []string{payReqs[1]})

	// Manually shutdown the node as it will mess up with our cleanup
	// process.
	ht.Shutdown(carol2)
}

// Proxy is a simple TCP proxy that forwards all traffic between a local and a
// remote address. We use it to simulate a network partition in the leader
// health check test.
type Proxy struct {
	listenAddr string
	targetAddr string
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	stopped    chan struct{}
}

// NewProxy creates a new Proxy instance with a provided context.
func NewProxy(listenAddr, targetAddr string) *Proxy {
	return &Proxy{
		listenAddr: listenAddr,
		targetAddr: targetAddr,
		stopped:    make(chan struct{}),
	}
}

// Start starts the proxy. It listens on the listen address and forwards all
// traffic to the target address.
func (p *Proxy) Start(ctx context.Context, t *testing.T) {
	listener, err := net.Listen("tcp", p.listenAddr)
	require.NoError(t, err, "Failed to listen on %s", p.listenAddr)
	t.Logf("Proxy is listening on %s", p.listenAddr)

	proxyCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel

	p.wg.Add(1)
	go func() {
		defer func() {
			close(p.stopped)
			p.wg.Done()
		}()

		for {
			select {
			case <-proxyCtx.Done():
				listener.Close()
				return
			default:
			}

			conn, err := listener.Accept()
			if err != nil {
				if proxyCtx.Err() != nil {
					// Context is done, exit the loop
					return
				}
				t.Logf("Proxy failed to accept connection: %v",
					err)

				continue
			}

			p.wg.Add(1)
			go p.handleConnection(proxyCtx, t, conn)
		}
	}()
}

// handleConnection handles an accepted connection and forwards all traffic
// between the listener and target.
func (p *Proxy) handleConnection(ctx context.Context, t *testing.T,
	conn net.Conn) {

	targetConn, err := net.Dial("tcp", p.targetAddr)
	require.NoError(t, err, "Failed to connect to target %s", p.targetAddr)

	defer func() {
		conn.Close()
		targetConn.Close()
		p.wg.Done()
	}()

	done := make(chan struct{})

	p.wg.Add(2)
	go func() {
		defer p.wg.Done()
		// Ignore the copy error due to the connection being closed.
		_, _ = io.Copy(targetConn, conn)
	}()

	go func() {
		defer p.wg.Done()
		// Ignore the copy error due to the connection being closed.
		_, _ = io.Copy(conn, targetConn)
		close(done)
	}()

	select {
	case <-ctx.Done():
	case <-done:
	}
}

// Stop stops the proxy and waits for all connections to be closed and all
// goroutines to be stopped.
func (p *Proxy) Stop(t *testing.T) {
	require.NotNil(t, p.cancel, "Proxy is not started")

	p.cancel()
	p.wg.Wait()
	<-p.stopped

	t.Log("Proxy stopped", time.Now())
}

// testLeaderHealthCheck tests that a node is properly shut down when the leader
// health check fails.
func testLeaderHealthCheck(ht *lntest.HarnessTest) {
	clientPort := port.NextAvailablePort()

	// Let's start a test etcd instance that we'll redirect through a proxy.
	etcdCfg, cleanup, err := kvdb.StartEtcdTestBackend(
		ht.T.TempDir(), uint16(clientPort),
		uint16(port.NextAvailablePort()), "",
	)
	require.NoError(ht, err, "Failed to start etcd instance")

	// Make leader election session TTL 5 sec to make the test run fast.
	const leaderSessionTTL = 5

	// Create an election observer that we will use to monitor the leader
	// election.
	observer, err := cluster.MakeLeaderElector(
		ht.Context(), cluster.EtcdLeaderElector, "observer",
		lncfg.DefaultEtcdElectionPrefix, leaderSessionTTL, etcdCfg,
	)
	require.NoError(ht, err, "Cannot start election observer")

	// Start a proxy that will forward all traffic to the etcd instance.
	clientAddr := fmt.Sprintf("localhost:%d", clientPort)
	proxyAddr := fmt.Sprintf("localhost:%d", port.NextAvailablePort())

	ctx, cancel := context.WithCancel(ht.Context())
	defer cancel()

	proxy := NewProxy(proxyAddr, clientAddr)
	proxy.Start(ctx, ht.T)

	// Copy the etcd config so that we can modify the host to point to the
	// proxy.
	proxyEtcdCfg := *etcdCfg
	// With the proxy in place, we can now configure the etcd client to
	// connect to the proxy instead of the etcd instance.
	proxyEtcdCfg.Host = "http://" + proxyAddr

	defer cleanup()

	// Start Carol-1 with cluster support and connect to etcd through the
	// proxy.
	password := []byte("the quick brown fox jumps the lazy dog")
	stateless := false
	cluster := true

	carol, _, _ := ht.NewNodeWithSeedEtcd(
		"Carol-1", &proxyEtcdCfg, password, stateless, cluster,
		leaderSessionTTL,
	)

	// Make sure Carol-1 is indeed the leader.
	assertLeader(ht, observer, "Carol-1")

	// At this point Carol-1 is the elected leader, while Carol-2 will wait
	// to become the leader when Carol-1 releases the lease. Note that for
	// Carol-2 we don't use the proxy as we want to simulate a network
	// partition only for Carol-1.
	carol2 := ht.NewNodeEtcd(
		"Carol-2", etcdCfg, password, cluster, leaderSessionTTL,
	)

	// Stop the proxy so that we simulate a network partition which
	// consequently will make the leader health check fail and force Carol
	// to shut down.
	proxy.Stop(ht.T)

	// Wait for Carol-1 to stop. If the health check wouldn't properly work
	// this call would timeout and trigger a test failure.
	require.NoError(ht.T, carol.WaitForProcessExit())

	// Now that Carol-1 is shut down we should fail over to Carol-2.
	failoverTimeout := time.Duration(2*leaderSessionTTL) * time.Second

	// Make sure that Carol-2 becomes the leader (reported by Carol-2).
	err = carol2.WaitUntilLeader(failoverTimeout)

	require.NoError(ht, err, "Waiting for Carol-2 to become the leader "+
		"failed")

	// Make sure Carol-2 is indeed the leader (repoted by the observer).
	assertLeader(ht, observer, "Carol-2")
}
