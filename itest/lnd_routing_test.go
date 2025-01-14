package itest

import (
	"encoding/hex"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

var sendToRouteTestCases = []*lntest.TestCase{
	{
		Name: "single hop with sync",
		TestFunc: func(ht *lntest.HarnessTest) {
			// useStream: false, routerrpc: false.
			testSingleHopSendToRouteCase(ht, false, false)
		},
	},
	{
		Name: "single hop with stream",
		TestFunc: func(ht *lntest.HarnessTest) {
			// useStream: true, routerrpc: false.
			testSingleHopSendToRouteCase(ht, true, false)
		},
	},
	{
		Name: "single hop with v2",
		TestFunc: func(ht *lntest.HarnessTest) {
			// useStream: false, routerrpc: true.
			testSingleHopSendToRouteCase(ht, false, true)
		},
	},
}

// testSingleHopSendToRouteCase tests that payments are properly processed
// through a provided route with a single hop. We'll create the following
// network topology:
//
//	Carol --100k--> Dave
//
// We'll query the daemon for routes from Carol to Dave and then send payments
// by feeding the route back into the various SendToRoute RPC methods. Here we
// test all three SendToRoute endpoints, forcing each to perform both a regular
// payment and an MPP payment.
func testSingleHopSendToRouteCase(ht *lntest.HarnessTest,
	useStream, useRPC bool) {

	const chanAmt = btcutil.Amount(100000)
	const paymentAmtSat = 1000
	const numPayments = 5
	const amountPaid = int64(numPayments * paymentAmtSat)

	// Create Carol and Dave, then establish a channel between them. Carol
	// is the sole funder of the channel with 100k satoshis. The network
	// topology should look like:
	// Carol -> 100k -> Dave
	carol := ht.NewNode("Carol", nil)
	dave := ht.NewNode("Dave", nil)

	ht.ConnectNodes(carol, dave)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// Open a channel with 100k satoshis between Carol and Dave with Carol
	// being the sole funder of the channel.
	chanPointCarol := ht.OpenChannel(
		carol, dave, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Create invoices for Dave, which expect a payment from Carol.
	payReqs, rHashes, _ := ht.CreatePayReqs(
		dave, paymentAmtSat, numPayments,
	)

	// Reconstruct payment addresses.
	var payAddrs [][]byte
	for _, payReq := range payReqs {
		resp := dave.RPC.DecodePayReq(payReq)
		payAddrs = append(payAddrs, resp.PaymentAddr)
	}

	// Assert Carol and Dave are synced to the chain before proceeding, to
	// ensure the queried route will have a valid final CLTV once the HTLC
	// reaches Dave.
	minerHeight := int32(ht.CurrentHeight())
	ht.WaitForNodeBlockHeight(carol, minerHeight)
	ht.WaitForNodeBlockHeight(dave, minerHeight)

	// Query for routes to pay from Carol to Dave using the default CLTV
	// config.
	routesReq := &lnrpc.QueryRoutesRequest{
		PubKey: dave.PubKeyStr,
		Amt:    paymentAmtSat,
	}
	routes := carol.RPC.QueryRoutes(routesReq)

	// There should only be one route to try, so take the first item.
	r := routes.Routes[0]

	// Construct a closure that will set MPP fields on the route, which
	// allows us to test MPP payments.
	setMPPFields := func(i int) {
		hop := r.Hops[len(r.Hops)-1]
		hop.TlvPayload = true
		hop.MppRecord = &lnrpc.MPPRecord{
			PaymentAddr:  payAddrs[i],
			TotalAmtMsat: paymentAmtSat * 1000,
		}
	}

	// Construct closures for each of the payment types covered:
	//  - main rpc server sync
	//  - main rpc server streaming
	//  - routerrpc server sync
	sendToRouteSync := func() {
		for i, rHash := range rHashes {
			setMPPFields(i)

			sendReq := &lnrpc.SendToRouteRequest{
				PaymentHash: rHash,
				Route:       r,
			}
			resp := carol.RPC.SendToRouteSync(sendReq)
			require.Emptyf(ht, resp.PaymentError,
				"received payment error from %s: %v",
				carol.Name(), resp.PaymentError)
		}
	}
	sendToRouteStream := func() {
		alicePayStream := carol.RPC.SendToRoute()

		for i, rHash := range rHashes {
			setMPPFields(i)

			sendReq := &lnrpc.SendToRouteRequest{
				PaymentHash: rHash,
				Route:       routes.Routes[0],
			}
			err := alicePayStream.Send(sendReq)
			require.NoError(ht, err, "unable to send payment")

			resp, err := ht.ReceiveSendToRouteUpdate(alicePayStream)
			require.NoError(ht, err, "unable to receive stream")
			require.Emptyf(ht, resp.PaymentError,
				"received payment error from %s: %v",
				carol.Name(), resp.PaymentError)
		}
	}
	sendToRouteRouterRPC := func() {
		for i, rHash := range rHashes {
			setMPPFields(i)

			sendReq := &routerrpc.SendToRouteRequest{
				PaymentHash: rHash,
				Route:       r,
			}
			resp := carol.RPC.SendToRouteV2(sendReq)
			require.Nilf(ht, resp.Failure, "received payment "+
				"error from %s", carol.Name())
		}
	}

	// Using Carol as the node as the source, send the payments
	// synchronously via the routerrpc's SendToRoute, or via the main RPC
	// server's SendToRoute streaming or sync calls.
	switch {
	case !useRPC && useStream:
		sendToRouteStream()
	case !useRPC && !useStream:
		sendToRouteSync()
	case useRPC && !useStream:
		sendToRouteRouterRPC()
	default:
		require.Fail(ht, "routerrpc does not support "+
			"streaming send_to_route")
	}

	// Verify that the payment's from Carol's PoV have the correct payment
	// hash and amount.
	payments := ht.AssertNumPayments(carol, numPayments)

	for i, p := range payments {
		// Assert that the payment hashes for each payment match up.
		rHashHex := hex.EncodeToString(rHashes[i])
		require.Equalf(ht, rHashHex, p.PaymentHash,
			"incorrect payment hash for payment %d", i)

		// Assert that each payment has no invoice since the payment
		// was completed using SendToRoute.
		require.Emptyf(ht, p.PaymentRequest,
			"incorrect payment request for payment: %d", i)

		// Assert the payment amount is correct.
		require.EqualValues(ht, paymentAmtSat, p.ValueSat,
			"incorrect payment amt for payment %d, ", i)

		// Assert exactly one htlc was made.
		require.Lenf(ht, p.Htlcs, 1,
			"expected 1 htlc for payment %d", i)

		// Assert the htlc's route is populated.
		htlc := p.Htlcs[0]
		require.NotNilf(ht, htlc.Route,
			"expected route for payment %d", i)

		// Assert the hop has exactly one hop.
		require.Lenf(ht, htlc.Route.Hops, 1,
			"expected 1 hop for payment %d", i)

		// If this is an MPP test, assert the MPP record's fields are
		// properly populated. Otherwise the hop should not have an MPP
		// record.
		hop := htlc.Route.Hops[0]
		require.NotNilf(ht, hop.MppRecord,
			"expected mpp record for mpp payment %d", i)

		require.EqualValues(ht, paymentAmtSat*1000,
			hop.MppRecord.TotalAmtMsat,
			"incorrect mpp total msat for payment %d", i)

		expAddr := payAddrs[i]
		require.Equal(ht, expAddr, hop.MppRecord.PaymentAddr,
			"incorrect mpp payment addr for payment %d ", i)
	}

	// Verify that the invoices's from Dave's PoV have the correct payment
	// hash and amount.
	invoices := ht.AssertNumInvoices(dave, numPayments)

	for i, inv := range invoices {
		// Assert that the payment hashes match up.
		require.Equal(ht, rHashes[i], inv.RHash,
			"incorrect payment hash for invoice %d", i)

		// Assert that the amount paid to the invoice is correct.
		require.EqualValues(ht, paymentAmtSat, inv.AmtPaidSat,
			"incorrect payment amt for invoice %d, ", i)
	}

	// At this point all the channels within our proto network should be
	// shifted by 5k satoshis in the direction of Dave, the sink within the
	// payment flow generated above. The order of asserts corresponds to
	// increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Carol->Dave, order is Dave and then Carol.
	ht.AssertAmountPaid("Carol(local) => Dave(remote)", dave,
		chanPointCarol, int64(0), amountPaid)
	ht.AssertAmountPaid("Carol(local) => Dave(remote)", carol,
		chanPointCarol, amountPaid, int64(0))
}

// testMultiHopSendToRoute tests that payments are properly processed
// through a provided route. We'll create the following network topology:
//
//	Alice --100k--> Bob --100k--> Carol
//
// We'll query the daemon for routes from Alice to Carol and then
// send payments through the routes.
func testMultiHopSendToRoute(ht *lntest.HarnessTest) {
	ht.Run("with cache", func(tt *testing.T) {
		st := ht.Subtest(tt)
		runMultiHopSendToRoute(st, true)
	})
	if *dbBackendFlag == "bbolt" {
		ht.Run("without cache", func(tt *testing.T) {
			st := ht.Subtest(tt)
			runMultiHopSendToRoute(st, false)
		})
	}
}

// runMultiHopSendToRoute tests that payments are properly processed
// through a provided route. We'll create the following network topology:
//
//	Alice --100k--> Bob --100k--> Carol
//
// We'll query the daemon for routes from Alice to Carol and then
// send payments through the routes.
func runMultiHopSendToRoute(ht *lntest.HarnessTest, useGraphCache bool) {
	var opts []string
	if !useGraphCache {
		opts = append(opts, "--db.no-graph-cache")
	}

	alice := ht.NewNodeWithCoins("Alice", opts)
	bob := ht.NewNodeWithCoins("Bob", opts)
	ht.EnsureConnected(alice, bob)

	const chanAmt = btcutil.Amount(100000)

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	chanPointAlice := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Create Carol and establish a channel from Bob. Bob is the sole
	// funder of the channel with 100k satoshis. The network topology
	// should look like:
	// Alice -> Bob -> Carol
	carol := ht.NewNode("Carol", nil)
	ht.ConnectNodes(carol, bob)

	chanPointBob := ht.OpenChannel(
		bob, carol, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Make sure Alice knows the channel between Bob and Carol.
	ht.AssertChannelInGraph(alice, chanPointBob)

	// Create 5 invoices for Carol, which expect a payment from Alice for
	// 1k satoshis with a different preimage each time.
	const (
		numPayments = 5
		paymentAmt  = 1000
	)
	_, rHashes, invoices := ht.CreatePayReqs(carol, paymentAmt, numPayments)

	// Construct a route from Alice to Carol for each of the invoices
	// created above.  We set FinalCltvDelta to 40 since by default
	// QueryRoutes returns the last hop with a final cltv delta of 9 where
	// as the default in htlcswitch is 40.
	routesReq := &lnrpc.QueryRoutesRequest{
		PubKey:         carol.PubKeyStr,
		Amt:            paymentAmt,
		FinalCltvDelta: chainreg.DefaultBitcoinTimeLockDelta,
	}
	routes := alice.RPC.QueryRoutes(routesReq)

	// Using Alice as the source, pay to the 5 invoices from Carol created
	// above.
	for i, rHash := range rHashes {
		// Manually set the MPP payload a new for each payment since
		// the payment addr will change with each invoice, although we
		// can re-use the route itself.
		route := routes.Routes[0]
		route.Hops[len(route.Hops)-1].TlvPayload = true
		route.Hops[len(route.Hops)-1].MppRecord = &lnrpc.MPPRecord{
			PaymentAddr: invoices[i].PaymentAddr,
			TotalAmtMsat: int64(
				lnwire.NewMSatFromSatoshis(paymentAmt),
			),
		}

		sendReq := &routerrpc.SendToRouteRequest{
			PaymentHash: rHash,
			Route:       route,
		}
		resp := alice.RPC.SendToRouteV2(sendReq)
		require.Nil(ht, resp.Failure, "received payment error")
	}

	// When asserting the amount of satoshis moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.
	const baseFee = 1

	// At this point all the channels within our proto network should be
	// shifted by 5k satoshis in the direction of Carol, the sink within
	// the payment flow generated above. The order of asserts corresponds
	// to increasing of time is needed to embed the HTLC in commitment
	// transaction, in channel Alice->Bob->Carol, order is Carol, Bob,
	// Alice.
	const amountPaid = int64(5000)
	ht.AssertAmountPaid("Bob(local) => Carol(remote)", carol,
		chanPointBob, int64(0), amountPaid)
	ht.AssertAmountPaid("Bob(local) => Carol(remote)", bob,
		chanPointBob, amountPaid, int64(0))
	ht.AssertAmountPaid("Alice(local) => Bob(remote)", bob,
		chanPointAlice, int64(0), amountPaid+(baseFee*numPayments))
	ht.AssertAmountPaid("Alice(local) => Bob(remote)", alice,
		chanPointAlice, amountPaid+(baseFee*numPayments), int64(0))
}

// testSendToRouteErrorPropagation tests propagation of errors that occur
// while processing a multi-hop payment through an unknown route.
func testSendToRouteErrorPropagation(ht *lntest.HarnessTest) {
	const chanAmt = btcutil.Amount(100000)

	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNode("Bob", nil)
	ht.EnsureConnected(alice, bob)

	ht.OpenChannel(alice, bob, lntest.OpenChannelParams{Amt: chanAmt})

	// Create a new nodes (Carol and Charlie), load her with some funds,
	// then establish a connection between Carol and Charlie with a channel
	// that has identical capacity to the one created above.Then we will
	// get route via queryroutes call which will be fake route for Alice ->
	// Bob graph.
	//
	// The network topology should now look like:
	// Alice -> Bob; Carol -> Charlie.
	carol := ht.NewNode("Carol", nil)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	charlie := ht.NewNode("Charlie", nil)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, charlie)

	ht.ConnectNodes(carol, charlie)
	ht.OpenChannel(carol, charlie, lntest.OpenChannelParams{Amt: chanAmt})

	// Query routes from Carol to Charlie which will be an invalid route
	// for Alice -> Bob.
	fakeReq := &lnrpc.QueryRoutesRequest{
		PubKey: charlie.PubKeyStr,
		Amt:    int64(1),
	}
	fakeRoute := carol.RPC.QueryRoutes(fakeReq)

	// Create 1 invoice for Bob, which expect a payment from Alice for 1k
	// satoshis.
	const paymentAmt = 1000

	invoice := &lnrpc.Invoice{
		Memo:  "testing",
		Value: paymentAmt,
	}
	resp := bob.RPC.AddInvoice(invoice)
	rHash := resp.RHash

	// Using Alice as the source, pay to the invoice from Bob.
	alicePayStream := alice.RPC.SendToRoute()

	sendReq := &lnrpc.SendToRouteRequest{
		PaymentHash: rHash,
		Route:       fakeRoute.Routes[0],
	}
	err := alicePayStream.Send(sendReq)
	require.NoError(ht, err, "unable to send payment")

	// At this place we should get an rpc error with notification
	// that edge is not found on hop(0)
	event, err := ht.ReceiveSendToRouteUpdate(alicePayStream)
	require.NoError(ht, err, "payment stream has been closed but fake "+
		"route has consumed")
	require.Contains(ht, event.PaymentError, "UnknownNextPeer")
}

// testPrivateChannels tests that a private channel can be used for
// routing by the two endpoints of the channel, but is not known by
// the rest of the nodes in the graph.
func testPrivateChannels(ht *lntest.HarnessTest) {
	const chanAmt = btcutil.Amount(100000)

	// We create the following topology:
	//
	// Dave --100k--> Alice --200k--> Bob
	//  ^		    ^
	//  |		    |
	// 100k		   100k
	//  |		    |
	//  +---- Carol ----+
	//
	// where the 100k channel between Carol and Alice is private.

	// Open a channel with 200k satoshis between Alice and Bob.
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNode("Bob", nil)
	ht.EnsureConnected(alice, bob)

	chanPointAlice := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: chanAmt * 2},
	)

	// Create Dave, and a channel to Alice of 100k.
	dave := ht.NewNode("Dave", nil)
	ht.ConnectNodes(dave, alice)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, dave)

	chanPointDave := ht.OpenChannel(
		dave, alice, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Next, we'll create Carol and establish a channel from her to
	// Dave of 100k.
	carol := ht.NewNode("Carol", nil)
	ht.ConnectNodes(carol, dave)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	chanPointCarol := ht.OpenChannel(
		carol, dave, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Now create a _private_ channel directly between Carol and
	// Alice of 100k.
	ht.ConnectNodes(carol, alice)
	chanPointPrivate := ht.OpenChannel(
		carol, alice, lntest.OpenChannelParams{
			Amt:     chanAmt,
			Private: true,
		},
	)

	// The channel should be available for payments between Carol and Alice.
	// We check this by sending payments from Carol to Bob, that
	// collectively would deplete at least one of Carol's channels.

	// Create 2 invoices for Bob, each of 70k satoshis. Since each of
	// Carol's channels is of size 100k, these payments cannot succeed
	// by only using one of the channels.
	const numPayments = 2
	const paymentAmt = 70000
	payReqs, _, _ := ht.CreatePayReqs(bob, paymentAmt, numPayments)

	// Let Carol pay the invoices.
	ht.CompletePaymentRequests(carol, payReqs)

	// When asserting the amount of satoshis moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when
	// creating the seed nodes in the network.
	const baseFee = 1

	// Bob should have received 140k satoshis from Alice.
	ht.AssertAmountPaid("Alice(local) => Bob(remote)", bob,
		chanPointAlice, int64(0), 2*paymentAmt)

	// Alice sent 140k to Bob.
	ht.AssertAmountPaid("Alice(local) => Bob(remote)", alice,
		chanPointAlice, 2*paymentAmt, int64(0))

	// Alice received 70k + fee from Dave.
	ht.AssertAmountPaid("Dave(local) => Alice(remote)", alice,
		chanPointDave, int64(0), paymentAmt+baseFee)

	// Dave sent 70k+fee to Alice.
	ht.AssertAmountPaid("Dave(local) => Alice(remote)", dave,
		chanPointDave, paymentAmt+baseFee, int64(0))

	// Dave received 70k+fee of two hops from Carol.
	ht.AssertAmountPaid("Carol(local) => Dave(remote)", dave,
		chanPointCarol, int64(0), paymentAmt+baseFee*2)

	// Carol sent 70k+fee of two hops to Dave.
	ht.AssertAmountPaid("Carol(local) => Dave(remote)", carol,
		chanPointCarol, paymentAmt+baseFee*2, int64(0))

	// Alice received 70k+fee from Carol.
	ht.AssertAmountPaid("Carol(local) [private=>] Alice(remote)",
		alice, chanPointPrivate, int64(0), paymentAmt+baseFee)

	// Carol sent 70k+fee to Alice.
	ht.AssertAmountPaid("Carol(local) [private=>] Alice(remote)",
		carol, chanPointPrivate, paymentAmt+baseFee, int64(0))

	// Alice should also be able to route payments using this channel,
	// so send two payments of 60k back to Carol.
	const paymentAmt60k = 60000
	payReqs, _, _ = ht.CreatePayReqs(carol, paymentAmt60k, numPayments)

	// Let Bob pay the invoices.
	ht.CompletePaymentRequests(alice, payReqs)

	// Carol and Alice should know about 4, while Bob and Dave should only
	// know about 3, since one channel is private.
	ht.AssertNumEdges(alice, 4, true)
	ht.AssertNumEdges(alice, 3, false)
	ht.AssertNumEdges(bob, 3, true)
	ht.AssertNumEdges(carol, 4, true)
	ht.AssertNumEdges(carol, 3, false)
	ht.AssertNumEdges(dave, 3, true)
}

// testInvoiceRoutingHints tests that the routing hints for an invoice are
// created properly.
func testInvoiceRoutingHints(ht *lntest.HarnessTest) {
	const chanAmt = btcutil.Amount(100000)

	// Throughout this test, we'll be opening a channel between Alice and
	// several other parties.
	//
	// First, we'll create a private channel between Alice and Bob. This
	// will be the only channel that will be considered as a routing hint
	// throughout this test. We'll include a push amount since we currently
	// require channels to have enough remote balance to cover the
	// invoice's payment.
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)
	ht.EnsureConnected(alice, bob)

	chanPointBob := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: chanAmt / 2,
			Private: true,
		},
	)

	// Then, we'll create Carol's node and open a public channel between
	// her and Alice. This channel will not be considered as a routing hint
	// due to it being public.
	carol := ht.NewNode("Carol", nil)

	ht.ConnectNodes(alice, carol)
	ht.OpenChannel(
		alice, carol, lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: chanAmt / 2,
		},
	)

	// We'll also create a public channel between Bob and Carol to ensure
	// that Bob gets selected as the only routing hint. We do this as we
	// should only include routing hints for nodes that are publicly
	// advertised, otherwise we'd end up leaking information about nodes
	// that wish to stay unadvertised.
	ht.ConnectNodes(bob, carol)
	ht.OpenChannel(
		bob, carol, lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: chanAmt / 2,
		},
	)

	// Then, we'll create Dave's node and open a private channel between
	// him and Alice. We will not include a push amount in order to not
	// consider this channel as a routing hint as it will not have enough
	// remote balance for the invoice's amount.
	dave := ht.NewNode("Dave", nil)

	ht.ConnectNodes(alice, dave)
	ht.OpenChannel(
		alice, dave, lntest.OpenChannelParams{
			Amt:     chanAmt,
			Private: true,
		},
	)

	// Finally, we'll create Eve's node and open a private channel between
	// her and Alice. This time though, we'll take Eve's node down after
	// the channel has been created to avoid populating routing hints for
	// inactive channels.
	eve := ht.NewNode("Eve", nil)
	ht.ConnectNodes(alice, eve)
	ht.OpenChannel(
		alice, eve, lntest.OpenChannelParams{
			Amt:     chanAmt,
			PushAmt: chanAmt / 2,
			Private: true,
		},
	)

	// Now that the channels are open, we'll disconnect the connection
	// between Alice and Eve and then take down Eve's node.
	ht.DisconnectNodes(alice, eve)
	ht.Shutdown(eve)

	// We'll need the short channel ID of the channel between Alice and Bob
	// to make sure the routing hint is for this channel.
	aliceBobChanID := ht.QueryChannelByChanPoint(alice, chanPointBob).ChanId

	checkInvoiceHints := func(invoice *lnrpc.Invoice) {
		// Due to the way the channels were set up above, the channel
		// between Alice and Bob should be the only channel used as a
		// routing hint.
		var decoded *lnrpc.PayReq
		err := wait.NoError(func() error {
			resp := alice.RPC.AddInvoice(invoice)

			// We'll decode the invoice's payment request to
			// determine which channels were used as routing hints.
			decoded = alice.RPC.DecodePayReq(resp.PaymentRequest)

			if len(decoded.RouteHints) != 1 {
				return fmt.Errorf("expected one route hint, "+
					"got %d", len(decoded.RouteHints))
			}

			return nil
		}, defaultTimeout)
		require.NoError(ht, err, "timeout checking invoice hints")

		hops := decoded.RouteHints[0].HopHints
		require.Len(ht, hops, 1, "expected one hop in route hint")

		chanID := hops[0].ChanId
		require.Equal(ht, aliceBobChanID, chanID, "chanID mismatch")
	}

	// Create an invoice for Alice that will populate the routing hints.
	invoice := &lnrpc.Invoice{
		Memo:    "routing hints",
		Value:   int64(chanAmt / 4),
		Private: true,
	}
	checkInvoiceHints(invoice)

	// Create another invoice for Alice with no value and ensure it still
	// populates routing hints.
	invoice = &lnrpc.Invoice{
		Memo:    "routing hints with no amount",
		Value:   0,
		Private: true,
	}
	checkInvoiceHints(invoice)
}

// testScidAliasRoutingHints tests that dynamically created aliases via the RPC
// are properly used when routing.
func testScidAliasRoutingHints(ht *lntest.HarnessTest) {
	bob := ht.NewNodeWithCoins("Bob", nil)

	const chanAmt = btcutil.Amount(800000)

	// Option-scid-alias is opt-in, as is anchors.
	scidAliasArgs := []string{
		"--protocol.option-scid-alias",
		"--protocol.anchors",
	}

	// We'll have a network Bob -> Carol -> Dave in the end, with both Carol
	// and Dave having an alias for the channel between them.
	carol := ht.NewNode("Carol", scidAliasArgs)
	dave := ht.NewNode("Dave", scidAliasArgs)

	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, dave)

	ht.ConnectNodes(carol, dave)

	// Create a channel between Carol and Dave, which uses the scid alias
	// feature.
	chanPointCD := ht.OpenChannel(carol, dave, lntest.OpenChannelParams{
		Amt:       chanAmt,
		PushAmt:   chanAmt / 2,
		ScidAlias: true,
		Private:   true,
	})

	// Find the channel ID of the channel between Carol and Dave.
	carolDaveChan := ht.QueryChannelByChanPoint(carol, chanPointCD)

	// Make sure we can't add an alias that's not actually in the alias
	// range (which is the case with the base SCID).
	err := carol.RPC.XAddLocalChanAliasesErr(&routerrpc.AddAliasesRequest{
		AliasMaps: []*lnrpc.AliasMap{
			{
				BaseScid: carolDaveChan.ChanId,
				Aliases: []uint64{
					carolDaveChan.ChanId,
				},
			},
		},
	})
	require.ErrorContains(ht, err, routerrpc.ErrNoValidAlias.Error())

	// Create an ephemeral alias that will be used as a routing hint.
	ephemeralChanPoint := lnwire.ShortChannelID{
		BlockHeight: 16_100_000,
		TxIndex:     1,
		TxPosition:  1,
	}
	ephemeralAlias := ephemeralChanPoint.ToUint64()
	ephemeralAliasMap := []*lnrpc.AliasMap{
		{
			BaseScid: carolDaveChan.ChanId,
			Aliases: []uint64{
				ephemeralAlias,
			},
		},
	}

	// Add the alias to Carol.
	carol.RPC.XAddLocalChanAliases(&routerrpc.AddAliasesRequest{
		AliasMaps: ephemeralAliasMap,
	})

	// We shouldn't be able to add the same alias again.
	err = carol.RPC.XAddLocalChanAliasesErr(&routerrpc.AddAliasesRequest{
		AliasMaps: ephemeralAliasMap,
	})
	require.ErrorContains(ht, err, routerrpc.ErrAliasAlreadyExists.Error())

	// Add the alias to Dave. This isn't strictly needed for the test, as
	// the payment will go from Bob -> Carol -> Dave, so only Carol needs
	// to know about the alias. So we'll later on remove it again to
	// demonstrate that.
	dave.RPC.XAddLocalChanAliases(&routerrpc.AddAliasesRequest{
		AliasMaps: ephemeralAliasMap,
	})

	carolChans := carol.RPC.ListChannels(&lnrpc.ListChannelsRequest{})
	require.Len(ht, carolChans.Channels, 1, "expected one channel")

	// Get the alias scids for Carol's channel.
	aliases := carolChans.Channels[0].AliasScids

	// There should be two aliases.
	require.Len(ht, aliases, 2, "expected two aliases")

	// The ephemeral alias should be included.
	require.Contains(
		ht, aliases, ephemeralAlias, "expected ephemeral alias",
	)

	// List Dave's Channels.
	daveChans := dave.RPC.ListChannels(&lnrpc.ListChannelsRequest{})

	require.Len(ht, daveChans.Channels, 1, "expected one channel")

	// Get the alias scids for his channel.
	aliases = daveChans.Channels[0].AliasScids

	// There should be two aliases.
	require.Len(ht, aliases, 2, "expected two aliases")

	// The ephemeral alias should be included.
	require.Contains(
		ht, aliases, ephemeralAlias, "expected ephemeral alias",
	)

	// Now that we've asserted that the alias is properly set up, we'll
	// delete the one for Dave again. The payment should still succeed.
	dave.RPC.XDeleteLocalChanAliases(&routerrpc.DeleteAliasesRequest{
		AliasMaps: ephemeralAliasMap,
	})

	// Connect the existing Bob node with Carol via a public channel.
	ht.ConnectNodes(bob, carol)
	ht.OpenChannel(bob, carol, lntest.OpenChannelParams{
		Amt:     chanAmt,
		PushAmt: chanAmt / 2,
	})

	// Create the hop hint that Dave will use to craft his invoice. The
	// goal here is to define only the ephemeral alias as a hop hint.
	hopHint := &lnrpc.HopHint{
		NodeId: carol.PubKeyStr,
		ChanId: ephemeralAlias,
		FeeBaseMsat: uint32(
			chainreg.DefaultBitcoinBaseFeeMSat,
		),
		FeeProportionalMillionths: uint32(
			chainreg.DefaultBitcoinFeeRate,
		),
		CltvExpiryDelta: chainreg.DefaultBitcoinTimeLockDelta,
	}

	// Define the invoice that Dave will add to his node.
	invoice := &lnrpc.Invoice{
		Memo:  "dynamic alias",
		Value: int64(chanAmt / 4),
		RouteHints: []*lnrpc.RouteHint{
			{
				HopHints: []*lnrpc.HopHint{hopHint},
			},
		},
	}

	// Add the invoice and retrieve the payment request.
	payReq := dave.RPC.AddInvoice(invoice).PaymentRequest

	// Now Alice will try to pay to that payment request.
	timeout := time.Second * 15
	stream := bob.RPC.SendPayment(&routerrpc.SendPaymentRequest{
		PaymentRequest: payReq,
		TimeoutSeconds: int32(timeout.Seconds()),
		FeeLimitSat:    math.MaxInt64,
	})

	// Payment should eventually succeed.
	ht.AssertPaymentSucceedWithTimeout(stream, timeout)

	// Check that Dave's invoice appears as settled.
	invoices := dave.RPC.ListInvoices(&lnrpc.ListInvoiceRequest{})
	require.Len(ht, invoices.Invoices, 1, "expected one invoice")
	require.Equal(ht, invoices.Invoices[0].State, lnrpc.Invoice_SETTLED,
		"expected settled invoice")

	// We'll now delete the alias again, but only on Carol's end. That
	// should be enough to make the payment fail, since she doesn't know
	// about the alias in the hop hint anymore.
	carol.RPC.XDeleteLocalChanAliases(&routerrpc.DeleteAliasesRequest{
		AliasMaps: ephemeralAliasMap,
	})
	payReq2 := dave.RPC.AddInvoice(invoice).PaymentRequest
	stream2 := bob.RPC.SendPayment(&routerrpc.SendPaymentRequest{
		PaymentRequest: payReq2,
		TimeoutSeconds: int32(timeout.Seconds()),
		FeeLimitSat:    math.MaxInt64,
	})
	ht.AssertPaymentStatusFromStream(stream2, lnrpc.Payment_FAILED)
}

// testMultiHopOverPrivateChannels tests that private channels can be used as
// intermediate hops in a route for payments.
func testMultiHopOverPrivateChannels(ht *lntest.HarnessTest) {
	// We'll test that multi-hop payments over private channels work as
	// intended. To do so, we'll create the following topology:
	//         private        public           private
	// Alice <--100k--> Bob <--100k--> Carol <--100k--> Dave
	const chanAmt = btcutil.Amount(100000)

	// First, we'll open a private channel between Alice and Bob with Alice
	// being the funder.
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)
	ht.EnsureConnected(alice, bob)

	chanPointAlice := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt:     chanAmt,
			Private: true,
		},
	)

	// Next, we'll create Carol's node and open a public channel between
	// her and Bob with Bob being the funder.
	carol := ht.NewNodeWithCoins("Carol", nil)
	ht.ConnectNodes(bob, carol)
	chanPointBob := ht.OpenChannel(
		bob, carol, lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// Alice should know the new channel from Bob.
	ht.AssertChannelInGraph(alice, chanPointBob)

	// Next, we'll create Dave's node and open a private channel between
	// him and Carol with Carol being the funder.
	dave := ht.NewNode("Dave", nil)
	ht.ConnectNodes(carol, dave)

	chanPointCarol := ht.OpenChannel(
		carol, dave, lntest.OpenChannelParams{
			Amt:     chanAmt,
			Private: true,
		},
	)

	// Dave should know the channel[Bob<->Carol] from Carol.
	ht.AssertChannelInGraph(dave, chanPointBob)

	// Now that all the channels are set up according to the topology from
	// above, we can proceed to test payments. We'll create an invoice for
	// Dave of 20k satoshis and pay it with Alice. Since there is no public
	// route from Alice to Dave, we'll need to use the private channel
	// between Carol and Dave as a routing hint encoded in the invoice.
	const paymentAmt = 20000

	// Create the invoice for Dave.
	invoice := &lnrpc.Invoice{
		Memo:    "two hopz!",
		Value:   paymentAmt,
		Private: true,
	}
	resp := dave.RPC.AddInvoice(invoice)

	// Let Alice pay the invoice.
	payReqs := []string{resp.PaymentRequest}
	ht.CompletePaymentRequests(alice, payReqs)

	// When asserting the amount of satoshis moved, we'll factor in the
	// default base fee, as we didn't modify the fee structure when opening
	// the channels.
	const baseFee = 1

	// Dave should have received 20k satoshis from Carol.
	ht.AssertAmountPaid("Carol(local) [private=>] Dave(remote)",
		dave, chanPointCarol, 0, paymentAmt)

	// Carol should have sent 20k satoshis to Dave.
	ht.AssertAmountPaid("Carol(local) [private=>] Dave(remote)",
		carol, chanPointCarol, paymentAmt, 0)

	// Carol should have received 20k satoshis + fee for one hop from Bob.
	ht.AssertAmountPaid("Bob(local) => Carol(remote)",
		carol, chanPointBob, 0, paymentAmt+baseFee)

	// Bob should have sent 20k satoshis + fee for one hop to Carol.
	ht.AssertAmountPaid("Bob(local) => Carol(remote)",
		bob, chanPointBob, paymentAmt+baseFee, 0)

	// Bob should have received 20k satoshis + fee for two hops from Alice.
	ht.AssertAmountPaid("Alice(local) [private=>] Bob(remote)", bob,
		chanPointAlice, 0, paymentAmt+baseFee*2)

	// Alice should have sent 20k satoshis + fee for two hops to Bob.
	ht.AssertAmountPaid("Alice(local) [private=>] Bob(remote)", alice,
		chanPointAlice, paymentAmt+baseFee*2, 0)
}

// testQueryRoutes checks the response of queryroutes.
// We'll create the following network topology:
//
//	Alice --> Bob --> Carol --> Dave
//
// and query the daemon for routes from Alice to Dave.
func testQueryRoutes(ht *lntest.HarnessTest) {
	const chanAmt = btcutil.Amount(100000)

	// Grab Alice and Bob from the standby nodes.
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)
	ht.EnsureConnected(alice, bob)

	// Create Carol and connect her to Bob. We also send her some coins for
	// channel opening.
	carol := ht.NewNode("Carol", nil)
	ht.ConnectNodes(carol, bob)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// Create Dave and connect him to Carol.
	dave := ht.NewNode("Dave", nil)
	ht.ConnectNodes(dave, carol)

	// We now proceed to open channels:
	//   Alice=>Bob, Bob=>Carol and Carol=>Dave.
	p := lntest.OpenChannelParams{Amt: chanAmt}
	reqs := []*lntest.OpenChannelRequest{
		{Local: alice, Remote: bob, Param: p},
		{Local: bob, Remote: carol, Param: p},
		{Local: carol, Remote: dave, Param: p},
	}
	resp := ht.OpenMultiChannelsAsync(reqs)

	// Extract channel points from the response.
	chanPointBob := resp[1]
	chanPointCarol := resp[2]

	// Before we continue, give Alice some time to catch up with the newly
	// opened channels.
	ht.AssertChannelInGraph(alice, chanPointBob)
	ht.AssertChannelInGraph(alice, chanPointCarol)

	// Query for routes to pay from Alice to Dave.
	const paymentAmt = 1000
	routesReq := &lnrpc.QueryRoutesRequest{
		PubKey: dave.PubKeyStr,
		Amt:    paymentAmt,
	}
	routesRes := ht.QueryRoutesAndRetry(alice, routesReq)

	const mSat = 1000
	feePerHopMSat := computeFee(1000, 1, paymentAmt*mSat)

	for i, route := range routesRes.Routes {
		expectedTotalFeesMSat :=
			lnwire.MilliSatoshi(len(route.Hops)-1) * feePerHopMSat

		expectedTotalAmtMSat :=
			(paymentAmt * mSat) + expectedTotalFeesMSat

		if route.TotalFees != route.TotalFeesMsat/mSat {
			ht.Fatalf("route %v: total fees %v (msat) does not "+
				"round down to %v (sat)",
				i, route.TotalFeesMsat, route.TotalFees)
		}
		if route.TotalFeesMsat != int64(expectedTotalFeesMSat) {
			ht.Fatalf("route %v: total fees in msat expected %v "+
				"got %v", i, expectedTotalFeesMSat,
				route.TotalFeesMsat)
		}

		if route.TotalAmt != route.TotalAmtMsat/mSat {
			ht.Fatalf("route %v: total amt %v (msat) does not "+
				"round down to %v (sat)",
				i, route.TotalAmtMsat, route.TotalAmt)
		}
		if route.TotalAmtMsat != int64(expectedTotalAmtMSat) {
			ht.Fatalf("route %v: total amt in msat expected %v "+
				"got %v", i, expectedTotalAmtMSat,
				route.TotalAmtMsat)
		}

		// For all hops except the last, we check that fee equals
		// feePerHop and amount to forward deducts feePerHop on each
		// hop.
		expectedAmtToForwardMSat := expectedTotalAmtMSat
		for j, hop := range route.Hops[:len(route.Hops)-1] {
			expectedAmtToForwardMSat -= feePerHopMSat

			if hop.Fee != hop.FeeMsat/mSat {
				ht.Fatalf("route %v hop %v: fee %v (msat) "+
					"does not round down to %v (sat)",
					i, j, hop.FeeMsat, hop.Fee)
			}
			if hop.FeeMsat != int64(feePerHopMSat) {
				ht.Fatalf("route %v hop %v: fee in msat "+
					"expected %v got %v",
					i, j, feePerHopMSat, hop.FeeMsat)
			}

			if hop.AmtToForward != hop.AmtToForwardMsat/mSat {
				ht.Fatalf("route %v hop %v: amt to forward %v"+
					"(msat) does not round down to %v(sat)",
					i, j, hop.AmtToForwardMsat,
					hop.AmtToForward)
			}
			if hop.AmtToForwardMsat !=
				int64(expectedAmtToForwardMSat) {

				ht.Fatalf("route %v hop %v: amt to forward "+
					"in msat expected %v got %v",
					i, j, expectedAmtToForwardMSat,
					hop.AmtToForwardMsat)
			}
		}

		// Last hop should have zero fee and amount to forward should
		// equal payment amount.
		hop := route.Hops[len(route.Hops)-1]

		if hop.Fee != 0 || hop.FeeMsat != 0 {
			ht.Fatalf("route %v hop %v: fee expected 0 got %v "+
				"(sat) %v (msat)", i, len(route.Hops)-1,
				hop.Fee, hop.FeeMsat)
		}

		if hop.AmtToForward != hop.AmtToForwardMsat/mSat {
			ht.Fatalf("route %v hop %v: amt to forward %v (msat) "+
				"does not round down to %v (sat)", i,
				len(route.Hops)-1, hop.AmtToForwardMsat,
				hop.AmtToForward)
		}
		if hop.AmtToForwardMsat != paymentAmt*mSat {
			ht.Fatalf("route %v hop %v: amt to forward in msat "+
				"expected %v got %v", i, len(route.Hops)-1,
				paymentAmt*mSat, hop.AmtToForwardMsat)
		}
	}

	// While we're here, we test updating mission control's config values
	// and assert that they are correctly updated and check that our mission
	// control import function updates appropriately.
	testMissionControlCfg(ht.T, alice)
	testMissionControlImport(ht, alice, bob.PubKey[:], carol.PubKey[:])
}

// testMissionControlCfg tests getting and setting of a node's mission control
// config, resetting to the original values after testing so that no other
// tests are affected.
func testMissionControlCfg(t *testing.T, hn *node.HarnessNode) {
	t.Helper()

	// Getting and setting does not alter the configuration.
	startCfg := hn.RPC.GetMissionControlConfig().Config
	hn.RPC.SetMissionControlConfig(startCfg)
	resp := hn.RPC.GetMissionControlConfig()
	require.True(t, proto.Equal(startCfg, resp.Config))

	// We test that setting and getting leads to the same config if all
	// fields are set.
	cfg := &routerrpc.MissionControlConfig{
		MaximumPaymentResults:       30,
		MinimumFailureRelaxInterval: 60,
		Model: routerrpc.
			MissionControlConfig_APRIORI,
		EstimatorConfig: &routerrpc.MissionControlConfig_Apriori{
			Apriori: &routerrpc.AprioriParameters{
				HalfLifeSeconds:  8000,
				HopProbability:   0.8,
				Weight:           0.3,
				CapacityFraction: 0.8,
			},
		},
	}
	hn.RPC.SetMissionControlConfig(cfg)

	// The deprecated fields should be populated.
	cfg.HalfLifeSeconds = 8000
	cfg.HopProbability = 0.8
	cfg.Weight = 0.3
	respCfg := hn.RPC.GetMissionControlConfig().Config
	require.True(t, proto.Equal(cfg, respCfg))

	// Switching to another estimator is possible.
	cfg = &routerrpc.MissionControlConfig{
		Model: routerrpc.
			MissionControlConfig_BIMODAL,
		EstimatorConfig: &routerrpc.MissionControlConfig_Bimodal{
			Bimodal: &routerrpc.BimodalParameters{
				ScaleMsat: 1_000,
				DecayTime: 500,
			},
		},
	}
	hn.RPC.SetMissionControlConfig(cfg)
	respCfg = hn.RPC.GetMissionControlConfig().Config
	require.NotNil(t, respCfg.GetBimodal())

	// If parameters are not set in the request, they will have zero values
	// after.
	require.Zero(t, respCfg.MaximumPaymentResults)
	require.Zero(t, respCfg.MinimumFailureRelaxInterval)
	require.Zero(t, respCfg.GetBimodal().NodeWeight)

	// Setting deprecated values will initialize the apriori estimator.
	cfg = &routerrpc.MissionControlConfig{
		MaximumPaymentResults:       30,
		MinimumFailureRelaxInterval: 60,
		HopProbability:              0.8,
		Weight:                      0.3,
		HalfLifeSeconds:             8000,
	}
	hn.RPC.SetMissionControlConfig(cfg)
	respCfg = hn.RPC.GetMissionControlConfig().Config
	require.NotNil(t, respCfg.GetApriori())

	// The default capacity fraction is set.
	require.Equal(t, routing.DefaultCapacityFraction,
		respCfg.GetApriori().CapacityFraction)

	// Setting the wrong config results in an error.
	cfg = &routerrpc.MissionControlConfig{
		Model: routerrpc.
			MissionControlConfig_APRIORI,
		EstimatorConfig: &routerrpc.MissionControlConfig_Bimodal{
			Bimodal: &routerrpc.BimodalParameters{
				ScaleMsat: 1_000,
			},
		},
	}
	hn.RPC.SetMissionControlConfigAssertErr(cfg)

	// Undo any changes.
	hn.RPC.SetMissionControlConfig(startCfg)
	resp = hn.RPC.GetMissionControlConfig()
	require.True(t, proto.Equal(startCfg, resp.Config))
}

// testMissionControlImport tests import of mission control results from an
// external source.
func testMissionControlImport(ht *lntest.HarnessTest, hn *node.HarnessNode,
	fromNode, toNode []byte) {

	// Reset mission control so that our query will return the default
	// probability for our first request.
	hn.RPC.ResetMissionControl()

	// Get our baseline probability for a 10 msat hop between our target
	// nodes.
	var amount int64 = 10
	probReq := &routerrpc.QueryProbabilityRequest{
		FromNode: fromNode,
		ToNode:   toNode,
		AmtMsat:  amount,
	}

	importHistory := &routerrpc.PairData{
		FailTime:    time.Now().Unix(),
		FailAmtMsat: amount,
	}

	// Assert that our history is not already equal to the value we want to
	// set. This should not happen because we have just cleared our state.
	resp1 := hn.RPC.QueryProbability(probReq)
	require.Zero(ht, resp1.History.FailTime)
	require.Zero(ht, resp1.History.FailAmtMsat)

	// Now, we import a single entry which tracks a failure of the amount
	// we want to query between our nodes.
	req := &routerrpc.XImportMissionControlRequest{
		Pairs: []*routerrpc.PairHistory{
			{
				NodeFrom: fromNode,
				NodeTo:   toNode,
				History:  importHistory,
			},
		},
	}
	hn.RPC.XImportMissionControl(req)

	resp2 := hn.RPC.QueryProbability(probReq)
	require.Equal(ht, importHistory.FailTime, resp2.History.FailTime)
	require.Equal(ht, importHistory.FailAmtMsat, resp2.History.FailAmtMsat)

	// Finally, check that we will fail if inconsistent sat/msat values are
	// set.
	importHistory.FailAmtSat = amount * 2
	hn.RPC.XImportMissionControlAssertErr(req)
}

// testRouteFeeCutoff tests that we are able to prevent querying routes and
// sending payments that incur a fee higher than the fee limit.
func testRouteFeeCutoff(ht *lntest.HarnessTest) {
	// For this test, we'll create the following topology:
	//
	//              --- Bob ---
	//            /             \
	// Alice ----                 ---- Dave
	//            \             /
	//              -- Carol --
	//
	// Alice will attempt to send payments to Dave that should not incur a
	// fee greater than the fee limit expressed as a percentage of the
	// amount and as a fixed amount of satoshis.
	const chanAmt = btcutil.Amount(100000)

	// Open a channel between Alice and Bob.
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)
	ht.EnsureConnected(alice, bob)

	chanPointAliceBob := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Create Carol's node and open a channel between her and Alice with
	// Alice being the funder.
	carol := ht.NewNode("Carol", nil)
	ht.ConnectNodes(carol, alice)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	chanPointAliceCarol := ht.OpenChannel(
		alice, carol, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Create Dave's node and open a channel between him and Bob with Bob
	// being the funder.
	dave := ht.NewNode("Dave", nil)
	ht.ConnectNodes(dave, bob)
	chanPointBobDave := ht.OpenChannel(
		bob, dave, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Open a channel between Carol and Dave.
	ht.ConnectNodes(carol, dave)
	chanPointCarolDave := ht.OpenChannel(
		carol, dave, lntest.OpenChannelParams{Amt: chanAmt},
	)

	// Now that all the channels were set up, we'll wait for all the nodes
	// to have seen all the channels.
	nodes := []*node.HarnessNode{alice, bob, carol, dave}
	networkChans := []*lnrpc.ChannelPoint{
		chanPointAliceBob, chanPointAliceCarol, chanPointBobDave,
		chanPointCarolDave,
	}
	for _, chanPoint := range networkChans {
		for _, node := range nodes {
			ht.AssertChannelInGraph(node, chanPoint)
		}
	}

	// The payments should only be successful across the route:
	//	Alice -> Bob -> Dave
	// Therefore, we'll update the fee policy on Carol's side for the
	// channel between her and Dave to invalidate the route:
	//	Alice -> Carol -> Dave
	baseFee := int64(10000)
	feeRate := int64(5)
	timeLockDelta := uint32(chainreg.DefaultBitcoinTimeLockDelta)
	maxHtlc := lntest.CalculateMaxHtlc(chanAmt)

	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMsat:      baseFee,
		FeeRateMilliMsat: testFeeBase * feeRate,
		TimeLockDelta:    timeLockDelta,
		MinHtlc:          1000, // default value
		MaxHtlcMsat:      maxHtlc,
	}

	updateFeeReq := &lnrpc.PolicyUpdateRequest{
		BaseFeeMsat:   baseFee,
		FeeRate:       float64(feeRate),
		TimeLockDelta: timeLockDelta,
		MaxHtlcMsat:   maxHtlc,
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: chanPointCarolDave,
		},
	}
	carol.RPC.UpdateChannelPolicy(updateFeeReq)

	// Wait for Alice to receive the channel update from Carol.
	ht.AssertChannelPolicyUpdate(
		alice, carol, expectedPolicy, chanPointCarolDave, false,
	)

	// We'll also need the channel IDs for Bob's channels in order to
	// confirm the route of the payments.
	channel := ht.QueryChannelByChanPoint(bob, chanPointAliceBob)
	aliceBobChanID := channel.ChanId
	require.NotZero(ht, aliceBobChanID,
		"channel between alice and bob not found")

	channel = ht.QueryChannelByChanPoint(bob, chanPointBobDave)
	bobDaveChanID := channel.ChanId
	require.NotZero(ht, bobDaveChanID,
		"channel between bob and dave not found")

	hopChanIDs := []uint64{aliceBobChanID, bobDaveChanID}

	// checkRoute is a helper closure to ensure the route contains the
	// correct intermediate hops.
	checkRoute := func(route *lnrpc.Route) {
		require.Len(ht, route.Hops, 2, "expected two hops")

		for i, hop := range route.Hops {
			require.Equal(ht, hopChanIDs[i], hop.ChanId,
				"hop chan id not match")
		}
	}

	// We'll be attempting to send two payments from Alice to Dave. One will
	// have a fee cutoff expressed as a percentage of the amount and the
	// other will have it expressed as a fixed amount of satoshis.
	const paymentAmt = 100
	carolFee := computeFee(lnwire.MilliSatoshi(baseFee), 1, paymentAmt)

	// testFeeCutoff is a helper closure that will ensure the different
	// types of fee limits work as intended when querying routes and sending
	// payments.
	testFeeCutoff := func(feeLimit *lnrpc.FeeLimit) {
		queryRoutesReq := &lnrpc.QueryRoutesRequest{
			PubKey:   dave.PubKeyStr,
			Amt:      paymentAmt,
			FeeLimit: feeLimit,
		}
		routesResp := alice.RPC.QueryRoutes(queryRoutesReq)

		checkRoute(routesResp.Routes[0])

		invoice := &lnrpc.Invoice{Value: paymentAmt}
		invoiceResp := dave.RPC.AddInvoice(invoice)

		sendReq := &routerrpc.SendPaymentRequest{
			PaymentRequest: invoiceResp.PaymentRequest,
			FeeLimitMsat:   noFeeLimitMsat,
		}
		switch limit := feeLimit.Limit.(type) {
		case *lnrpc.FeeLimit_Fixed:
			sendReq.FeeLimitMsat = 1000 * limit.Fixed
		case *lnrpc.FeeLimit_Percent:
			sendReq.FeeLimitMsat = 1000 * paymentAmt *
				limit.Percent / 100
		}

		result := ht.SendPaymentAssertSettled(alice, sendReq)

		checkRoute(result.Htlcs[0].Route)
	}

	// We'll start off using percentages first. Since the fee along the
	// route using Carol as an intermediate hop is 10% of the payment's
	// amount, we'll use a lower percentage in order to invalid that route.
	feeLimitPercent := &lnrpc.FeeLimit{
		Limit: &lnrpc.FeeLimit_Percent{
			Percent: baseFee/1000 - 1,
		},
	}
	testFeeCutoff(feeLimitPercent)

	// Now we'll test using fixed fee limit amounts. Since we computed the
	// fee for the route using Carol as an intermediate hop earlier, we can
	// use a smaller value in order to invalidate that route.
	feeLimitFixed := &lnrpc.FeeLimit{
		Limit: &lnrpc.FeeLimit_Fixed{
			Fixed: int64(carolFee.ToSatoshis()) - 1,
		},
	}
	testFeeCutoff(feeLimitFixed)
}

// testFeeLimitAfterQueryRoutes tests that a payment's fee limit is consistent
// with the fee of a queried route.
func testFeeLimitAfterQueryRoutes(ht *lntest.HarnessTest) {
	// Create a three hop network: Alice -> Bob -> Carol.
	chanAmt := btcutil.Amount(100000)
	cfgs := [][]string{nil, nil, nil}
	chanPoints, nodes := ht.CreateSimpleNetwork(
		cfgs, lntest.OpenChannelParams{Amt: chanAmt},
	)
	alice, bob, carol := nodes[0], nodes[1], nodes[2]
	chanPointAliceBob := chanPoints[0]

	// We set an inbound fee discount on Bob's channel to Alice to
	// effectively set the outbound fees charged to Carol to zero.
	expectedPolicy := &lnrpc.RoutingPolicy{
		FeeBaseMsat:             1000,
		FeeRateMilliMsat:        1,
		InboundFeeBaseMsat:      -1000,
		InboundFeeRateMilliMsat: -1,
		TimeLockDelta: uint32(
			chainreg.DefaultBitcoinTimeLockDelta,
		),
		MinHtlc:     1000,
		MaxHtlcMsat: lntest.CalculateMaxHtlc(chanAmt),
	}

	updateFeeReq := &lnrpc.PolicyUpdateRequest{
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: chanPointAliceBob,
		},
		BaseFeeMsat:   expectedPolicy.FeeBaseMsat,
		FeeRatePpm:    uint32(expectedPolicy.FeeRateMilliMsat),
		TimeLockDelta: expectedPolicy.TimeLockDelta,
		MaxHtlcMsat:   expectedPolicy.MaxHtlcMsat,
		InboundFee: &lnrpc.InboundFee{
			BaseFeeMsat: expectedPolicy.InboundFeeBaseMsat,
			FeeRatePpm:  expectedPolicy.InboundFeeRateMilliMsat,
		},
	}
	bob.RPC.UpdateChannelPolicy(updateFeeReq)

	// Wait for Alice to receive the channel update from Bob.
	ht.AssertChannelPolicyUpdate(
		alice, bob, expectedPolicy, chanPointAliceBob, false,
	)

	// We query the only route available to Carol.
	queryRoutesReq := &lnrpc.QueryRoutesRequest{
		PubKey: carol.PubKeyStr,
		Amt:    paymentAmt,
	}
	routesResp := alice.RPC.QueryRoutes(queryRoutesReq)

	// Verify that the route has zero fees.
	require.Len(ht, routesResp.Routes, 1)
	require.Len(ht, routesResp.Routes[0].Hops, 2)
	require.Zero(ht, routesResp.Routes[0].TotalFeesMsat)

	// Attempt a payment with a fee limit of zero.
	invoice := &lnrpc.Invoice{Value: paymentAmt}
	invoiceResp := carol.RPC.AddInvoice(invoice)
	sendReq := &routerrpc.SendPaymentRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
		FeeLimitMsat:   0,
	}

	// We assert that a route compatible with the fee limit is available.
	ht.SendPaymentAssertSettled(alice, sendReq)
}

// computeFee calculates the payment fee as specified in BOLT07.
func computeFee(baseFee, feeRate, amt lnwire.MilliSatoshi) lnwire.MilliSatoshi {
	return baseFee + amt*feeRate/1000000
}
