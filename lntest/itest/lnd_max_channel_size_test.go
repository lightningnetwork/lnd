// +build rpctest

package itest

import (
	"context"
	"fmt"
	"strings"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lntest"
)

// testMaxChannelSize tests that lnd handles --maxchansize parameter
// correctly. Wumbo nodes should enforce a default soft limit of 10 BTC by
// default. This limit can be adjusted with --maxchansize config option
func testMaxChannelSize(net *lntest.NetworkHarness, t *harnessTest) {
	// We'll make two new nodes, both wumbo but with the default
	// limit on maximum channel size (10 BTC)
	wumboNode, err := net.NewNode(
		"wumbo", []string{"--protocol.wumbo-channels"},
	)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	defer shutdownAndAssert(net, t, wumboNode)

	wumboNode2, err := net.NewNode(
		"wumbo2", []string{"--protocol.wumbo-channels"},
	)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	defer shutdownAndAssert(net, t, wumboNode2)

	// We'll send 11 BTC to the wumbo node so it can test the wumbo soft limit.
	ctxb := context.Background()
	err = net.SendCoins(ctxb, 11*btcutil.SatoshiPerBitcoin, wumboNode)
	if err != nil {
		t.Fatalf("unable to send coins to wumbo node: %v", err)
	}

	// Next we'll connect both nodes, then attempt to make a wumbo channel
	// funding request, which should fail as it exceeds the default wumbo
	// soft limit of 10 BTC.
	err = net.EnsureConnected(ctxb, wumboNode, wumboNode2)
	if err != nil {
		t.Fatalf("unable to connect peers: %v", err)
	}

	chanAmt := funding.MaxBtcFundingAmountWumbo + 1
	_, err = net.OpenChannel(
		ctxb, wumboNode, wumboNode2, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	if err == nil {
		t.Fatalf("expected channel funding to fail as it exceeds 10 BTC limit")
	}

	// The test should show failure due to the channel exceeding our max size.
	if !strings.Contains(err.Error(), "exceeds maximum chan size") {
		t.Fatalf("channel should be rejected due to size, instead "+
			"error was: %v", err)
	}

	// Next we'll create a non-wumbo node to verify that it enforces the
	// BOLT-02 channel size limit and rejects our funding request.
	miniNode, err := net.NewNode("mini", nil)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	defer shutdownAndAssert(net, t, miniNode)

	err = net.EnsureConnected(ctxb, wumboNode, miniNode)
	if err != nil {
		t.Fatalf("unable to connect peers: %v", err)
	}

	_, err = net.OpenChannel(
		ctxb, wumboNode, miniNode, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	if err == nil {
		t.Fatalf("expected channel funding to fail as it exceeds 0.16 BTC limit")
	}

	// The test should show failure due to the channel exceeding our max size.
	if !strings.Contains(err.Error(), "exceeds maximum chan size") {
		t.Fatalf("channel should be rejected due to size, instead "+
			"error was: %v", err)
	}

	// We'll now make another wumbo node with appropriate maximum channel size
	// to accept our wumbo channel funding.
	wumboNode3, err := net.NewNode(
		"wumbo3", []string{"--protocol.wumbo-channels",
			fmt.Sprintf("--maxchansize=%v", int64(funding.MaxBtcFundingAmountWumbo+1))},
	)
	if err != nil {
		t.Fatalf("unable to create new node: %v", err)
	}
	defer shutdownAndAssert(net, t, wumboNode3)

	// Creating a wumbo channel between these two nodes should succeed.
	err = net.EnsureConnected(ctxb, wumboNode, wumboNode3)
	if err != nil {
		t.Fatalf("unable to connect peers: %v", err)
	}
	chanPoint := openChannelAndAssert(
		ctxb, t, net, wumboNode, wumboNode3,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	closeChannelAndAssert(ctxb, t, net, wumboNode, chanPoint, false)

}
