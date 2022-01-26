//go:build kvdb_etcd
// +build kvdb_etcd

package itest

import (
	"context"
	"io/ioutil"
	"testing"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/cluster"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
)

func assertLeader(ht *harnessTest, observer cluster.LeaderElector,
	expected string) {

	leader, err := observer.Leader(context.Background())
	if err != nil {
		ht.Fatalf("Unable to query leader: %v", err)
	}

	if leader != expected {
		ht.Fatalf("Leader should be '%v', got: '%v'", expected, leader)
	}
}

// testEtcdFailover tests that in a cluster setup where two LND nodes form a
// single cluster (sharing the same identity) one can hand over the leader role
// to the other (failing over after graceful shutdown or forceful abort).
func testEtcdFailover(net *lntest.NetworkHarness, ht *harnessTest) {
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

		ht.t.Run(test.name, func(t1 *testing.T) {
			ht1 := newHarnessTest(t1, ht.lndHarness)
			ht1.RunTestCase(&testCase{
				name: test.name,
				test: func(_ *lntest.NetworkHarness,
					tt *harnessTest) {

					testEtcdFailoverCase(net, tt, test.kill)
				},
			})
		})
	}
}

func testEtcdFailoverCase(net *lntest.NetworkHarness, ht *harnessTest,
	kill bool) {

	ctxb := context.Background()

	tmpDir, err := ioutil.TempDir("", "etcd")
	etcdCfg, cleanup, err := kvdb.StartEtcdTestBackend(
		tmpDir, uint16(lntest.NextAvailablePort()),
		uint16(lntest.NextAvailablePort()), "",
	)
	if err != nil {
		ht.Fatalf("Failed to start etcd instance: %v", err)
	}
	defer cleanup()

	observer, err := cluster.MakeLeaderElector(
		ctxb, cluster.EtcdLeaderElector, "observer",
		lncfg.DefaultEtcdElectionPrefix, etcdCfg,
	)
	if err != nil {
		ht.Fatalf("Cannot start election observer")
	}

	password := []byte("the quick brown fox jumps the lazy dog")
	entropy := [16]byte{1, 2, 3}
	stateless := false
	cluster := true

	carol1, _, _, err := net.NewNodeWithSeedEtcd(
		"Carol-1", etcdCfg, password, entropy[:], stateless, cluster,
	)
	if err != nil {
		ht.Fatalf("unable to start Carol-1: %v", err)
	}

	ctxt, _ := context.WithTimeout(ctxb, defaultTimeout)
	info1, err := carol1.GetInfo(ctxt, &lnrpc.GetInfoRequest{})

	net.ConnectNodes(ht.t, carol1, net.Alice)

	// Open a channel with 100k satoshis between Carol and Alice with Alice
	// being the sole funder of the channel.
	chanAmt := btcutil.Amount(100000)
	_ = openChannelAndAssert(
		ht, net, net.Alice, carol1,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// At this point Carol-1 is the elected leader, while Carol-2 will wait
	// to become the leader when Carol-1 stops.
	carol2, err := net.NewNodeEtcd(
		"Carol-2", etcdCfg, password, cluster, false,
	)
	if err != nil {
		ht.Fatalf("Unable to start Carol-2: %v", err)
	}

	assertLeader(ht, observer, "Carol-1")

	amt := btcutil.Amount(1000)
	payReqs, _, _, err := createPayReqs(carol1, amt, 2)
	if err != nil {
		ht.Fatalf("Carol-2 is unable to create payment requests: %v",
			err)
	}
	sendAndAssertSuccess(
		ht, net.Alice, &routerrpc.SendPaymentRequest{
			PaymentRequest: payReqs[0],
			TimeoutSeconds: 60,
			FeeLimitSat:    noFeeLimitMsat,
		},
	)

	// Shut down or kill Carol-1 and wait for Carol-2 to become the leader.
	var failoverTimeout time.Duration
	if kill {
		err = net.KillNode(carol1)
		if err != nil {
			ht.Fatalf("Can't kill Carol-1: %v", err)
		}

		failoverTimeout = 2 * time.Minute

	} else {
		shutdownAndAssert(net, ht, carol1)
		failoverTimeout = 30 * time.Second
	}

	err = carol2.WaitUntilLeader(failoverTimeout)
	if err != nil {
		ht.Fatalf("Waiting for Carol-2 to become the leader failed: %v",
			err)
	}

	assertLeader(ht, observer, "Carol-2")

	err = carol2.Unlock(&lnrpc.UnlockWalletRequest{
		WalletPassword: password,
	})
	if err != nil {
		ht.Fatalf("Unlocking Carol-2 was not successful: %v", err)
	}

	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)

	// Make sure Carol-1 and Carol-2 have the same identity.
	info2, err := carol2.GetInfo(ctxt, &lnrpc.GetInfoRequest{})
	if info1.IdentityPubkey != info2.IdentityPubkey {
		ht.Fatalf("Carol-1 and Carol-2 must have the same identity: "+
			"%v vs %v", info1.IdentityPubkey, info2.IdentityPubkey)
	}

	// Now let Alice pay the second invoice but this time we expect Carol-2
	// to receive the payment.
	sendAndAssertSuccess(
		ht, net.Alice, &routerrpc.SendPaymentRequest{
			PaymentRequest: payReqs[1],
			TimeoutSeconds: 60,
			FeeLimitSat:    noFeeLimitMsat,
		},
	)

	shutdownAndAssert(net, ht, carol2)
}
