package itest

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/wtclientrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// testWatchtowerSessionManagement tests that session deletion is done
// correctly.
func testWatchtowerSessionManagement(ht *lntest.HarnessTest) {
	const (
		chanAmt           = funding.MaxBtcFundingAmount
		paymentAmt        = 10_000
		numInvoices       = 5
		maxUpdates        = numInvoices * 2
		externalIP        = "1.2.3.4"
		sessionCloseRange = 1
	)

	// Set up Wallis the watchtower who will be used by Dave to watch over
	// his channel commitment transactions.
	wallis := ht.NewNode("Wallis", []string{
		"--watchtower.active",
		"--watchtower.externalip=" + externalIP,
	})

	wallisInfo := wallis.RPC.GetInfoWatchtower()

	// Assert that Wallis has one listener and it is 0.0.0.0:9911 or
	// [::]:9911. Since no listener is explicitly specified, one of these
	// should be the default depending on whether the host supports IPv6 or
	// not.
	require.Len(ht, wallisInfo.Listeners, 1)
	listener := wallisInfo.Listeners[0]
	require.True(ht, listener == "0.0.0.0:9911" || listener == "[::]:9911")

	// Assert the Wallis's URIs properly display the chosen external IP.
	require.Len(ht, wallisInfo.Uris, 1)
	require.Contains(ht, wallisInfo.Uris[0], externalIP)

	// Dave will be the tower client.
	daveArgs := []string{
		"--wtclient.active",
		fmt.Sprintf("--wtclient.max-updates=%d", maxUpdates),
		fmt.Sprintf(
			"--wtclient.session-close-range=%d", sessionCloseRange,
		),
	}
	dave := ht.NewNode("Dave", daveArgs)

	addTowerReq := &wtclientrpc.AddTowerRequest{
		Pubkey:  wallisInfo.Pubkey,
		Address: listener,
	}
	dave.RPC.AddTower(addTowerReq)

	// Assert that there exists a session between Dave and Wallis.
	err := wait.NoError(func() error {
		info := dave.RPC.GetTowerInfo(&wtclientrpc.GetTowerInfoRequest{
			Pubkey:          wallisInfo.Pubkey,
			IncludeSessions: true,
		})

		var numSessions uint32
		for _, sessionType := range info.SessionInfo {
			numSessions += sessionType.NumSessions
		}
		if numSessions > 0 {
			return nil
		}

		return fmt.Errorf("expected a non-zero number of sessions")
	}, defaultTimeout)
	require.NoError(ht, err)

	// Before we make a channel, we'll load up Dave with some coins sent
	// directly from the miner.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, dave)

	// Connect Dave and Alice.
	ht.ConnectNodes(dave, ht.Alice)

	// Open a channel between Dave and Alice.
	params := lntest.OpenChannelParams{
		Amt: chanAmt,
	}
	chanPoint := ht.OpenChannel(dave, ht.Alice, params)

	// Since there are 2 updates made for every payment and the maximum
	// number of updates per session has been set to 10, make 5 payments
	// between the pair so that the session is exhausted.
	alicePayReqs, _, _ := ht.CreatePayReqs(
		ht.Alice, paymentAmt, numInvoices,
	)

	send := func(node *node.HarnessNode, payReq string) {
		stream := node.RPC.SendPayment(&routerrpc.SendPaymentRequest{
			PaymentRequest: payReq,
			TimeoutSeconds: 60,
			FeeLimitMsat:   noFeeLimitMsat,
		})

		ht.AssertPaymentStatusFromStream(
			stream, lnrpc.Payment_SUCCEEDED,
		)
	}

	for i := 0; i < numInvoices; i++ {
		send(dave, alicePayReqs[i])
	}

	// assertNumBackups is a closure that asserts that Dave has a certain
	// number of backups backed up to the tower. If mineOnFail is true,
	// then a block will be mined each time the assertion fails.
	assertNumBackups := func(expected int, mineOnFail bool) {
		err = wait.NoError(func() error {
			info := dave.RPC.GetTowerInfo(
				&wtclientrpc.GetTowerInfoRequest{
					Pubkey:          wallisInfo.Pubkey,
					IncludeSessions: true,
				},
			)

			var numBackups uint32
			for _, sessionType := range info.SessionInfo {
				for _, session := range sessionType.Sessions {
					numBackups += session.NumBackups
				}
			}

			if numBackups == uint32(expected) {
				return nil
			}

			if mineOnFail {
				ht.Miner.MineBlocksSlow(1)
			}

			return fmt.Errorf("expected %d backups, got %d",
				expected, numBackups)
		}, defaultTimeout)
		require.NoError(ht, err)
	}

	// Assert that one of the sessions now has 10 backups.
	assertNumBackups(10, false)

	// Now close the channel and wait for the close transaction to appear
	// in the mempool so that it is included in a block when we mine.
	ht.CloseChannelAssertPending(dave, chanPoint, false)

	// Mine enough blocks to surpass the session-close-range. This should
	// trigger the session to be deleted.
	ht.MineBlocksAndAssertNumTxes(sessionCloseRange+6, 1)

	// Wait for the session to be deleted. We know it has been deleted once
	// the number of backups is back to zero. We check for number of backups
	// instead of number of sessions because it is expected that the client
	// would immediately negotiate another session after deleting the
	// exhausted one. This time we set the "mineOnFail" parameter to true to
	// ensure that the session deleting logic is run.
	assertNumBackups(0, true)
}
