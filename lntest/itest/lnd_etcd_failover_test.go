//go:build kvdb_etcd
// +build kvdb_etcd

package itest

import (
	"io/ioutil"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/cluster"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
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

		ht.Run(test.name, func(t1 *testing.T) {
			st := ht.Subtest(t1)
			st.RunTestCase(&lntest.TestCase{
				Name: test.name,
				TestFunc: func(ht *lntest.HarnessTest) {
					testEtcdFailoverCase(ht, test.kill)
				},
			})
		})
	}
}

func testEtcdFailoverCase(ht *lntest.HarnessTest, kill bool) {
	tmpDir, err := ioutil.TempDir("", "etcd")
	if err != nil {
		ht.Fatalf("Failed to create temp dir: %v", err)
	}
	etcdCfg, cleanup, err := kvdb.StartEtcdTestBackend(
		tmpDir, uint16(lntest.NextAvailablePort()),
		uint16(lntest.NextAvailablePort()), "",
	)
	require.NoError(ht, err, "Failed to start etcd instance")
	defer cleanup()

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
	info1 := ht.GetInfo(carol1)

	alice := ht.NewNode("Alice", nil)
	ht.ConnectNodes(carol1, alice)

	// Give Alice some coins to fund the channel.
	ht.SendCoins(btcutil.SatoshiPerBitcoin, alice)

	// Open a channel with 100k satoshis between Carol and Alice with Alice
	// being the sole funder of the channel.
	chanAmt := btcutil.Amount(100000)
	ht.OpenChannel(alice, carol1, lntest.OpenChannelParams{Amt: chanAmt})

	// At this point Carol-1 is the elected leader, while Carol-2 will wait
	// to become the leader when Carol-1 stops.
	carol2 := ht.NewNodeEtcd(
		"Carol-2", etcdCfg, password, cluster, false, leaderSessionTTL,
	)

	assertLeader(ht, observer, "Carol-1")

	amt := btcutil.Amount(1000)
	payReqs, _, _ := ht.CreatePayReqs(carol1, amt, 2)

	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: payReqs[0],
		TimeoutSeconds: 60,
		FeeLimitSat:    noFeeLimitMsat,
	}
	ht.SendPaymentAndAssert(alice, req)

	// Shut down or kill Carol-1 and wait for Carol-2 to become the leader.
	failoverTimeout := time.Duration(2*leaderSessionTTL) * time.Second
	if kill {
		ht.KillNode(carol1)
	} else {
		ht.Shutdown(carol1)
	}

	err = carol2.WaitUntilLeader(failoverTimeout)
	require.NoError(ht, err,
		"Waiting for Carol-2 to become the leader failed")

	assertLeader(ht, observer, "Carol-2")

	err = carol2.Unlock(&lnrpc.UnlockWalletRequest{
		WalletPassword: password,
	})
	require.NoError(ht, err, "Unlocking Carol-2 failed")

	// Make sure Carol-1 and Carol-2 have the same identity.
	info2 := ht.GetInfo(carol2)
	require.Equal(ht, info1.IdentityPubkey, info2.IdentityPubkey,
		"Carol-1 and Carol-2 must have the same identity")

	// Now let Alice pay the second invoice but this time we expect Carol-2
	// to receive the payment.
	req = &routerrpc.SendPaymentRequest{
		PaymentRequest: payReqs[1],
		TimeoutSeconds: 60,
		FeeLimitSat:    noFeeLimitMsat,
	}
	ht.SendPaymentAndAssert(alice, req)

	ht.Shutdown(carol2)
}
