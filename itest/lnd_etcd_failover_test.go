//go:build kvdb_etcd
// +build kvdb_etcd

package itest

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/cluster"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
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
		ht.T.TempDir(), uint16(node.NextAvailablePort()),
		uint16(node.NextAvailablePort()), "",
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
	entropy := [16]byte{1, 2, 3}
	stateless := false
	cluster := true

	carol1, _, _ := ht.NewNodeWithSeedEtcd(
		"Carol-1", etcdCfg, password, entropy[:], stateless, cluster,
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
