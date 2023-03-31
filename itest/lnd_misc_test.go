package itest

import (
	"encoding/hex"
	"fmt"
	"io/ioutil"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// testDisconnectingTargetPeer performs a test which disconnects Alice-peer
// from Bob-peer and then re-connects them again. We expect Alice to be able to
// disconnect at any point.
//
// TODO(yy): move to lnd_network_test.
func testDisconnectingTargetPeer(ht *lntest.HarnessTest) {
	// We'll start both nodes with a high backoff so that they don't
	// reconnect automatically during our test.
	args := []string{
		"--minbackoff=1m",
		"--maxbackoff=1m",
	}

	alice, bob := ht.Alice, ht.Bob
	ht.RestartNodeWithExtraArgs(alice, args)
	ht.RestartNodeWithExtraArgs(bob, args)

	// Start by connecting Alice and Bob with no channels.
	ht.EnsureConnected(alice, bob)

	chanAmt := funding.MaxBtcFundingAmount
	pushAmt := btcutil.Amount(0)

	// Create a new channel that requires 1 confs before it's considered
	// open, then broadcast the funding transaction
	const numConfs = 1
	p := lntest.OpenChannelParams{
		Amt:     chanAmt,
		PushAmt: pushAmt,
	}
	stream := ht.OpenChannelAssertStream(alice, bob, p)

	// At this point, the channel's funding transaction will have been
	// broadcast, but not confirmed. Alice and Bob's nodes should reflect
	// this when queried via RPC.
	ht.AssertNumPendingOpenChannels(alice, 1)
	ht.AssertNumPendingOpenChannels(bob, 1)

	// Disconnect Alice-peer from Bob-peer should have no error.
	ht.DisconnectNodes(alice, bob)

	// Assert that the connection was torn down.
	ht.AssertNotConnected(alice, bob)

	// Mine a block, then wait for Alice's node to notify us that the
	// channel has been opened.
	ht.MineBlocksAndAssertNumTxes(numConfs, 1)

	// At this point, the channel should be fully opened and there should
	// be no pending channels remaining for either node.
	ht.AssertNumPendingOpenChannels(alice, 0)
	ht.AssertNumPendingOpenChannels(bob, 0)

	// Reconnect the nodes so that the channel can become active.
	ht.ConnectNodes(alice, bob)

	// The channel should be listed in the peer information returned by
	// both peers.
	chanPoint := ht.WaitForChannelOpenEvent(stream)

	// Check both nodes to ensure that the channel is ready for operation.
	ht.AssertChannelExists(alice, chanPoint)
	ht.AssertChannelExists(bob, chanPoint)

	// Disconnect Alice-peer from Bob-peer should have no error.
	ht.DisconnectNodes(alice, bob)

	// Check existing connection.
	ht.AssertNotConnected(alice, bob)

	// Reconnect both nodes before force closing the channel.
	ht.ConnectNodes(alice, bob)

	// Finally, immediately close the channel. This function will also
	// block until the channel is closed and will additionally assert the
	// relevant channel closing post conditions.
	ht.ForceCloseChannel(alice, chanPoint)

	// Disconnect Alice-peer from Bob-peer should have no error.
	ht.DisconnectNodes(alice, bob)

	// Check that the nodes not connected.
	ht.AssertNotConnected(alice, bob)

	// Finally, re-connect both nodes.
	ht.ConnectNodes(alice, bob)

	// Check existing connection.
	ht.AssertConnected(alice, bob)
}

// testSphinxReplayPersistence verifies that replayed onion packets are
// rejected by a remote peer after a restart. We use a combination of unsafe
// configuration arguments to force Carol to replay the same sphinx packet
// after reconnecting to Dave, and compare the returned failure message with
// what we expect for replayed onion packets.
func testSphinxReplayPersistence(ht *lntest.HarnessTest) {
	// Open a channel with 100k satoshis between Carol and Dave with Carol
	// being the sole funder of the channel.
	chanAmt := btcutil.Amount(100000)

	// First, we'll create Dave, the receiver, and start him in hodl mode.
	dave := ht.NewNode("Dave", []string{"--hodl.exit-settle"})

	// Next, we'll create Carol and establish a channel to from her to
	// Dave. Carol is started in both unsafe-replay which will cause her to
	// replay any pending Adds held in memory upon reconnection.
	carol := ht.NewNode("Carol", []string{"--unsafe-replay"})
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	ht.ConnectNodes(carol, dave)
	chanPoint := ht.OpenChannel(
		carol, dave, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Next, we'll create Fred who is going to initiate the payment and
	// establish a channel to from him to Carol. We can't perform this test
	// by paying from Carol directly to Dave, because the '--unsafe-replay'
	// setup doesn't apply to locally added htlcs. In that case, the
	// mailbox, that is responsible for generating the replay, is bypassed.
	fred := ht.NewNode("Fred", nil)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, fred)

	ht.ConnectNodes(fred, carol)
	chanPointFC := ht.OpenChannel(
		fred, carol, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)
	defer ht.CloseChannel(fred, chanPointFC)

	// Now that the channel is open, create an invoice for Dave which
	// expects a payment of 1000 satoshis from Carol paid via a particular
	// preimage.
	const paymentAmt = 1000
	preimage := ht.Random32Bytes()
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: preimage,
		Value:     paymentAmt,
	}
	invoiceResp := dave.RPC.AddInvoice(invoice)

	// Wait for all channels to be recognized and advertized.
	ht.AssertTopologyChannelOpen(carol, chanPoint)
	ht.AssertTopologyChannelOpen(dave, chanPoint)
	ht.AssertTopologyChannelOpen(carol, chanPointFC)
	ht.AssertTopologyChannelOpen(fred, chanPointFC)

	// With the invoice for Dave added, send a payment from Fred paying
	// to the above generated invoice.
	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	payStream := fred.RPC.SendPayment(req)

	// Dave's invoice should not be marked as settled.
	msg := &invoicesrpc.LookupInvoiceMsg{
		InvoiceRef: &invoicesrpc.LookupInvoiceMsg_PaymentAddr{
			PaymentAddr: invoiceResp.PaymentAddr,
		},
	}
	dbInvoice := dave.RPC.LookupInvoiceV2(msg)
	require.NotEqual(ht, lnrpc.InvoiceHTLCState_SETTLED, dbInvoice.State,
		"dave's invoice should not be marked as settled")

	// With the payment sent but hedl, all balance related stats should not
	// have changed.
	ht.AssertAmountPaid("carol => dave", carol, chanPoint, 0, 0)
	ht.AssertAmountPaid("dave <= carol", dave, chanPoint, 0, 0)

	// Before we restart Dave, make sure both Carol and Dave have added the
	// HTLC.
	ht.AssertNumActiveHtlcs(carol, 2)
	ht.AssertNumActiveHtlcs(dave, 1)

	// With the first payment sent, restart dave to make sure he is
	// persisting the information required to detect replayed sphinx
	// packets.
	ht.RestartNode(dave)

	// Carol should retransmit the Add hedl in her mailbox on startup. Dave
	// should not accept the replayed Add, and actually fail back the
	// pending payment. Even though he still holds the original settle, if
	// he does fail, it is almost certainly caused by the sphinx replay
	// protection, as it is the only validation we do in hodl mode.
	//
	// Assert that Fred receives the expected failure after Carol sent a
	// duplicate packet that fails due to sphinx replay detection.
	ht.AssertPaymentStatusFromStream(payStream, lnrpc.Payment_FAILED)
	ht.AssertLastHTLCError(fred, lnrpc.Failure_INVALID_ONION_KEY)

	// Since the payment failed, the balance should still be left
	// unaltered.
	ht.AssertAmountPaid("carol => dave", carol, chanPoint, 0, 0)
	ht.AssertAmountPaid("dave <= carol", dave, chanPoint, 0, 0)

	// Cleanup by mining the force close and sweep transaction.
	ht.ForceCloseChannel(carol, chanPoint)
}

// testListChannels checks that the response from ListChannels is correct. It
// tests the values in all ChannelConstraints are returned as expected. Once
// ListChannels becomes mature, a test against all fields in ListChannels
// should be performed.
func testListChannels(ht *lntest.HarnessTest) {
	const aliceRemoteMaxHtlcs = 50
	const bobRemoteMaxHtlcs = 100

	// Get the standby nodes and open a channel between them.
	alice, bob := ht.Alice, ht.Bob

	args := []string{fmt.Sprintf(
		"--default-remote-max-htlcs=%v",
		bobRemoteMaxHtlcs,
	)}
	ht.RestartNodeWithExtraArgs(bob, args)

	// Connect Alice to Bob.
	ht.EnsureConnected(alice, bob)

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel. The minial HTLC amount is set
	// to 4200 msats.
	const customizedMinHtlc = 4200

	chanAmt := btcutil.Amount(100000)
	pushAmt := btcutil.Amount(1000)
	p := lntest.OpenChannelParams{
		Amt:            chanAmt,
		PushAmt:        pushAmt,
		MinHtlc:        customizedMinHtlc,
		RemoteMaxHtlcs: aliceRemoteMaxHtlcs,
	}
	chanPoint := ht.OpenChannel(alice, bob, p)
	defer ht.CloseChannel(alice, chanPoint)

	// Alice should have one channel opened with Bob.
	ht.AssertNodeNumChannels(alice, 1)
	// Bob should have one channel opened with Alice.
	ht.AssertNodeNumChannels(bob, 1)

	// Check the returned response is correct.
	aliceChannel := ht.QueryChannelByChanPoint(alice, chanPoint)

	// Query the channel again, this time with peer alias lookup.
	aliceChannelWithAlias := ht.QueryChannelByChanPoint(
		alice, chanPoint, lntest.WithPeerAliasLookup(),
	)

	// Since Alice is the initiator, she pays the commit fee.
	aliceBalance := int64(chanAmt) - aliceChannel.CommitFee - int64(pushAmt)

	bobAlias := bob.RPC.GetInfo().Alias

	// Check the balance related fields are correct.
	require.Equal(ht, aliceBalance, aliceChannel.LocalBalance)
	require.Empty(ht, aliceChannel.PeerAlias)
	require.Equal(ht, bobAlias, aliceChannelWithAlias.PeerAlias)
	require.EqualValues(ht, pushAmt, aliceChannel.RemoteBalance)
	require.EqualValues(ht, pushAmt, aliceChannel.PushAmountSat)

	// Calculate the dust limit we'll use for the test.
	dustLimit := lnwallet.DustLimitForSize(input.UnknownWitnessSize)

	// defaultConstraints is a ChannelConstraints with default values. It
	// is used to test against Alice's local channel constraints.
	defaultConstraints := &lnrpc.ChannelConstraints{
		CsvDelay:          4,
		ChanReserveSat:    1000,
		DustLimitSat:      uint64(dustLimit),
		MaxPendingAmtMsat: 99000000,
		MinHtlcMsat:       1,
		MaxAcceptedHtlcs:  bobRemoteMaxHtlcs,
	}
	assertChannelConstraintsEqual(
		ht, defaultConstraints, aliceChannel.LocalConstraints,
	)

	// customizedConstraints is a ChannelConstraints with customized
	// values. Ideally, all these values can be passed in when creating the
	// channel. Currently, only the MinHtlcMsat is customized. It is used
	// to check against Alice's remote channel constratins.
	customizedConstraints := &lnrpc.ChannelConstraints{
		CsvDelay:          4,
		ChanReserveSat:    1000,
		DustLimitSat:      uint64(dustLimit),
		MaxPendingAmtMsat: 99000000,
		MinHtlcMsat:       customizedMinHtlc,
		MaxAcceptedHtlcs:  aliceRemoteMaxHtlcs,
	}
	assertChannelConstraintsEqual(
		ht, customizedConstraints, aliceChannel.RemoteConstraints,
	)

	// Get the ListChannel response for Bob.
	bobChannel := ht.QueryChannelByChanPoint(bob, chanPoint)
	require.Equal(ht, aliceChannel.ChannelPoint, bobChannel.ChannelPoint,
		"Bob's channel point mismatched")

	// Query the channel again, this time with node alias lookup.
	bobChannelWithAlias := ht.QueryChannelByChanPoint(
		bob, chanPoint, lntest.WithPeerAliasLookup(),
	)

	aliceAlias := alice.RPC.GetInfo().Alias

	// Check the balance related fields are correct.
	require.Equal(ht, aliceBalance, bobChannel.RemoteBalance)
	require.Empty(ht, bobChannel.PeerAlias)
	require.Equal(ht, aliceAlias, bobChannelWithAlias.PeerAlias)
	require.EqualValues(ht, pushAmt, bobChannel.LocalBalance)
	require.EqualValues(ht, pushAmt, bobChannel.PushAmountSat)

	// Check channel constraints match. Alice's local channel constraint
	// should be equal to Bob's remote channel constraint, and her remote
	// one should be equal to Bob's local one.
	assertChannelConstraintsEqual(
		ht, aliceChannel.LocalConstraints, bobChannel.RemoteConstraints,
	)
	assertChannelConstraintsEqual(
		ht, aliceChannel.RemoteConstraints, bobChannel.LocalConstraints,
	)
}

// testMaxPendingChannels checks that error is returned from remote peer if
// max pending channel number was exceeded and that '--maxpendingchannels' flag
// exists and works properly.
func testMaxPendingChannels(ht *lntest.HarnessTest) {
	maxPendingChannels := lncfg.DefaultMaxPendingChannels + 1
	amount := funding.MaxBtcFundingAmount

	// Create a new node (Carol) with greater number of max pending
	// channels.
	args := []string{
		fmt.Sprintf("--maxpendingchannels=%v", maxPendingChannels),
	}
	carol := ht.NewNode("Carol", args)

	alice := ht.Alice
	ht.ConnectNodes(alice, carol)

	carolBalance := btcutil.Amount(maxPendingChannels) * amount
	ht.FundCoins(carolBalance, carol)

	// Send open channel requests without generating new blocks thereby
	// increasing pool of pending channels. Then check that we can't open
	// the channel if the number of pending channels exceed max value.
	openStreams := make(
		[]lnrpc.Lightning_OpenChannelClient, maxPendingChannels,
	)
	for i := 0; i < maxPendingChannels; i++ {
		stream := ht.OpenChannelAssertStream(
			alice, carol, lntest.OpenChannelParams{
				Amt: amount,
			},
		)
		openStreams[i] = stream
	}

	// Carol exhausted available amount of pending channels, next open
	// channel request should cause ErrorGeneric to be sent back to Alice.
	ht.OpenChannelAssertErr(
		alice, carol, lntest.OpenChannelParams{
			Amt: amount,
		}, lnwire.ErrMaxPendingChannels,
	)

	// For now our channels are in pending state, in order to not interfere
	// with other tests we should clean up - complete opening of the
	// channel and then close it.

	// Mine 6 blocks, then wait for node's to notify us that the channel
	// has been opened. The funding transactions should be found within the
	// first newly mined block. 6 blocks make sure the funding transaction
	// has enough confirmations to be announced publicly.
	block := ht.MineBlocksAndAssertNumTxes(6, maxPendingChannels)[0]

	chanPoints := make([]*lnrpc.ChannelPoint, maxPendingChannels)
	for i, stream := range openStreams {
		fundingChanPoint := ht.WaitForChannelOpenEvent(stream)

		fundingTxID := ht.GetChanPointFundingTxid(fundingChanPoint)

		// Ensure that the funding transaction enters a block, and is
		// properly advertised by Alice.
		ht.Miner.AssertTxInBlock(block, fundingTxID)
		ht.AssertTopologyChannelOpen(alice, fundingChanPoint)

		// The channel should be listed in the peer information
		// returned by both peers.
		ht.AssertChannelExists(alice, fundingChanPoint)

		chanPoints[i] = fundingChanPoint
	}

	// Next, close the channel between Alice and Carol, asserting that the
	// channel has been properly closed on-chain.
	for _, chanPoint := range chanPoints {
		ht.CloseChannel(alice, chanPoint)
	}
}

// testGarbageCollectLinkNodes tests that we properly garbage collect link
// nodes from the database and the set of persistent connections within the
// server.
func testGarbageCollectLinkNodes(ht *lntest.HarnessTest) {
	const chanAmt = 1000000

	alice, bob := ht.Alice, ht.Bob

	// Open a channel between Alice and Bob which will later be
	// cooperatively closed.
	coopChanPoint := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Create Carol's node and connect Alice to her.
	carol := ht.NewNode("Carol", nil)
	ht.ConnectNodes(alice, carol)

	// Open a channel between Alice and Carol which will later be force
	// closed.
	forceCloseChanPoint := ht.OpenChannel(
		alice, carol, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Now, create Dave's a node and also open a channel between Alice and
	// him. This link will serve as the only persistent link throughout
	// restarts in this test.
	dave := ht.NewNode("Dave", nil)

	ht.ConnectNodes(alice, dave)
	persistentChanPoint := ht.OpenChannel(
		alice, dave, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Restart both Bob and Carol to ensure Alice is able to reconnect to
	// them.
	ht.RestartNode(bob)
	ht.RestartNode(carol)

	ht.AssertConnected(alice, bob)
	ht.AssertConnected(alice, carol)

	// We'll also restart Alice to ensure she can reconnect to her peers
	// with open channels.
	ht.RestartNode(alice)

	ht.AssertConnected(alice, bob)
	ht.AssertConnected(alice, carol)
	ht.AssertConnected(alice, dave)

	// testReconnection is a helper closure that restarts the nodes at both
	// ends of a channel to ensure they do not reconnect after restarting.
	// When restarting Alice, we'll first need to ensure she has
	// reestablished her connection with Dave, as they still have an open
	// channel together.
	testReconnection := func(node *node.HarnessNode) {
		// Restart both nodes, to trigger the pruning logic.
		ht.RestartNode(node)
		ht.RestartNode(alice)

		// Now restart both nodes and make sure they don't reconnect.
		ht.RestartNode(node)
		ht.AssertNotConnected(alice, node)

		ht.RestartNode(alice)
		ht.AssertConnected(alice, dave)
		ht.AssertNotConnected(alice, node)
	}

	// Now, we'll close the channel between Alice and Bob and ensure there
	// is no reconnection logic between the both once the channel is fully
	// closed.
	ht.CloseChannel(alice, coopChanPoint)

	testReconnection(bob)

	// We'll do the same with Alice and Carol, but this time we'll force
	// close the channel instead.
	ht.ForceCloseChannel(alice, forceCloseChanPoint)

	// We'll need to mine some blocks in order to mark the channel fully
	// closed.
	ht.MineBlocks(
		chainreg.DefaultBitcoinTimeLockDelta - defaultCSV,
	)

	// Before we test reconnection, we'll ensure that the channel has been
	// fully cleaned up for both Carol and Alice.
	ht.AssertNumPendingForceClose(alice, 0)
	ht.AssertNumPendingForceClose(carol, 0)

	testReconnection(carol)

	// Finally, we'll ensure that Bob and Carol no longer show in Alice's
	// channel graph.
	req := &lnrpc.ChannelGraphRequest{IncludeUnannounced: true}
	channelGraph := alice.RPC.DescribeGraph(req)
	require.NotContains(ht, channelGraph.Nodes, bob.PubKeyStr,
		"did not expect to find bob in the channel graph, but did")
	require.NotContains(ht, channelGraph.Nodes, carol.PubKeyStr,
		"did not expect to find carol in the channel graph, but did")

	// Now that the test is done, we can also close the persistent link.
	ht.CloseChannel(alice, persistentChanPoint)
}

// testRejectHTLC tests that a node can be created with the flag --rejecthtlc.
// This means that the node will reject all forwarded HTLCs but can still
// accept direct HTLCs as well as send HTLCs.
func testRejectHTLC(ht *lntest.HarnessTest) {
	//             RejectHTLC
	// Alice ------> Carol ------> Bob
	//
	const chanAmt = btcutil.Amount(1000000)
	alice, bob := ht.Alice, ht.Bob

	// Create Carol with reject htlc flag.
	carol := ht.NewNode("Carol", []string{"--rejecthtlc"})

	// Connect Alice to Carol.
	ht.ConnectNodes(alice, carol)

	// Connect Carol to Bob.
	ht.ConnectNodes(carol, bob)

	// Send coins to Carol.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// Open a channel between Alice and Carol.
	chanPointAlice := ht.OpenChannel(
		alice, carol, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Open a channel between Carol and Bob.
	chanPointCarol := ht.OpenChannel(
		carol, bob, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Channel should be ready for payments.
	const payAmt = 100

	// Create an invoice from Carol of 100 satoshis.
	// We expect Alice to be able to pay this invoice.
	carolInvoice := &lnrpc.Invoice{
		Memo:      "testing - alice should pay carol",
		RPreimage: ht.Random32Bytes(),
		Value:     payAmt,
	}

	// Carol adds the invoice to her database.
	resp := carol.RPC.AddInvoice(carolInvoice)

	// Alice pays Carols invoice.
	ht.CompletePaymentRequests(alice, []string{resp.PaymentRequest})

	// Create an invoice from Bob of 100 satoshis.
	// We expect Carol to be able to pay this invoice.
	bobInvoice := &lnrpc.Invoice{
		Memo:      "testing - carol should pay bob",
		RPreimage: ht.Random32Bytes(),
		Value:     payAmt,
	}

	// Bob adds the invoice to his database.
	resp = bob.RPC.AddInvoice(bobInvoice)

	// Carol pays Bobs invoice.
	ht.CompletePaymentRequests(carol, []string{resp.PaymentRequest})

	// Create an invoice from Bob of 100 satoshis.
	// Alice attempts to pay Bob but this should fail, since we are
	// using Carol as a hop and her node will reject onward HTLCs.
	bobInvoice = &lnrpc.Invoice{
		Memo:      "testing - alice tries to pay bob",
		RPreimage: ht.Random32Bytes(),
		Value:     payAmt,
	}

	// Bob adds the invoice to his database.
	resp = bob.RPC.AddInvoice(bobInvoice)

	// Alice attempts to pay Bobs invoice. This payment should be rejected
	// since we are using Carol as an intermediary hop, Carol is running
	// lnd with --rejecthtlc.
	paymentReq := &routerrpc.SendPaymentRequest{
		PaymentRequest: resp.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	payStream := alice.RPC.SendPayment(paymentReq)
	ht.AssertPaymentStatusFromStream(payStream, lnrpc.Payment_FAILED)

	ht.AssertLastHTLCError(alice, lnrpc.Failure_CHANNEL_DISABLED)

	// Close all channels.
	ht.CloseChannel(alice, chanPointAlice)
	ht.CloseChannel(carol, chanPointCarol)
}

// testNodeSignVerify checks that only connected nodes are allowed to perform
// signing and verifying messages.
func testNodeSignVerify(ht *lntest.HarnessTest) {
	chanAmt := funding.MaxBtcFundingAmount
	pushAmt := btcutil.Amount(100000)
	alice, bob := ht.Alice, ht.Bob

	// Create a channel between alice and bob.
	aliceBobCh := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: pushAmt,
		},
	)

	// alice signs "alice msg" and sends her signature to bob.
	aliceMsg := []byte("alice msg")
	sigResp := alice.RPC.SignMessage(aliceMsg)
	aliceSig := sigResp.Signature

	// bob verifying alice's signature should succeed since alice and bob
	// are connected.
	verifyResp := bob.RPC.VerifyMessage(aliceMsg, aliceSig)
	require.True(ht, verifyResp.Valid, "alice's signature didn't validate")
	require.Equal(ht, verifyResp.Pubkey, alice.PubKeyStr,
		"alice's signature doesn't contain alice's pubkey.")

	// carol is a new node that is unconnected to alice or bob.
	carol := ht.NewNode("Carol", nil)

	// carol signs "carol msg" and sends her signature to bob.
	carolMsg := []byte("carol msg")
	sigResp = carol.RPC.SignMessage(carolMsg)
	carolSig := sigResp.Signature

	// bob verifying carol's signature should fail since they are not
	// connected.
	verifyResp = bob.RPC.VerifyMessage(carolMsg, carolSig)
	require.False(ht, verifyResp.Valid, "carol's signature didn't validate")
	require.Equal(ht, verifyResp.Pubkey, carol.PubKeyStr,
		"carol's signature doesn't contain alice's pubkey.")

	// Close the channel between alice and bob.
	ht.CloseChannel(alice, aliceBobCh)
}

// testAbandonChannel abandons a channel and asserts that it is no longer open
// and not in one of the pending closure states. It also verifies that the
// abandoned channel is reported as closed with close type 'abandoned'.
func testAbandonChannel(ht *lntest.HarnessTest) {
	alice, bob := ht.Alice, ht.Bob

	// First establish a channel between Alice and Bob.
	channelParam := lntest.OpenChannelParams{
		Amt:     funding.MaxBtcFundingAmount,
		PushAmt: btcutil.Amount(100000),
	}
	chanPoint := ht.OpenChannel(alice, bob, channelParam)

	// Now that the channel is open, we'll obtain its channel ID real quick
	// so we can use it to query the graph below.
	chanID := ht.QueryChannelByChanPoint(alice, chanPoint).ChanId

	// To make sure the channel is removed from the backup file as well
	// when being abandoned, grab a backup snapshot so we can compare it
	// with the later state.
	bkupBefore, err := ioutil.ReadFile(alice.Cfg.ChanBackupPath())
	require.NoError(ht, err, "channel backup before abandoning channel")

	// Send request to abandon channel.
	abandonChannelRequest := &lnrpc.AbandonChannelRequest{
		ChannelPoint: chanPoint,
	}
	alice.RPC.AbandonChannel(abandonChannelRequest)

	// Assert that channel in no longer open.
	ht.AssertNodeNumChannels(alice, 0)

	// Assert that channel is not pending closure.
	ht.AssertNumWaitingClose(alice, 0)

	// Assert that channel is listed as abandoned.
	req := &lnrpc.ClosedChannelsRequest{Abandoned: true}
	aliceClosedList := alice.RPC.ClosedChannels(req)
	require.Len(ht, aliceClosedList.Channels, 1, "alice closed channels")

	// Ensure that the channel can no longer be found in the channel graph.
	ht.AssertZombieChannel(alice, chanID)

	// Make sure the channel is no longer in the channel backup list.
	err = wait.NoError(func() error {
		bkupAfter, err := ioutil.ReadFile(alice.Cfg.ChanBackupPath())
		if err != nil {
			return fmt.Errorf("could not get channel backup "+
				"before abandoning channel: %v", err)
		}

		if len(bkupAfter) >= len(bkupBefore) {
			return fmt.Errorf("expected backups after to be less "+
				"than %d but was %d", bkupBefore, bkupAfter)
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err, "channel removed from backup file")

	// Calling AbandonChannel again, should result in no new errors, as the
	// channel has already been removed.
	alice.RPC.AbandonChannel(abandonChannelRequest)

	// Now that we're done with the test, the channel can be closed. This
	// is necessary to avoid unexpected outcomes of other tests that use
	// Bob's lnd instance.
	ht.ForceCloseChannel(bob, chanPoint)
}

// testSweepAllCoins tests that we're able to properly sweep all coins from the
// wallet into a single target address at the specified fee rate.
func testSweepAllCoins(ht *lntest.HarnessTest) {
	// First, we'll make a new node, Ainz who'll we'll use to test wallet
	// sweeping.
	//
	// NOTE: we won't use standby nodes here since the test will change
	// each of the node's wallet state.
	ainz := ht.NewNode("Ainz", nil)

	// Next, we'll give Ainz exactly 2 utxos of 1 BTC each, with one of
	// them being p2wkh and the other being a n2wpkh address.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, ainz)
	ht.FundCoinsNP2WKH(btcutil.SatoshiPerBitcoin, ainz)

	// Create a label that will be used to label the transaction with.
	sendCoinsLabel := "send all coins"

	// Ensure that we can't send coins to our own Pubkey.
	ainz.RPC.SendCoinsAssertErr(&lnrpc.SendCoinsRequest{
		Addr:    ainz.RPC.GetInfo().IdentityPubkey,
		SendAll: true,
		Label:   sendCoinsLabel,
	})

	// Ensure that we can't send coins to another user's Pubkey.
	ainz.RPC.SendCoinsAssertErr(&lnrpc.SendCoinsRequest{
		Addr:    ht.Alice.RPC.GetInfo().IdentityPubkey,
		SendAll: true,
		Label:   sendCoinsLabel,
	})

	// With the two coins above mined, we'll now instruct Ainz to sweep all
	// the coins to an external address not under its control. We will first
	// attempt to send the coins to addresses that are not compatible
	// with the current network. This is to test that the wallet will
	// prevent any on-chain transactions to addresses that are not on the
	// same network as the user.

	// Send coins to a testnet3 address.
	ainz.RPC.SendCoinsAssertErr(&lnrpc.SendCoinsRequest{
		Addr:    "tb1qfc8fusa98jx8uvnhzavxccqlzvg749tvjw82tg",
		SendAll: true,
		Label:   sendCoinsLabel,
	})

	// Send coins to a mainnet address.
	ainz.RPC.SendCoinsAssertErr(&lnrpc.SendCoinsRequest{
		Addr:    "1MPaXKp5HhsLNjVSqaL7fChE3TVyrTMRT3",
		SendAll: true,
		Label:   sendCoinsLabel,
	})

	// Send coins to a compatible address.
	ainz.RPC.SendCoins(&lnrpc.SendCoinsRequest{
		Addr:    ht.Miner.NewMinerAddress().String(),
		SendAll: true,
		Label:   sendCoinsLabel,
	})

	// We'll mine a block which should include the sweep transaction we
	// generated above.
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]

	// The sweep transaction should have exactly two inputs as we only had
	// two UTXOs in the wallet.
	sweepTx := block.Transactions[1]
	require.Len(ht, sweepTx.TxIn, 2, "expected 2 inputs")

	// assertTxLabel is a helper function which finds a target tx in our
	// set of transactions and checks that it has the desired label.
	assertTxLabel := func(targetTx, label string) error {
		// List all transactions relevant to our wallet, and find the
		// tx so that we can check the correct label has been set.
		txResp := ainz.RPC.GetTransactions(nil)

		var target *lnrpc.Transaction

		// First we need to find the target tx.
		for _, txn := range txResp.Transactions {
			if txn.TxHash == targetTx {
				target = txn
				break
			}
		}

		// If we cannot find it, return an error.
		if target == nil {
			return fmt.Errorf("target tx %v not found", targetTx)
		}

		// Otherwise, check the labels are matched.
		if target.Label == label {
			return nil
		}

		return fmt.Errorf("labels not match, want: "+
			"%v, got %v", label, target.Label)
	}

	// waitTxLabel waits until the desired tx label is found or timeout.
	waitTxLabel := func(targetTx, label string) {
		err := wait.NoError(func() error {
			return assertTxLabel(targetTx, label)
		}, defaultTimeout)

		require.NoError(ht, err, "timeout assertTxLabel")
	}

	sweepTxStr := sweepTx.TxHash().String()
	waitTxLabel(sweepTxStr, sendCoinsLabel)

	// While we are looking at labels, we test our label transaction
	// command to make sure it is behaving as expected. First, we try to
	// label our transaction with an empty label, and check that we fail as
	// expected.
	sweepHash := sweepTx.TxHash()
	err := ainz.RPC.LabelTransactionAssertErr(
		&walletrpc.LabelTransactionRequest{
			Txid:      sweepHash[:],
			Label:     "",
			Overwrite: false,
		},
	)

	// Our error will be wrapped in a rpc error, so we check that it
	// contains the error we expect.
	errZeroLabel := "cannot label transaction with empty label"
	require.Contains(ht, err.Error(), errZeroLabel)

	// Next, we try to relabel our transaction without setting the overwrite
	// boolean. We expect this to fail, because the wallet requires setting
	// of this param to prevent accidental overwrite of labels.
	err = ainz.RPC.LabelTransactionAssertErr(
		&walletrpc.LabelTransactionRequest{
			Txid:      sweepHash[:],
			Label:     "label that will not work",
			Overwrite: false,
		},
	)

	// Our error will be wrapped in a rpc error, so we check that it
	// contains the error we expect.
	require.Contains(ht, err.Error(), wallet.ErrTxLabelExists.Error())

	// Finally, we overwrite our label with a new label, which should not
	// fail.
	newLabel := "new sweep tx label"
	ainz.RPC.LabelTransaction(&walletrpc.LabelTransactionRequest{
		Txid:      sweepHash[:],
		Label:     newLabel,
		Overwrite: true,
	})

	waitTxLabel(sweepTxStr, newLabel)

	// Finally, Ainz should now have no coins at all within his wallet.
	resp := ainz.RPC.WalletBalance()
	require.Zero(ht, resp.ConfirmedBalance, "wrong confirmed balance")
	require.Zero(ht, resp.UnconfirmedBalance, "wrong unconfirmed balance")

	// If we try again, but this time specifying an amount, then the call
	// should fail.
	ainz.RPC.SendCoinsAssertErr(&lnrpc.SendCoinsRequest{
		Addr:    ht.Miner.NewMinerAddress().String(),
		Amount:  10000,
		SendAll: true,
		Label:   sendCoinsLabel,
	})

	// With all the edge cases tested, we'll now test the happy paths of
	// change output types.
	// We'll be using a "main" address where we send the funds to and from
	// several times.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, ainz)
	mainAddrResp := ainz.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	})
	spendAddrTypes := []lnrpc.AddressType{
		lnrpc.AddressType_NESTED_PUBKEY_HASH,
		lnrpc.AddressType_WITNESS_PUBKEY_HASH,
		lnrpc.AddressType_TAPROOT_PUBKEY,
	}
	for _, addrType := range spendAddrTypes {
		ht.Logf("testing with address type %s", addrType)

		// First, spend all the coins in the wallet to an address of the
		// given type so that UTXO will be picked when sending coins.
		sendAllCoinsToAddrType(ht, ainz, addrType)

		// Let's send some coins to the main address.
		const amt = 123456
		resp := ainz.RPC.SendCoins(&lnrpc.SendCoinsRequest{
			Addr:   mainAddrResp.Address,
			Amount: amt,
		})
		block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
		sweepTx := block.Transactions[1]
		require.Equal(ht, sweepTx.TxHash().String(), resp.Txid)

		// Find the change output, it will be the one with an amount
		// different from the amount we sent.
		var changeOutput *wire.TxOut
		for idx := range sweepTx.TxOut {
			txOut := sweepTx.TxOut[idx]
			if txOut.Value != amt {
				changeOutput = txOut
				break
			}
		}
		require.NotNil(ht, changeOutput)

		// Assert the type of change output to be p2tr.
		pkScript, err := txscript.ParsePkScript(changeOutput.PkScript)
		require.NoError(ht, err)
		require.Equal(ht, txscript.WitnessV1TaprootTy, pkScript.Class())
	}
}

// testListAddresses tests that we get all the addresses and their
// corresponding balance correctly.
func testListAddresses(ht *lntest.HarnessTest) {
	// First, we'll make a new node - Alice, which will be generating
	// new addresses.
	alice := ht.NewNode("Alice", nil)

	// Next, we'll give Alice exactly 1 utxo of 1 BTC.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, alice)

	type addressDetails struct {
		Balance int64
		Type    walletrpc.AddressType
	}

	// A map of generated address and its balance.
	generatedAddr := make(map[string]addressDetails)

	// Create an address generated from internal keys.
	keyLoc := &walletrpc.KeyReq{KeyFamily: 123}
	keyDesc := alice.RPC.DeriveNextKey(keyLoc)

	// Hex Encode the public key.
	pubkeyString := hex.EncodeToString(keyDesc.RawKeyBytes)

	// Create a p2tr address.
	resp := alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_TAPROOT_PUBKEY,
	})
	generatedAddr[resp.Address] = addressDetails{
		Balance: 200_000,
		Type:    walletrpc.AddressType_TAPROOT_PUBKEY,
	}

	// Create a p2wkh address.
	resp = alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	})
	generatedAddr[resp.Address] = addressDetails{
		Balance: 300_000,
		Type:    walletrpc.AddressType_WITNESS_PUBKEY_HASH,
	}

	// Create a np2wkh address.
	resp = alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_NESTED_PUBKEY_HASH,
	})
	generatedAddr[resp.Address] = addressDetails{
		Balance: 400_000,
		Type: walletrpc.
			AddressType_HYBRID_NESTED_WITNESS_PUBKEY_HASH,
	}

	for addr, addressDetail := range generatedAddr {
		alice.RPC.SendCoins(&lnrpc.SendCoinsRequest{
			Addr:             addr,
			Amount:           addressDetail.Balance,
			SpendUnconfirmed: true,
		})
	}

	ht.MineBlocksAndAssertNumTxes(1, 3)

	// Get all the accounts except LND's custom accounts.
	addressLists := alice.RPC.ListAddresses(
		&walletrpc.ListAddressesRequest{},
	)

	foundAddresses := 0
	for _, addressList := range addressLists.AccountWithAddresses {
		addresses := addressList.Addresses
		derivationPath, err := lntest.ParseDerivationPath(
			addressList.DerivationPath,
		)
		require.NoError(ht, err)

		// Should not get an account with KeyFamily - 123.
		require.NotEqual(
			ht, uint32(keyLoc.KeyFamily), derivationPath[2],
		)

		for _, address := range addresses {
			if _, ok := generatedAddr[address.Address]; ok {
				addrDetails := generatedAddr[address.Address]
				require.Equal(
					ht, addrDetails.Balance,
					address.Balance,
				)
				require.Equal(
					ht, addrDetails.Type,
					addressList.AddressType,
				)
				foundAddresses++
			}
		}
	}

	require.Equal(ht, len(generatedAddr), foundAddresses)
	foundAddresses = 0

	// Get all the accounts (including LND's custom accounts).
	addressLists = alice.RPC.ListAddresses(
		&walletrpc.ListAddressesRequest{
			ShowCustomAccounts: true,
		},
	)

	for _, addressList := range addressLists.AccountWithAddresses {
		addresses := addressList.Addresses
		derivationPath, err := lntest.ParseDerivationPath(
			addressList.DerivationPath,
		)
		require.NoError(ht, err)

		for _, address := range addresses {
			// Check if the KeyFamily in derivation path is 123.
			if uint32(keyLoc.KeyFamily) == derivationPath[2] {
				// For LND's custom accounts, the address
				// represents the public key.
				pubkey := address.Address
				require.Equal(ht, pubkeyString, pubkey)
			} else if _, ok := generatedAddr[address.Address]; ok {
				addrDetails := generatedAddr[address.Address]
				require.Equal(
					ht, addrDetails.Balance,
					address.Balance,
				)
				require.Equal(
					ht, addrDetails.Type,
					addressList.AddressType,
				)
				foundAddresses++
			}
		}
	}

	require.Equal(ht, len(generatedAddr), foundAddresses)
}

func assertChannelConstraintsEqual(ht *lntest.HarnessTest,
	want, got *lnrpc.ChannelConstraints) {

	require.Equal(ht, want.CsvDelay, got.CsvDelay, "CsvDelay mismatched")

	require.Equal(ht, want.ChanReserveSat, got.ChanReserveSat,
		"ChanReserveSat mismatched")

	require.Equal(ht, want.DustLimitSat, got.DustLimitSat,
		"DustLimitSat mismatched")

	require.Equal(ht, want.MaxPendingAmtMsat, got.MaxPendingAmtMsat,
		"MaxPendingAmtMsat mismatched")

	require.Equal(ht, want.MinHtlcMsat, got.MinHtlcMsat,
		"MinHtlcMsat mismatched")

	require.Equal(ht, want.MaxAcceptedHtlcs, got.MaxAcceptedHtlcs,
		"MaxAcceptedHtlcs mismatched")
}

// testSignVerifyMessageWithAddr tests signing and also verifying a signature
// on a message with a provided address.
func testSignVerifyMessageWithAddr(ht *lntest.HarnessTest) {
	// Using different nodes to sign the message and verify the signature.
	alice, bob := ht.Alice, ht.Bob

	// Test an lnd wallet created P2WKH address.
	respAddr := alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	})

	aliceMsg := []byte("alice msg")

	respSig := alice.RPC.SignMessageWithAddr(
		&walletrpc.SignMessageWithAddrRequest{
			Msg:  aliceMsg,
			Addr: respAddr.Address,
		},
	)

	respValid := bob.RPC.VerifyMessageWithAddr(
		&walletrpc.VerifyMessageWithAddrRequest{
			Msg:       aliceMsg,
			Signature: respSig.Signature,
			Addr:      respAddr.Address,
		},
	)

	require.True(ht, respValid.Valid, "alice's signature didn't validate")

	// Test an lnd wallet created NP2WKH address.
	respAddr = alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_NESTED_PUBKEY_HASH,
	})

	respSig = alice.RPC.SignMessageWithAddr(
		&walletrpc.SignMessageWithAddrRequest{
			Msg:  aliceMsg,
			Addr: respAddr.Address,
		},
	)

	respValid = bob.RPC.VerifyMessageWithAddr(
		&walletrpc.VerifyMessageWithAddrRequest{
			Msg:       aliceMsg,
			Signature: respSig.Signature,
			Addr:      respAddr.Address,
		},
	)

	require.True(ht, respValid.Valid, "alice's signature didn't validate")

	// Test an lnd wallet created P2TR address.
	respAddr = alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_TAPROOT_PUBKEY,
	})

	respSig = alice.RPC.SignMessageWithAddr(
		&walletrpc.SignMessageWithAddrRequest{
			Msg:  aliceMsg,
			Addr: respAddr.Address,
		},
	)

	respValid = bob.RPC.VerifyMessageWithAddr(
		&walletrpc.VerifyMessageWithAddrRequest{
			Msg:       aliceMsg,
			Signature: respSig.Signature,
			Addr:      respAddr.Address,
		},
	)

	require.True(ht, respValid.Valid, "alice's signature didn't validate")

	// Test verifying a signature with an external P2PKH address.
	// P2PKH address type is not supported by the lnd wallet therefore
	// using an external source (bitcoin-core) for address and
	// signature creation.
	externalMsg := []byte("external msg")
	externalAddr := "msS5c4VihSiJ64QzvMMEmWh6rYBnuWo2xH"

	// Base64 encoded signature created with bitcoin-core regtest.
	externalSig := "H5DqqM7Cc8xZnYBr7j3gD4XD+AuQsim9Un/IxBrrhBA7I9//" +
		"3exuQRg+u7HpwG65yobPsew6RMUteyuxyNkLF5E="

	respValid = alice.RPC.VerifyMessageWithAddr(
		&walletrpc.VerifyMessageWithAddrRequest{
			Msg:       externalMsg,
			Signature: externalSig,
			Addr:      externalAddr,
		},
	)

	require.True(ht, respValid.Valid, "external signature didn't validate")

	// Test verifying a signature with a different address which
	// initially was used to create the following signature.
	// externalAddr is a valid legacy P2PKH bitcoin address created
	// with bitcoin-core.
	externalAddr = "mugbg8CqFe9CbdrYjFTkMhmL3JxuEXkNbY"

	// Base64 encoded signature created with bitcoin-core regtest but with
	// the address msS5c4VihSiJ64QzvMMEmWh6rYBnuWo2xH.
	externalSig = "H5DqqM7Cc8xZnYBr7j3gD4XD+AuQsim9Un/IxBrrhBA7I9//" +
		"3exuQRg+u7HpwG65yobPsew6RMUteyuxyNkLF5E="

	respValid = alice.RPC.VerifyMessageWithAddr(
		&walletrpc.VerifyMessageWithAddrRequest{
			Msg:       externalMsg,
			Signature: externalSig,
			Addr:      externalAddr,
		},
	)

	require.False(ht, respValid.Valid, "external signature did validate")
}
