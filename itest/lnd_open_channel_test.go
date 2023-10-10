package itest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/rpc"
	"github.com/stretchr/testify/require"
)

// testOpenChannelAfterReorg tests that in the case where we have an open
// channel where the funding tx gets reorged out, the channel will no
// longer be present in the node's routing table.
func testOpenChannelAfterReorg(ht *lntest.HarnessTest) {
	// Skip test for neutrino, as we cannot disconnect the miner at will.
	// TODO(halseth): remove when either can disconnect at will, or restart
	// node with connection to new miner.
	if ht.IsNeutrinoBackend() {
		ht.Skipf("skipping reorg test for neutrino backend")
	}

	// Create a temp miner.
	tempMiner := ht.Miner.SpawnTempMiner()

	miner := ht.Miner
	alice, bob := ht.Alice, ht.Bob

	// Create a new channel that requires 1 confs before it's considered
	// open, then broadcast the funding transaction
	params := lntest.OpenChannelParams{
		Amt:     funding.MaxBtcFundingAmount,
		Private: true,
	}
	pendingUpdate := ht.OpenChannelAssertPending(alice, bob, params)

	// Wait for miner to have seen the funding tx. The temporary miner is
	// disconnected, and won't see the transaction.
	ht.Miner.AssertNumTxsInMempool(1)

	// At this point, the channel's funding transaction will have been
	// broadcast, but not confirmed, and the channel should be pending.
	ht.AssertNodesNumPendingOpenChannels(alice, bob, 1)

	fundingTxID, err := chainhash.NewHash(pendingUpdate.Txid)
	require.NoError(ht, err, "convert funding txid into chainhash failed")

	// We now cause a fork, by letting our original miner mine 10 blocks,
	// and our new miner mine 15. This will also confirm our pending
	// channel on the original miner's chain, which should be considered
	// open.
	block := ht.MineBlocks(10)[0]
	ht.Miner.AssertTxInBlock(block, fundingTxID)
	_, err = tempMiner.Client.Generate(15)
	require.NoError(ht, err, "unable to generate blocks")

	// Ensure the chain lengths are what we expect, with the temp miner
	// being 5 blocks ahead.
	miner.AssertMinerBlockHeightDelta(tempMiner, 5)

	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: pendingUpdate.Txid,
		},
		OutputIndex: pendingUpdate.OutputIndex,
	}

	// Ensure channel is no longer pending.
	ht.AssertNodesNumPendingOpenChannels(alice, bob, 0)

	// Wait for Alice and Bob to recognize and advertise the new channel
	// generated above.
	ht.AssertTopologyChannelOpen(alice, chanPoint)
	ht.AssertTopologyChannelOpen(bob, chanPoint)

	// Alice should now have 1 edge in her graph.
	ht.AssertNumEdges(alice, 1, true)

	// Now we disconnect Alice's chain backend from the original miner, and
	// connect the two miners together. Since the temporary miner knows
	// about a longer chain, both miners should sync to that chain.
	ht.DisconnectMiner()

	// Connecting to the temporary miner should now cause our original
	// chain to be re-orged out.
	miner.ConnectMiner(tempMiner)

	// Once again they should be on the same chain.
	miner.AssertMinerBlockHeightDelta(tempMiner, 0)

	// Now we disconnect the two miners, and connect our original miner to
	// our chain backend once again.
	miner.DisconnectMiner(tempMiner)

	ht.ConnectMiner()

	// This should have caused a reorg, and Alice should sync to the longer
	// chain, where the funding transaction is not confirmed.
	_, tempMinerHeight, err := tempMiner.Client.GetBestBlock()
	require.NoError(ht, err, "unable to get current blockheight")
	ht.WaitForNodeBlockHeight(alice, tempMinerHeight)

	// Since the fundingtx was reorged out, Alice should now have no edges
	// in her graph.
	ht.AssertNumEdges(alice, 0, true)

	// Cleanup by mining the funding tx again, then closing the channel.
	block = ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.Miner.AssertTxInBlock(block, fundingTxID)

	ht.CloseChannel(alice, chanPoint)
}

// testOpenChannelFeePolicy checks if different channel fee scenarios are
// correctly handled when the optional channel fee parameters baseFee and
// feeRate are provided. If the OpenChannelRequest is not provided with a value
// for baseFee/feeRate the expectation is that the default baseFee/feeRate is
// applied.
//
//  1. No params provided to OpenChannelRequest:
//     ChannelUpdate --> defaultBaseFee, defaultFeeRate
//  2. Only baseFee provided to OpenChannelRequest:
//     ChannelUpdate --> provided baseFee, defaultFeeRate
//  3. Only feeRate provided to OpenChannelRequest:
//     ChannelUpdate --> defaultBaseFee, provided FeeRate
//  4. baseFee and feeRate provided to OpenChannelRequest:
//     ChannelUpdate --> provided baseFee, provided feeRate
//  5. Both baseFee and feeRate are set to a value lower than the default:
//     ChannelUpdate --> provided baseFee, provided feeRate
func testOpenChannelUpdateFeePolicy(ht *lntest.HarnessTest) {
	const (
		defaultBaseFee       = 1000
		defaultFeeRate       = 1
		defaultTimeLockDelta = chainreg.DefaultBitcoinTimeLockDelta
		defaultMinHtlc       = 1000
		optionalBaseFee      = 1337
		optionalFeeRate      = 1337
		lowBaseFee           = 0
		lowFeeRate           = 900
	)

	defaultMaxHtlc := lntest.CalculateMaxHtlc(funding.MaxBtcFundingAmount)

	chanAmt := funding.MaxBtcFundingAmount
	pushAmt := chanAmt / 2

	feeScenarios := []lntest.OpenChannelParams{
		{
			Amt:        chanAmt,
			PushAmt:    pushAmt,
			UseBaseFee: false,
			UseFeeRate: false,
		},
		{
			Amt:        chanAmt,
			PushAmt:    pushAmt,
			BaseFee:    optionalBaseFee,
			UseBaseFee: true,
			UseFeeRate: false,
		},
		{
			Amt:        chanAmt,
			PushAmt:    pushAmt,
			FeeRate:    optionalFeeRate,
			UseBaseFee: false,
			UseFeeRate: true,
		},
		{
			Amt:        chanAmt,
			PushAmt:    pushAmt,
			BaseFee:    optionalBaseFee,
			FeeRate:    optionalFeeRate,
			UseBaseFee: true,
			UseFeeRate: true,
		},
		{
			Amt:        chanAmt,
			PushAmt:    pushAmt,
			BaseFee:    lowBaseFee,
			FeeRate:    lowFeeRate,
			UseBaseFee: true,
			UseFeeRate: true,
		},
	}

	expectedPolicies := []lnrpc.RoutingPolicy{
		{
			FeeBaseMsat:      defaultBaseFee,
			FeeRateMilliMsat: defaultFeeRate,
			TimeLockDelta:    defaultTimeLockDelta,
			MinHtlc:          defaultMinHtlc,
			MaxHtlcMsat:      defaultMaxHtlc,
		},
		{
			FeeBaseMsat:      optionalBaseFee,
			FeeRateMilliMsat: defaultFeeRate,
			TimeLockDelta:    defaultTimeLockDelta,
			MinHtlc:          defaultMinHtlc,
			MaxHtlcMsat:      defaultMaxHtlc,
		},
		{
			FeeBaseMsat:      defaultBaseFee,
			FeeRateMilliMsat: optionalFeeRate,
			TimeLockDelta:    defaultTimeLockDelta,
			MinHtlc:          defaultMinHtlc,
			MaxHtlcMsat:      defaultMaxHtlc,
		},
		{
			FeeBaseMsat:      optionalBaseFee,
			FeeRateMilliMsat: optionalFeeRate,
			TimeLockDelta:    defaultTimeLockDelta,
			MinHtlc:          defaultMinHtlc,
			MaxHtlcMsat:      defaultMaxHtlc,
		},
		{
			FeeBaseMsat:      lowBaseFee,
			FeeRateMilliMsat: lowFeeRate,
			TimeLockDelta:    defaultTimeLockDelta,
			MinHtlc:          defaultMinHtlc,
			MaxHtlcMsat:      defaultMaxHtlc,
		},
	}

	bobExpectedPolicy := lnrpc.RoutingPolicy{
		FeeBaseMsat:      defaultBaseFee,
		FeeRateMilliMsat: defaultFeeRate,
		TimeLockDelta:    defaultTimeLockDelta,
		MinHtlc:          defaultMinHtlc,
		MaxHtlcMsat:      defaultMaxHtlc,
	}

	// In this basic test, we'll need a third node, Carol, so we can forward
	// a payment through the channel we'll open with the different fee
	// policies.
	carol := ht.NewNode("Carol", nil)

	alice, bob := ht.Alice, ht.Bob
	nodes := []*node.HarnessNode{alice, bob, carol}

	runTestCase := func(ht *lntest.HarnessTest,
		chanParams lntest.OpenChannelParams,
		alicePolicy, bobPolicy *lnrpc.RoutingPolicy) {

		// Create a channel Alice->Bob.
		chanPoint := ht.OpenChannel(alice, bob, chanParams)
		defer ht.CloseChannel(alice, chanPoint)

		// Create a channel Carol->Alice.
		chanPoint2 := ht.OpenChannel(
			carol, alice, lntest.OpenChannelParams{
				Amt: 500000,
			},
		)
		defer ht.CloseChannel(carol, chanPoint2)

		// Alice and Bob should see each other's ChannelUpdates,
		// advertising the preferred routing policies.
		assertNodesPolicyUpdate(
			ht, nodes, alice, alicePolicy, chanPoint,
		)
		assertNodesPolicyUpdate(ht, nodes, bob, bobPolicy, chanPoint)

		// They should now know about the default policies.
		for _, n := range nodes {
			ht.AssertChannelPolicy(
				n, alice.PubKeyStr, alicePolicy, chanPoint,
			)
			ht.AssertChannelPolicy(
				n, bob.PubKeyStr, bobPolicy, chanPoint,
			)
		}

		// We should be able to forward a payment from Carol to Bob
		// through the new channel we opened.
		payReqs, _, _ := ht.CreatePayReqs(bob, paymentAmt, 1)
		ht.CompletePaymentRequests(carol, payReqs)
	}

	for i, feeScenario := range feeScenarios {
		ht.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			st := ht.Subtest(t)
			st.EnsureConnected(alice, bob)

			st.RestartNode(carol)

			// Because we're using ht.Subtest(), we need to restart
			// any node we have to refresh its runtime context.
			// Otherwise, we'll get a "context canceled" error on
			// RPC calls.
			st.EnsureConnected(alice, carol)

			// Send Carol enough coins to be able to open a channel
			// to Alice.
			ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

			runTestCase(
				st, feeScenario,
				&expectedPolicies[i], &bobExpectedPolicy,
			)
		})
	}
}

// testBasicChannelCreationAndUpdates tests multiple channel opening and
// closing, and ensures that if a node is subscribed to channel updates they
// will be received correctly for both cooperative and force closed channels.
func testBasicChannelCreationAndUpdates(ht *lntest.HarnessTest) {
	runBasicChannelCreationAndUpdates(ht, ht.Alice, ht.Bob)
}

// runBasicChannelCreationAndUpdates tests multiple channel opening and closing,
// and ensures that if a node is subscribed to channel updates they will be
// received correctly for both cooperative and force closed channels.
func runBasicChannelCreationAndUpdates(ht *lntest.HarnessTest,
	alice, bob *node.HarnessNode) {

	const (
		numChannels = 2
		amount      = funding.MaxBtcFundingAmount
	)

	// Subscribe Bob and Alice to channel event notifications.
	bobChanSub := bob.RPC.SubscribeChannelEvents()
	aliceChanSub := alice.RPC.SubscribeChannelEvents()

	// Open the channels between Alice and Bob, asserting that the channels
	// have been properly opened on-chain. We also attach the optional Memo
	// argument to one of the channels so we can test that it can be
	// retrieved correctly when querying the created channel.
	chanPoints := make([]*lnrpc.ChannelPoint, numChannels)
	openChannelParams := []lntest.OpenChannelParams{
		{Amt: amount, Memo: "bob is a good peer"},
		{Amt: amount},
	}
	for i := 0; i < numChannels; i++ {
		chanPoints[i] = ht.OpenChannel(
			alice, bob, openChannelParams[i],
		)
	}

	// Alice should see the memo when retrieving the first channel.
	channel := ht.QueryChannelByChanPoint(alice, chanPoints[0])
	require.Equal(ht, "bob is a good peer", channel.Memo)

	// Bob shouldn't see the memo since it's for Alice only.
	channel = ht.QueryChannelByChanPoint(bob, chanPoints[0])
	require.Empty(ht, channel.Memo, "Memo is not empty")

	// The second channel doesn't have a memo.
	channel = ht.QueryChannelByChanPoint(alice, chanPoints[1])
	require.Empty(ht, channel.Memo, "Memo is not empty")

	// Since each of the channels just became open, Bob and Alice should
	// each receive an open and an active notification for each channel.
	const numExpectedOpenUpdates = 3 * numChannels
	verifyOpenUpdatesReceived := func(sub rpc.ChannelEventsClient) error {
		for i := 0; i < numExpectedOpenUpdates; i++ {
			update := ht.ReceiveChannelEvent(sub)

			switch update.Type {
			case lnrpc.ChannelEventUpdate_PENDING_OPEN_CHANNEL:
				if i%3 == 0 {
					continue
				}

				return fmt.Errorf("expected open or active" +
					"channel ntfn, got pending open " +
					"channel ntfn instead")

			case lnrpc.ChannelEventUpdate_OPEN_CHANNEL:
				if i%3 == 1 {
					continue
				}

				return fmt.Errorf("expected pending open or " +
					"active channel ntfn, got open" +
					"channel ntfn instead")

			case lnrpc.ChannelEventUpdate_ACTIVE_CHANNEL:
				if i%3 == 2 {
					continue
				}

				return fmt.Errorf("expected pending open or " +
					"open channel ntfn, got active " +
					"channel ntfn instead")

			default:
				return fmt.Errorf("update type mismatch: "+
					"expected open or active channel "+
					"notification, got: %v", update.Type)
			}
		}

		return nil
	}

	require.NoError(ht, verifyOpenUpdatesReceived(bobChanSub),
		"bob open channels")
	require.NoError(ht, verifyOpenUpdatesReceived(aliceChanSub),
		"alice open channels")

	// Close the channels between Alice and Bob, asserting that the
	// channels have been properly closed on-chain.
	for i, chanPoint := range chanPoints {
		// Force close the first of the two channels.
		force := i%2 == 0
		if force {
			ht.ForceCloseChannel(alice, chanPoint)
		} else {
			ht.CloseChannel(alice, chanPoint)
		}
	}

	// If Bob now tries to open a channel with an invalid memo, reject it.
	invalidMemo := strings.Repeat("a", 501)
	params := lntest.OpenChannelParams{
		Amt:  funding.MaxBtcFundingAmount,
		Memo: invalidMemo,
	}
	expErr := fmt.Errorf("provided memo (%s) is of length 501, exceeds 500",
		invalidMemo)
	ht.OpenChannelAssertErr(bob, alice, params, expErr)

	// verifyCloseUpdatesReceived is used to verify that Alice and Bob
	// receive the correct channel updates in order.
	const numExpectedCloseUpdates = 3 * numChannels
	verifyCloseUpdatesReceived := func(sub rpc.ChannelEventsClient,
		forceType lnrpc.ChannelCloseSummary_ClosureType,
		closeInitiator lnrpc.Initiator) error {

		// Ensure one inactive and one closed notification is received
		// for each closed channel.
		for i := 0; i < numExpectedCloseUpdates; i++ {
			expectedCloseType := lnrpc.
				ChannelCloseSummary_COOPERATIVE_CLOSE

			// Every other channel should be force closed. If this
			// channel was force closed, set the expected close type
			// to the type passed in.
			force := (i/3)%2 == 0
			if force {
				expectedCloseType = forceType
			}

			chanUpdate := ht.ReceiveChannelEvent(sub)
			err := verifyCloseUpdate(
				chanUpdate, expectedCloseType,
				closeInitiator,
			)
			if err != nil {
				return err
			}
		}

		return nil
	}

	// Verify Bob receives all closed channel notifications. He should
	// receive a remote force close notification for force closed channels.
	// All channels (cooperatively and force closed) should have a remote
	// close initiator because Alice closed the channels.
	require.NoError(
		ht, verifyCloseUpdatesReceived(
			bobChanSub,
			lnrpc.ChannelCloseSummary_REMOTE_FORCE_CLOSE,
			lnrpc.Initiator_INITIATOR_REMOTE,
		), "verifying bob close updates",
	)

	// Verify Alice receives all closed channel notifications. She should
	// receive a remote force close notification for force closed channels.
	// All channels (cooperatively and force closed) should have a local
	// close initiator because Alice closed the channels.
	require.NoError(
		ht, verifyCloseUpdatesReceived(
			aliceChanSub,
			lnrpc.ChannelCloseSummary_LOCAL_FORCE_CLOSE,
			lnrpc.Initiator_INITIATOR_LOCAL,
		), "verifying alice close updates",
	)
}

// testUpdateOnPendingOpenChannels checks that `update_add_htlc` followed by
// `channel_ready` is properly handled. In specific, when a node is in a state
// that it's still processing a remote `channel_ready` message, meanwhile an
// `update_add_htlc` is received, this HTLC message is cached and settled once
// processing `channel_ready` is complete.
func testUpdateOnPendingOpenChannels(ht *lntest.HarnessTest) {
	// Test funder's behavior. Funder sees the channel pending, but fundee
	// sees it active and sends an HTLC.
	ht.Run("pending on funder side", func(t *testing.T) {
		st := ht.Subtest(t)
		testUpdateOnFunderPendingOpenChannels(st)
	})

	// Test fundee's behavior. Fundee sees the channel pending, but funder
	// sees it active and sends an HTLC.
	ht.Run("pending on fundee side", func(t *testing.T) {
		st := ht.Subtest(t)
		testUpdateOnFundeePendingOpenChannels(st)
	})
}

// testUpdateOnFunderPendingOpenChannels checks that when the fundee sends an
// `update_add_htlc` followed by `channel_ready` while the funder is still
// processing the fundee's `channel_ready`, the HTLC will be cached and
// eventually settled.
func testUpdateOnFunderPendingOpenChannels(ht *lntest.HarnessTest) {
	// Grab the channel participants.
	alice, bob := ht.Alice, ht.Bob

	// Restart Alice with the config so she won't process Bob's
	// channel_ready msg immediately.
	ht.RestartNodeWithExtraArgs(alice, []string{
		"--dev.processchannelreadywait=10s",
	})

	// Make sure Alice and Bob are connected.
	ht.EnsureConnected(alice, bob)

	// Create a new channel that requires 1 confs before it's considered
	// open.
	params := lntest.OpenChannelParams{
		Amt:     funding.MaxBtcFundingAmount,
		PushAmt: funding.MaxBtcFundingAmount / 2,
	}
	pendingChan := ht.OpenChannelAssertPending(alice, bob, params)
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: pendingChan.Txid,
		},
		OutputIndex: pendingChan.OutputIndex,
	}

	// Alice and Bob should both consider the channel pending open.
	ht.AssertNumPendingOpenChannels(alice, 1)
	ht.AssertNumPendingOpenChannels(bob, 1)

	// Mine one block to confirm the funding transaction.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// TODO(yy): we've prematurely marked the channel as open before
	// processing channel ready messages. We need to mark it as open after
	// we've processed channel ready messages and change the check to,
	// ht.AssertNumPendingOpenChannels(alice, 1)
	ht.AssertNumPendingOpenChannels(alice, 0)

	// Bob will consider the channel open as there's no wait time to send
	// and receive Alice's channel_ready message.
	ht.AssertNumPendingOpenChannels(bob, 0)

	// Alice and Bob now have different view of the channel. For Bob,
	// since the channel_ready messages are processed, he will have a
	// working link to route HTLCs. For Alice, because she hasn't handled
	// Bob's channel_ready, there's no active link yet.
	//
	// Alice now adds an invoice.
	req := &lnrpc.Invoice{
		RPreimage: ht.Random32Bytes(),
		Value:     10_000,
	}
	invoice := alice.RPC.AddInvoice(req)

	// Bob sends an `update_add_htlc`, which would result in this message
	// being cached in Alice's `peer.Brontide` and the payment will stay
	// in-flight instead of being failed by Alice.
	bobReq := &routerrpc.SendPaymentRequest{
		PaymentRequest: invoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	bobStream := bob.RPC.SendPayment(bobReq)
	ht.AssertPaymentStatusFromStream(bobStream, lnrpc.Payment_IN_FLIGHT)

	// Wait until Alice finishes processing Bob's channel_ready.
	//
	// NOTE: no effect before fixing the above TODO.
	ht.AssertNumPendingOpenChannels(alice, 0)

	// Once Alice sees the channel as active, she will process the cached
	// premature `update_add_htlc` and settles the payment.
	ht.AssertPaymentStatusFromStream(bobStream, lnrpc.Payment_SUCCEEDED)

	// Close the channel.
	ht.CloseChannel(alice, chanPoint)
}

// testUpdateOnFundeePendingOpenChannels checks that when the funder sends an
// `update_add_htlc` followed by `channel_ready` while the fundee is still
// processing the funder's `channel_ready`, the HTLC will be cached and
// eventually settled.
func testUpdateOnFundeePendingOpenChannels(ht *lntest.HarnessTest) {
	// Grab the channel participants.
	alice, bob := ht.Alice, ht.Bob

	// Restart Bob with the config so he won't process Alice's
	// channel_ready msg immediately.
	ht.RestartNodeWithExtraArgs(bob, []string{
		"--dev.processchannelreadywait=10s",
	})

	// Make sure Alice and Bob are connected.
	ht.EnsureConnected(alice, bob)

	// Create a new channel that requires 1 confs before it's considered
	// open.
	params := lntest.OpenChannelParams{
		Amt: funding.MaxBtcFundingAmount,
	}
	pendingChan := ht.OpenChannelAssertPending(alice, bob, params)
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: pendingChan.Txid,
		},
		OutputIndex: pendingChan.OutputIndex,
	}

	// Alice and Bob should both consider the channel pending open.
	ht.AssertNumPendingOpenChannels(alice, 1)
	ht.AssertNumPendingOpenChannels(bob, 1)

	// Mine one block to confirm the funding transaction.
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// Alice will consider the channel open as there's no wait time to send
	// and receive Bob's channel_ready message.
	ht.AssertNumPendingOpenChannels(alice, 0)

	// TODO(yy): we've prematurely marked the channel as open before
	// processing channel ready messages. We need to mark it as open after
	// we've processed channel ready messages and change the check to,
	// ht.AssertNumPendingOpenChannels(bob, 1)
	ht.AssertNumPendingOpenChannels(bob, 0)

	// Alice and Bob now have different view of the channel. For Alice,
	// since the channel_ready messages are processed, she will have a
	// working link to route HTLCs. For Bob, because he hasn't handled
	// Alice's channel_ready, there's no active link yet.
	//
	// Bob now adds an invoice.
	req := &lnrpc.Invoice{
		RPreimage: ht.Random32Bytes(),
		Value:     10_000,
	}
	bobInvoice := bob.RPC.AddInvoice(req)

	// Alice sends an `update_add_htlc`, which would result in this message
	// being cached in Bob's `peer.Brontide` and the payment will stay
	// in-flight instead of being failed by Bob.
	aliceReq := &routerrpc.SendPaymentRequest{
		PaymentRequest: bobInvoice.PaymentRequest,
		TimeoutSeconds: 60,
		FeeLimitMsat:   noFeeLimitMsat,
	}
	aliceStream := alice.RPC.SendPayment(aliceReq)
	ht.AssertPaymentStatusFromStream(aliceStream, lnrpc.Payment_IN_FLIGHT)

	// Wait until Bob finishes processing Alice's channel_ready.
	//
	// NOTE: no effect before fixing the above TODO.
	ht.AssertNumPendingOpenChannels(bob, 0)

	// Once Bob sees the channel as active, he will process the cached
	// premature `update_add_htlc` and settles the payment.
	ht.AssertPaymentStatusFromStream(aliceStream, lnrpc.Payment_SUCCEEDED)

	// Close the channel.
	ht.CloseChannel(alice, chanPoint)
}

// verifyCloseUpdate is used to verify that a closed channel update is of the
// expected type.
func verifyCloseUpdate(chanUpdate *lnrpc.ChannelEventUpdate,
	closeType lnrpc.ChannelCloseSummary_ClosureType,
	closeInitiator lnrpc.Initiator) error {

	// We should receive one inactive and one closed notification
	// for each channel.
	switch update := chanUpdate.Channel.(type) {
	case *lnrpc.ChannelEventUpdate_InactiveChannel:
		if chanUpdate.Type !=
			lnrpc.ChannelEventUpdate_INACTIVE_CHANNEL {

			return fmt.Errorf("update type mismatch: "+
				"expected %v, got %v",
				lnrpc.ChannelEventUpdate_INACTIVE_CHANNEL,
				chanUpdate.Type)
		}

	case *lnrpc.ChannelEventUpdate_ClosedChannel:
		if chanUpdate.Type !=
			lnrpc.ChannelEventUpdate_CLOSED_CHANNEL {

			return fmt.Errorf("update type mismatch: "+
				"expected %v, got %v",
				lnrpc.ChannelEventUpdate_CLOSED_CHANNEL,
				chanUpdate.Type)
		}

		if update.ClosedChannel.CloseType != closeType {
			return fmt.Errorf("channel closure type "+
				"mismatch: expected %v, got %v",
				closeType,
				update.ClosedChannel.CloseType)
		}

		if update.ClosedChannel.CloseInitiator != closeInitiator {
			return fmt.Errorf("expected close intiator: %v, "+
				"got: %v", closeInitiator,
				update.ClosedChannel.CloseInitiator)
		}

	case *lnrpc.ChannelEventUpdate_FullyResolvedChannel:
		if chanUpdate.Type !=
			lnrpc.ChannelEventUpdate_FULLY_RESOLVED_CHANNEL {

			return fmt.Errorf("update type mismatch: "+
				"expected %v, got %v",
				lnrpc.ChannelEventUpdate_FULLY_RESOLVED_CHANNEL,
				chanUpdate.Type)
		}

	default:
		return fmt.Errorf("channel update channel of wrong type, "+
			"expected closed channel, got %T",
			update)
	}

	return nil
}

// testFundingExpiryBlocksOnPending checks that after an OpenChannel, and
// before the funding transaction is confirmed, that the FundingExpiryBlocks
// field of a PendingChannels decreases.
func testFundingExpiryBlocksOnPending(ht *lntest.HarnessTest) {
	alice, bob := ht.Alice, ht.Bob
	param := lntest.OpenChannelParams{Amt: 100000}
	update := ht.OpenChannelAssertPending(alice, bob, param)

	// At this point, the channel's funding transaction will have been
	// broadcast, but not confirmed. Alice and Bob's nodes should reflect
	// this when queried via RPC. FundingExpiryBlock should decrease
	// as blocks are mined, until the channel is confirmed. Empty blocks
	// won't confirm the funding transaction, so let's mine a few empty
	// blocks and verify the value of FundingExpiryBlock at each step.
	const numEmptyBlocks = 3
	for i := int32(0); i < numEmptyBlocks; i++ {
		expectedVal := funding.MaxWaitNumBlocksFundingConf - i
		pending := ht.AssertNumPendingOpenChannels(alice, 1)
		require.Equal(ht, expectedVal, pending[0].FundingExpiryBlocks)
		pending = ht.AssertNumPendingOpenChannels(bob, 1)
		require.Equal(ht, expectedVal, pending[0].FundingExpiryBlocks)
		ht.MineEmptyBlocks(1)
	}

	// Mine 1 block to confirm the funding transaction, and then close the
	// channel.
	ht.MineBlocksAndAssertNumTxes(1, 1)
	chanPoint := lntest.ChanPointFromPendingUpdate(update)
	ht.CloseChannel(alice, chanPoint)
}

// testSimpleTaprootChannelActivation ensures that a simple taproot channel is
// active if the initiator disconnects and reconnects in between channel opening
// and channel confirmation.
func testSimpleTaprootChannelActivation(ht *lntest.HarnessTest) {
	simpleTaprootChanArgs := lntest.NodeArgsForCommitType(
		lnrpc.CommitmentType_SIMPLE_TAPROOT,
	)

	// Make the new set of participants.
	alice := ht.NewNode("alice", simpleTaprootChanArgs)
	defer ht.Shutdown(alice)
	bob := ht.NewNode("bob", simpleTaprootChanArgs)
	defer ht.Shutdown(bob)

	ht.FundCoins(btcutil.SatoshiPerBitcoin, alice)

	// Make sure Alice and Bob are connected.
	ht.EnsureConnected(alice, bob)

	// Create simple taproot channel opening parameters.
	params := lntest.OpenChannelParams{
		FundMax:        true,
		CommitmentType: lnrpc.CommitmentType_SIMPLE_TAPROOT,
		Private:        true,
	}

	// Alice opens the channel to Bob.
	pendingChan := ht.OpenChannelAssertPending(alice, bob, params)

	// We'll create the channel point to be able to close the channel once
	// our test is done.
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: pendingChan.Txid,
		},
		OutputIndex: pendingChan.OutputIndex,
	}

	// We disconnect and reconnect Alice and Bob before the channel is
	// confirmed. Our expectation is that the channel is active once the
	// channel is confirmed.
	ht.DisconnectNodes(alice, bob)
	ht.EnsureConnected(alice, bob)

	// Mine six blocks to confirm the channel funding transaction.
	ht.MineBlocksAndAssertNumTxes(6, 1)

	// Verify that Alice sees an active channel to Bob.
	ht.AssertChannelActive(alice, chanPoint)

	// Our test is done and Alice closes her channel to Bob.
	ht.CloseChannel(alice, chanPoint)
}
