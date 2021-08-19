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
	wumboNode := net.NewNode(
		t.t, "wumbo", []string{"--protocol.wumbo-channels"},
	)
	defer shutdownAndAssert(net, t, wumboNode)

	wumboNode2 := net.NewNode(
		t.t, "wumbo2", []string{"--protocol.wumbo-channels"},
	)
	defer shutdownAndAssert(net, t, wumboNode2)

	// We'll send 11 BTC to the wumbo node so it can test the wumbo soft limit.
	ctxb := context.Background()
	net.SendCoins(t.t, 11*btcutil.SatoshiPerBitcoin, wumboNode)

	// Next we'll connect both nodes, then attempt to make a wumbo channel
	// funding request, which should fail as it exceeds the default wumbo
	// soft limit of 10 BTC.
	net.EnsureConnected(t.t, wumboNode, wumboNode2)

	chanAmt := funding.MaxBtcFundingAmountWumbo + 1
	_, err := net.OpenChannel(
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
	miniNode := net.NewNode(t.t, "mini", nil)
	defer shutdownAndAssert(net, t, miniNode)

	net.EnsureConnected(t.t, wumboNode, miniNode)

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
	wumboNode3 := net.NewNode(
		t.t, "wumbo3", []string{
			"--protocol.wumbo-channels",
			fmt.Sprintf(
				"--maxchansize=%v",
				int64(funding.MaxBtcFundingAmountWumbo+1),
			),
		},
	)
	defer shutdownAndAssert(net, t, wumboNode3)

	// Creating a wumbo channel between these two nodes should succeed.
	net.EnsureConnected(t.t, wumboNode, wumboNode3)
	chanPoint := openChannelAndAssert(
		t, net, wumboNode, wumboNode3,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	closeChannelAndAssert(t, net, wumboNode, chanPoint, false)

}
