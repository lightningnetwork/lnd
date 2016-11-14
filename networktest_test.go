package main

import (
	"context"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/roasbeef/btcd/rpctest"
	"github.com/roasbeef/btcrpcclient"
)

// TODO(roasbeef): randomize ports so can start multiple instance with each
// other, will need to catch up to rpctest upstream.

func TestNodeRestart(t *testing.T) {
	// Create a new instance of the network harness in order to initialize the
	// necessary state. We'll also need to create a temporary instance of
	// rpctest due to initialization dependancies.
	lndHarness, err := newNetworkHarness()
	if err != nil {
		t.Fatalf("unable to create lightning network harness: %v", err)
	}
	defer lndHarness.TearDownAll()

	handlers := &btcrpcclient.NotificationHandlers{
		OnTxAccepted: lndHarness.OnTxAccepted,
	}

	btcdHarness, err := rpctest.New(harnessNetParams, handlers, nil)
	if err != nil {
		t.Fatalf("unable to create mining node: %v", err)
	}
	defer btcdHarness.TearDown()
	if err := btcdHarness.SetUp(true, 50); err != nil {
		t.Fatalf("unable to set up mining node: %v", err)
	}
	if err := btcdHarness.Node.NotifyNewTransactions(false); err != nil {
		t.Fatalf("unable to request transaction notifications: %v", err)
	}

	if err := lndHarness.InitializeSeedNodes(btcdHarness, nil); err != nil {
		t.Fatalf("unable to initialize seed nodes: %v", err)
	}
	if err = lndHarness.SetUp(); err != nil {
		t.Fatalf("unable to set up test lightning network: %v", err)
	}

	// With the harness set up, we can test the node restart method,
	// asserting all data is properly persisted and recovered.
	ctx := context.Background()

	// First, we'll test restarting one of the initial seed nodes: Alice.
	alice := lndHarness.Alice
	aliceInfo, err := alice.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		t.Fatalf("unable to query for alice's info: %v", err)
	}

	// With alice's node information stored, attempt to restart the node.
	if lndHarness.RestartNode(alice); err != nil {
		t.Fatalf("unable to resart node: %v", err)
	}

	// Query for alice's current information, it should be identical to
	// what we received above.
	aliceInfoRestart, err := alice.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		t.Fatalf("unable to query for alice's info: %v", err)
	}

	if aliceInfo.IdentityPubkey != aliceInfoRestart.IdentityPubkey {
		t.Fatalf("node info after restart doesn't match: %v vs %v",
			aliceInfo.IdentityPubkey, aliceInfoRestart.IdentityPubkey)
	}
}
