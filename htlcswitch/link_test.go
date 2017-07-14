package htlcswitch

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"reflect"

	"io"

	"math"

	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcutil"
)

// messageToString is used to produce less spammy log messages in trace
// mode by setting the 'Curve" parameter to nil. Doing this avoids printing out
// each of the field elements in the curve parameters for secp256k1.
func messageToString(msg lnwire.Message) string {
	switch m := msg.(type) {
	case *lnwire.RevokeAndAck:
		m.NextRevocationKey.Curve = nil
	case *lnwire.NodeAnnouncement:
		m.NodeID.Curve = nil
	case *lnwire.ChannelAnnouncement:
		m.NodeID1.Curve = nil
		m.NodeID2.Curve = nil
		m.BitcoinKey1.Curve = nil
		m.BitcoinKey2.Curve = nil
	case *lnwire.SingleFundingComplete:
		m.RevocationKey.Curve = nil
	case *lnwire.SingleFundingRequest:
		m.CommitmentKey.Curve = nil
		m.ChannelDerivationPoint.Curve = nil
	case *lnwire.SingleFundingResponse:
		m.ChannelDerivationPoint.Curve = nil
		m.CommitmentKey.Curve = nil
		m.RevocationKey.Curve = nil
	}

	return spew.Sdump(msg)
}

// createLogFunc is a helper function which returns the function which will be
// used for logging message are received from another peer.
func createLogFunc(name string, channelID lnwire.ChannelID) messageInterceptor {
	return func(m lnwire.Message) {
		if getChanID(m) == channelID {
			// Skip logging of extend revocation window messages.
			switch m := m.(type) {
			case *lnwire.RevokeAndAck:
				var zeroHash chainhash.Hash
				if bytes.Equal(zeroHash[:], m.Revocation[:]) {
					return
				}
			}

			fmt.Printf("---------------------- \n %v received: "+
				"%v", name, messageToString(m))
		}
	}
}

// TestChannelLinkSingleHopPayment in this test we checks the interaction
// between Alice and Bob within scope of one channel.
func TestChannelLinkSingleHopPayment(t *testing.T) {
	t.Parallel()

	n := newThreeHopNetwork(t,
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5,
	)
	if err := n.start(); err != nil {
		t.Fatal(err)
	}
	defer n.stop()

	bobBandwidthBefore := n.firstBobChannelLink.Bandwidth()
	aliceBandwidthBefore := n.aliceChannelLink.Bandwidth()

	debug := false
	if debug {
		// Log message that alice receives.
		n.aliceServer.record(createLogFunc("alice",
			n.aliceChannelLink.ChanID()))

		// Log message that bob receives.
		n.bobServer.record(createLogFunc("bob",
			n.firstBobChannelLink.ChanID()))
	}

	var amount btcutil.Amount = btcutil.SatoshiPerBitcoin
	htlcAmt, totalTimelock, hops := generateHops(amount, n.firstBobChannelLink)

	// Wait for:
	// * HTLC add request to be sent to bob.
	// * alice<->bob commitment state to be updated.
	// * settle request to be sent back from bob to alice.
	// * alice<->bob commitment state to be updated.
	// * user notification to be sent.
	invoice, err := n.makePayment(n.aliceServer, n.bobServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt, totalTimelock)
	if err != nil {
		t.Fatalf("unable to make the payment: %v", err)
	}

	// Wait for Bob to receive the revocation.
	//
	// TODO(roasbef); replace with select over returned err chan
	time.Sleep(100 * time.Millisecond)

	// Check that alice invoice was settled and bandwidth of HTLC
	// links was changed.
	if !invoice.Terms.Settled {
		t.Fatal("invoice wasn't settled")
	}

	if aliceBandwidthBefore-amount != n.aliceChannelLink.Bandwidth() {
		t.Fatal("alice bandwidth should have descreased on payment " +
			"amount")
	}

	if bobBandwidthBefore+amount != n.firstBobChannelLink.Bandwidth() {
		t.Fatal("bob bandwidth isn't match")
	}
}

// TestChannelLinkBidirectionalOneHopPayments tests the ability of channel
// link to cope with bigger number of payment updates that commitment
// transaction may consist.
func TestChannelLinkBidirectionalOneHopPayments(t *testing.T) {
	t.Parallel()

	n := newThreeHopNetwork(t,
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5,
	)
	if err := n.start(); err != nil {
		t.Fatal(err)
	}
	defer n.stop()

	bobBandwidthBefore := n.firstBobChannelLink.Bandwidth()
	aliceBandwidthBefore := n.aliceChannelLink.Bandwidth()

	debug := false
	if debug {
		// Log message that alice receives.
		n.aliceServer.record(createLogFunc("alice",
			n.aliceChannelLink.ChanID()))

		// Log message that bob receives.
		n.bobServer.record(createLogFunc("bob",
			n.firstBobChannelLink.ChanID()))
	}

	const amt btcutil.Amount = 10

	htlcAmt, totalTimelock, hopsForwards := generateHops(amt,
		n.firstBobChannelLink)
	_, _, hopsBackwards := generateHops(amt, n.aliceChannelLink)

	type result struct {
		err    error
		start  time.Time
		number int
		sender string
	}

	// Send max available payment number in both sides, thereby testing
	// the property of channel link to cope with overflowing.
	resultChan := make(chan *result)
	count := 2 * lnwallet.MaxHTLCNumber
	for i := 0; i < count/2; i++ {
		go func(i int) {
			r := &result{
				start:  time.Now(),
				number: i,
				sender: "alice",
			}

			_, r.err = n.makePayment(n.aliceServer, n.bobServer,
				n.bobServer.PubKey(), hopsForwards, amt, htlcAmt,
				totalTimelock)
			resultChan <- r
		}(i)
	}

	for i := 0; i < count/2; i++ {
		go func(i int) {
			r := &result{
				start:  time.Now(),
				number: i,
				sender: "bob",
			}

			_, r.err = n.makePayment(n.bobServer, n.aliceServer,
				n.aliceServer.PubKey(), hopsBackwards, amt, htlcAmt,
				totalTimelock)
			resultChan <- r
		}(i)
	}

	maxDelay := time.Duration(0)
	minDelay := time.Duration(math.MaxInt64)
	averageDelay := time.Duration(0)

	// Check that alice invoice was settled and bandwidth of HTLC
	// links was changed.
	for i := 0; i < count; i++ {
		select {
		case r := <-resultChan:
			if r.err != nil {
				t.Fatalf("unable to make the payment: %v", r.err)
			}

			delay := time.Since(r.start)
			if delay > maxDelay {
				maxDelay = delay
			}

			if delay < minDelay {
				minDelay = delay
			}
			averageDelay += delay

		case <-time.After(30 * time.Second):
			t.Fatalf("timeout: (%v/%v)", i+1, count)
		}
	}

	// At the end Bob and Alice balances should be the same as previous,
	// because they sent the equal amount of money to each other.
	if aliceBandwidthBefore != n.aliceChannelLink.Bandwidth() {
		t.Fatal("alice bandwidth shouldn't have changed")
	}

	if bobBandwidthBefore != n.firstBobChannelLink.Bandwidth() {
		t.Fatal("bob bandwidth shouldn't have changed")
	}

	t.Logf("Max waiting: %v", maxDelay)
	t.Logf("Min waiting: %v", minDelay)
	t.Logf("Average waiting: %v", time.Duration(int(averageDelay)/count))
}

// TestChannelLinkMultiHopPayment checks the ability to send payment over two
// hops. In this test we send the payment from Carol to Alice over Bob peer.
// (Carol -> Bob -> Alice) and checking that HTLC was settled properly and
// balances were changed in two channels.
func TestChannelLinkMultiHopPayment(t *testing.T) {
	t.Parallel()

	n := newThreeHopNetwork(t,
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5,
	)
	if err := n.start(); err != nil {
		t.Fatal(err)
	}
	defer n.stop()

	carolBandwidthBefore := n.carolChannelLink.Bandwidth()
	firstBobBandwidthBefore := n.firstBobChannelLink.Bandwidth()
	secondBobBandwidthBefore := n.secondBobChannelLink.Bandwidth()
	aliceBandwidthBefore := n.aliceChannelLink.Bandwidth()

	debug := false
	if debug {
		// Log messages that alice receives from bob.
		n.aliceServer.record(createLogFunc("[alice]<-bob<-carol: ",
			n.aliceChannelLink.ChanID()))

		// Log messages that bob receives from alice.
		n.bobServer.record(createLogFunc("alice->[bob]->carol: ",
			n.firstBobChannelLink.ChanID()))

		// Log messages that bob receives from carol.
		n.bobServer.record(createLogFunc("alice<-[bob]<-carol: ",
			n.secondBobChannelLink.ChanID()))

		// Log messages that carol receives from bob.
		n.carolServer.record(createLogFunc("alice->bob->[carol]",
			n.carolChannelLink.ChanID()))
	}

	var amount btcutil.Amount = btcutil.SatoshiPerBitcoin
	htlcAmt, totalTimelock, hops := generateHops(amount,
		n.firstBobChannelLink, n.carolChannelLink)

	// Wait for:
	// * HTLC add request to be sent from Alice to Bob.
	// * Alice<->Bob commitment states to be updated.
	// * HTLC add request to be propagated to Carol.
	// * Bob<->Carol commitment state to be updated.
	// * settle request to be sent back from Carol to Bob.
	// * Alice<->Bob commitment state to be updated.
	// * settle request to be sent back from Bob to Alice.
	// * Alice<->Bob commitment states to be updated.
	// * user notification to be sent.
	invoice, err := n.makePayment(n.aliceServer, n.carolServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt,
		totalTimelock)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// Wait for Bob to receive the revocation.
	time.Sleep(100 * time.Millisecond)

	// Check that Carol invoice was settled and bandwidth of HTLC
	// links were changed.
	if !invoice.Terms.Settled {
		t.Fatal("alice invoice wasn't settled")
	}

	expectedAliceBandwidth := aliceBandwidthBefore - htlcAmt
	if expectedAliceBandwidth != n.aliceChannelLink.Bandwidth() {
		t.Fatalf("channel bandwidth incorrect: expected %v, got %v",
			expectedAliceBandwidth, n.aliceChannelLink.Bandwidth())
	}

	expectedBobBandwidth1 := firstBobBandwidthBefore + htlcAmt
	if expectedBobBandwidth1 != n.firstBobChannelLink.Bandwidth() {
		t.Fatalf("channel bandwidth incorrect: expected %v, got %v",
			expectedBobBandwidth1, n.firstBobChannelLink.Bandwidth())
	}

	expectedBobBandwidth2 := secondBobBandwidthBefore - amount
	if expectedBobBandwidth2 != n.secondBobChannelLink.Bandwidth() {
		t.Fatalf("channel bandwidth incorrect: expected %v, got %v",
			expectedBobBandwidth2, n.secondBobChannelLink.Bandwidth())
	}

	expectedCarolBandwidth := carolBandwidthBefore + amount
	if expectedCarolBandwidth != n.carolChannelLink.Bandwidth() {
		t.Fatalf("channel bandwidth incorrect: expected %v, got %v",
			expectedCarolBandwidth, n.carolChannelLink.Bandwidth())
	}
}

// TestExitNodeTimelockPayloadMismatch tests that when an exit node receives an
// incoming HTLC, if the time lock encoded in the payload of the forwarded HTLC
// doesn't match the expected payment value, then the HTLC will be rejected
// with the appropriate error.
func TestExitNodeTimelockPayloadMismatch(t *testing.T) {
	t.Parallel()

	n := newThreeHopNetwork(t,
		btcutil.SatoshiPerBitcoin*5,
		btcutil.SatoshiPerBitcoin*5,
	)
	if err := n.start(); err != nil {
		t.Fatal(err)
	}
	defer n.stop()

	const amount = btcutil.SatoshiPerBitcoin
	htlcAmt, htlcExpiry, hops := generateHops(amount,
		n.firstBobChannelLink)

	// In order to exercise this case, we'll now _manually_ modify the
	// per-hop payload for outgoing time lock to be the incorrect value.
	// The proper value of the outgoing CLTV should be the policy set by
	// the receiving node, instead we set it to be a random value.
	hops[0].OutgoingCTLV = 500

	_, err := n.makePayment(n.aliceServer, n.bobServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt, htlcExpiry)
	if err == nil {
		t.Fatalf("payment should have failed but didn't")
	} else if err.Error() != lnwire.UpstreamTimeout.String() {
		// TODO(roasbeef): use proper error after error propagation is
		// in
		t.Fatalf("incorrect error, expected insufficient value, "+
			"instead have: %v", err)
	}
}

// TestExitNodeAmountPayloadMismatch tests that when an exit node receives an
// incoming HTLC, if the amount encoded in the onion payload of the forwarded
// HTLC doesn't match the expected payment value, then the HTLC will be
// rejected.
func TestExitNodeAmountPayloadMismatch(t *testing.T) {
	t.Parallel()

	n := newThreeHopNetwork(t,
		btcutil.SatoshiPerBitcoin*5,
		btcutil.SatoshiPerBitcoin*5,
	)
	if err := n.start(); err != nil {
		t.Fatal(err)
	}
	defer n.stop()

	const amount = btcutil.SatoshiPerBitcoin
	htlcAmt, htlcExpiry, hops := generateHops(amount, n.firstBobChannelLink)

	// In order to exercise this case, we'll now _manually_ modify the
	// per-hop payload for amount to be the incorrect value.  The proper
	// value of the amount to forward should be the amount that the
	// receiving node expects to receive.
	hops[0].AmountToForward = 1

	_, err := n.makePayment(n.aliceServer, n.bobServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt, htlcExpiry)
	if err == nil {
		t.Fatalf("payment should have failed but didn't")
	} else if err.Error() != lnwire.IncorrectValue.String() {
		// TODO(roasbeef): use proper error after error propagation is
		// in
		t.Fatalf("incorrect error, expected insufficient value, "+
			"instead have: %v", err)
	}
}

// TestLinkForwardMinHTLCPolicyMismatch tests that if a node is an intermediate
// node in a multi-hop payment, and receives an HTLC which violates its
// specified multi-hop policy, then the HTLC is rejected.
func TestLinkForwardTimelockPolicyMismatch(t *testing.T) {
	t.Parallel()

	n := newThreeHopNetwork(t,
		btcutil.SatoshiPerBitcoin*5,
		btcutil.SatoshiPerBitcoin*5,
	)
	if err := n.start(); err != nil {
		t.Fatal(err)
	}
	defer n.stop()

	// We'll be sending 1 BTC over a 2-hop (3 vertex) route.
	var amount btcutil.Amount = btcutil.SatoshiPerBitcoin

	// Generate the route over two hops, ignoring the total time lock that
	// we'll need to use for the first HTLC in order to have a sufficient
	// time-lock value to account for the decrements over the entire route.
	htlcAmt, htlcExpiry, hops := generateHops(amount, n.firstBobChannelLink,
		n.carolChannelLink)
	htlcExpiry += 10

	// Next, we'll make the payment which'll send an HTLC with our
	// specified parameters to the first hop in the route.
	_, err := n.makePayment(n.aliceServer, n.bobServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt, htlcExpiry)

	// We should get an error, and that error should indicate that the HTLC
	// should be rejected due to a policy violation.
	if err == nil {
		t.Fatalf("payment should have failed but didn't")
	} else if err.Error() != lnwire.UpstreamTimeout.String() {
		// TODO(roasbeef): use proper error after error propagation is
		// in
		t.Fatalf("incorrect error, expected insufficient value, "+
			"instead have: %v", err)
	}
}

// TestLinkForwardTimelockPolicyMismatch tests that if a node is an
// intermediate node in a multi-hop payment and receives an HTLC that violates
// its current fee policy, then the HTLC is rejected with the proper error.
func TestLinkForwardFeePolicyMismatch(t *testing.T) {
	t.Parallel()

	n := newThreeHopNetwork(t,
		btcutil.SatoshiPerBitcoin*5,
		btcutil.SatoshiPerBitcoin*5,
	)
	if err := n.start(); err != nil {
		t.Fatal(err)
	}
	defer n.stop()

	// We'll be sending 1 BTC over a 2-hop (3 vertex) route. Given the
	// current default fee of 1 SAT, if we just send a single BTC over in
	// an HTLC, it should be rejected.
	var amountNoFee btcutil.Amount = btcutil.SatoshiPerBitcoin

	// Generate the route over two hops, ignoring the amount we _should_
	// actually send in order to be able to cover fees.
	_, htlcExpiry, hops := generateHops(amountNoFee, n.firstBobChannelLink,
		n.carolChannelLink)

	// Next, we'll make the payment which'll send an HTLC with our
	// specified parameters to the first hop in the route.
	_, err := n.makePayment(n.aliceServer, n.bobServer,
		n.bobServer.PubKey(), hops, amountNoFee, amountNoFee,
		htlcExpiry)

	// We should get an error, and that error should indicate that the HTLC
	// should be rejected due to a policy violation.
	if err == nil {
		t.Fatalf("payment should have failed but didn't")
	} else if err.Error() != lnwire.IncorrectValue.String() {
		// TODO(roasbeef): use proper error after error propagation is
		// in
		t.Fatalf("incorrect error, expected insufficient value, "+
			"instead have: %v", err)
	}
}

// TestLinkForwardFeePolicyMismatch tests that if a node is an intermediate
// node and receives an HTLC which is _below_ its min HTLC policy, then the
// HTLC will be rejected.
func TestLinkForwardMinHTLCPolicyMismatch(t *testing.T) {
	t.Parallel()

	n := newThreeHopNetwork(t,
		btcutil.SatoshiPerBitcoin*5,
		btcutil.SatoshiPerBitcoin*5,
	)
	if err := n.start(); err != nil {
		t.Fatal(err)
	}
	defer n.stop()

	// The current default global min HTLC policy set in the default config
	// for the three-hop-network is 5 SAT. So in order to trigger this
	// failure mode, we'll create an HTLC with 1 satoshi.
	amountNoFee := btcutil.Amount(1)

	// With the amount set, we'll generate a route over 2 hops within the
	// network that attempts to pay out our specified amount.
	htlcAmt, htlcExpiry, hops := generateHops(amountNoFee,
		n.firstBobChannelLink, n.carolChannelLink)

	// Next, we'll make the payment which'll send an HTLC with our
	// specified parameters to the first hop in the route.
	_, err := n.makePayment(n.aliceServer, n.bobServer,
		n.bobServer.PubKey(), hops, amountNoFee, htlcAmt,
		htlcExpiry)

	// We should get an error, and that error should indicate that the HTLC
	// should be rejected due to a policy violation (below min HTLC).
	if err == nil {
		t.Fatalf("payment should have failed but didn't")
	} else if err.Error() != lnwire.IncorrectValue.String() {
		// TODO(roasbeef): use proper error after error propagation is
		// in
		t.Fatalf("incorrect error, expected insufficient value, "+
			"instead have: %v", err)
	}
}

// TestUpdateForwardingPolicy tests that the forwarding policy for a link is
// able to be updated properly. We'll first create an HTLC that meets the
// specified policy, assert that it succeeds, update the policy (to invalidate
// the prior HTLC), and then ensure that the HTLC is rejected.
func TestUpdateForwardingPolicy(t *testing.T) {
	t.Parallel()

	n := newThreeHopNetwork(t,
		btcutil.SatoshiPerBitcoin*5,
		btcutil.SatoshiPerBitcoin*5,
	)
	if err := n.start(); err != nil {
		t.Fatal(err)
	}
	defer n.stop()

	carolBandwidthBefore := n.carolChannelLink.Bandwidth()
	firstBobBandwidthBefore := n.firstBobChannelLink.Bandwidth()
	secondBobBandwidthBefore := n.secondBobChannelLink.Bandwidth()
	aliceBandwidthBefore := n.aliceChannelLink.Bandwidth()

	amountNoFee := btcutil.Amount(10)
	htlcAmt, htlcExpiry, hops := generateHops(amountNoFee,
		n.firstBobChannelLink, n.carolChannelLink)

	// First, send this 1 BTC payment over the three hops, the payment
	// should succeed, and all balances should be updated
	// accordingly.
	invoice, err := n.makePayment(n.aliceServer, n.carolServer,
		n.bobServer.PubKey(), hops, amountNoFee, htlcAmt,
		htlcExpiry)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Carol's invoice should now be shown as settled as the payment
	// succeeded.
	if !invoice.Terms.Settled {
		t.Fatal("carol's invoice wasn't settled")
	}
	expectedAliceBandwidth := aliceBandwidthBefore - htlcAmt
	if expectedAliceBandwidth != n.aliceChannelLink.Bandwidth() {
		t.Fatalf("channel bandwidth incorrect: expected %v, got %v",
			expectedAliceBandwidth, n.aliceChannelLink.Bandwidth())
	}
	expectedBobBandwidth1 := firstBobBandwidthBefore + htlcAmt
	if expectedBobBandwidth1 != n.firstBobChannelLink.Bandwidth() {
		t.Fatalf("channel bandwidth incorrect: expected %v, got %v",
			expectedBobBandwidth1, n.firstBobChannelLink.Bandwidth())
	}
	expectedBobBandwidth2 := secondBobBandwidthBefore - amountNoFee
	if expectedBobBandwidth2 != n.secondBobChannelLink.Bandwidth() {
		t.Fatalf("channel bandwidth incorrect: expected %v, got %v",
			expectedBobBandwidth2, n.secondBobChannelLink.Bandwidth())
	}
	expectedCarolBandwidth := carolBandwidthBefore + amountNoFee
	if expectedCarolBandwidth != n.carolChannelLink.Bandwidth() {
		t.Fatalf("channel bandwidth incorrect: expected %v, got %v",
			expectedCarolBandwidth, n.carolChannelLink.Bandwidth())
	}

	// Now we'll update Bob's policy to jack up his free rate to an extent
	// that'll cause him to reject the same HTLC that we just sent.
	//
	// TODO(roasbeef): should implement grace period within link policy
	// update logic
	newPolicy := n.globalPolicy
	newPolicy.BaseFee = btcutil.Amount(1000)
	n.firstBobChannelLink.UpdateForwardingPolicy(newPolicy)
}

// TestChannelLinkMultiHopInsufficientPayment checks that we receive error if
// bob<->alice channel has insufficient BTC capacity/bandwidth. In this test we
// send the payment from Carol to Alice over Bob peer. (Carol -> Bob -> Alice)
func TestChannelLinkMultiHopInsufficientPayment(t *testing.T) {
	t.Parallel()

	n := newThreeHopNetwork(t,
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5,
	)
	if err := n.start(); err != nil {
		t.Fatalf("can't start three hop network: %v", err)
	}
	defer n.stop()

	carolBandwidthBefore := n.carolChannelLink.Bandwidth()
	firstBobBandwidthBefore := n.firstBobChannelLink.Bandwidth()
	secondBobBandwidthBefore := n.secondBobChannelLink.Bandwidth()
	aliceBandwidthBefore := n.aliceChannelLink.Bandwidth()

	var amount btcutil.Amount = 4 * btcutil.SatoshiPerBitcoin
	htlcAmt, totalTimelock, hops := generateHops(amount,
		n.firstBobChannelLink, n.carolChannelLink)

	// Wait for:
	// * HTLC add request to be sent to from Alice to Bob.
	// * Alice<->Bob commitment states to be updated.
	// * Bob trying to add HTLC add request in Bob<->Carol channel.
	// * Cancel HTLC request to be sent back from Bob to Alice.
	// * user notification to be sent.
	invoice, err := n.makePayment(n.aliceServer, n.bobServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt, totalTimelock)
	if err == nil {
		t.Fatal("error haven't been received")
	} else if err.Error() != errors.New(lnwire.InsufficientCapacity).Error() {
		t.Fatalf("wrong error have been received: %v", err)
	}

	// Wait for Alice to receive the revocation.
	//
	// TODO(roasbeef): add in ntfn hook for state transition completion
	time.Sleep(100 * time.Millisecond)

	// Check that alice invoice wasn't settled and bandwidth of htlc
	// links hasn't been changed.
	if invoice.Terms.Settled {
		t.Fatal("alice invoice was settled")
	}

	if n.aliceChannelLink.Bandwidth() != aliceBandwidthBefore {
		t.Fatal("the bandwidth of alice channel link which handles " +
			"alice->bob channel should be the same")
	}

	if n.firstBobChannelLink.Bandwidth() != firstBobBandwidthBefore {
		t.Fatal("the bandwidth of bob channel link which handles " +
			"alice->bob channel should be the same")
	}

	if n.secondBobChannelLink.Bandwidth() != secondBobBandwidthBefore {
		t.Fatal("the bandwidth of bob channel link which handles " +
			"bob->carol channel should be the same")
	}

	if n.carolChannelLink.Bandwidth() != carolBandwidthBefore {
		t.Fatal("the bandwidth of carol channel link which handles " +
			"bob->carol channel should be the same")
	}
}

// TestChannelLinkMultiHopUnknownPaymentHash checks that we receive remote error
// from Alice if she received not suitable payment hash for htlc.
func TestChannelLinkMultiHopUnknownPaymentHash(t *testing.T) {
	t.Parallel()

	n := newThreeHopNetwork(t,
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5,
	)
	if err := n.start(); err != nil {
		t.Fatalf("can't start three hop network: %v", err)
	}
	defer n.stop()

	carolBandwidthBefore := n.carolChannelLink.Bandwidth()
	firstBobBandwidthBefore := n.firstBobChannelLink.Bandwidth()
	secondBobBandwidthBefore := n.secondBobChannelLink.Bandwidth()
	aliceBandwidthBefore := n.aliceChannelLink.Bandwidth()

	var amount btcutil.Amount = btcutil.SatoshiPerBitcoin

	htlcAmt, totalTimelock, hops := generateHops(amount,
		n.firstBobChannelLink, n.carolChannelLink)
	blob, err := generateRoute(hops...)
	if err != nil {
		t.Fatal(err)
	}

	// Generate payment: invoice and htlc.
	invoice, htlc, err := generatePayment(amount, htlcAmt, totalTimelock,
		blob)
	if err != nil {
		t.Fatal(err)
	}

	// We need to have wrong rhash for that reason we should change the
	// preimage. Inverse first byte by xoring with 0xff.
	invoice.Terms.PaymentPreimage[0] ^= byte(255)

	// Check who is last in the route and add invoice to server registry.
	if err := n.carolServer.registry.AddInvoice(invoice); err != nil {
		t.Fatalf("can't add invoice in carol registry: %v", err)
	}

	// Send payment and expose err channel.
	_, err = n.aliceServer.htlcSwitch.SendHTLC(n.bobServer.PubKey(), htlc)
	if err == nil {
		t.Fatal("error wasn't received")
	}

	// Wait for Alice to receive the revocation.
	time.Sleep(100 * time.Millisecond)

	// Check that alice invoice wasn't settled and bandwidth of htlc
	// links hasn't been changed.
	if invoice.Terms.Settled {
		t.Fatal("alice invoice was settled")
	}

	if n.aliceChannelLink.Bandwidth() != aliceBandwidthBefore {
		t.Fatal("the bandwidth of alice channel link which handles " +
			"alice->bob channel should be the same")
	}

	if n.firstBobChannelLink.Bandwidth() != firstBobBandwidthBefore {
		t.Fatal("the bandwidth of bob channel link which handles " +
			"alice->bob channel should be the same")
	}

	if n.secondBobChannelLink.Bandwidth() != secondBobBandwidthBefore {
		t.Fatal("the bandwidth of bob channel link which handles " +
			"bob->carol channel should be the same")
	}

	if n.carolChannelLink.Bandwidth() != carolBandwidthBefore {
		t.Fatal("the bandwidth of carol channel link which handles " +
			"bob->carol channel should be the same")
	}
}

// TestChannelLinkMultiHopUnknownNextHop construct the chain of hops
// Carol<->Bob<->Alice and checks that we receive remote error from Bob if he
// has no idea about next hop (hop might goes down and routing info not updated
// yet).
func TestChannelLinkMultiHopUnknownNextHop(t *testing.T) {
	t.Parallel()

	n := newThreeHopNetwork(t,
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5,
	)
	if err := n.start(); err != nil {
		t.Fatal(err)
	}
	defer n.stop()

	carolBandwidthBefore := n.carolChannelLink.Bandwidth()
	firstBobBandwidthBefore := n.firstBobChannelLink.Bandwidth()
	secondBobBandwidthBefore := n.secondBobChannelLink.Bandwidth()
	aliceBandwidthBefore := n.aliceChannelLink.Bandwidth()

	var amount btcutil.Amount = btcutil.SatoshiPerBitcoin
	htlcAmt, totalTimelock, hops := generateHops(amount,
		n.firstBobChannelLink, n.carolChannelLink)

	davePub := newMockServer(t, "save").PubKey()
	invoice, err := n.makePayment(n.aliceServer, n.bobServer, davePub, hops,
		amount, htlcAmt, totalTimelock)
	if err == nil {
		t.Fatal("error haven't been received")
	} else if err.Error() != errors.New(lnwire.UnknownDestination).Error() {
		t.Fatalf("wrong error have been received: %v", err)
	}

	// Wait for Alice to receive the revocation.
	//
	// TODO(roasbeef): add in ntfn hook for state transition completion
	time.Sleep(100 * time.Millisecond)

	// Check that alice invoice wasn't settled and bandwidth of htlc
	// links hasn't been changed.
	if invoice.Terms.Settled {
		t.Fatal("alice invoice was settled")
	}

	if n.aliceChannelLink.Bandwidth() != aliceBandwidthBefore {
		t.Fatal("the bandwidth of alice channel link which handles " +
			"alice->bob channel should be the same")
	}

	if n.firstBobChannelLink.Bandwidth() != firstBobBandwidthBefore {
		t.Fatal("the bandwidth of bob channel link which handles " +
			"alice->bob channel should be the same")
	}

	if n.secondBobChannelLink.Bandwidth() != secondBobBandwidthBefore {
		t.Fatal("the bandwidth of bob channel link which handles " +
			"bob->carol channel should be the same")
	}

	if n.carolChannelLink.Bandwidth() != carolBandwidthBefore {
		t.Fatal("the bandwidth of carol channel link which handles " +
			"bob->carol channel should be the same")
	}
}

// TestChannelLinkMultiHopDecodeError checks that we send HTLC cancel if
// decoding of onion blob failed.
func TestChannelLinkMultiHopDecodeError(t *testing.T) {
	t.Parallel()

	n := newThreeHopNetwork(t,
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5,
	)
	if err := n.start(); err != nil {
		t.Fatalf("can't start three hop network: %v", err)
	}
	defer n.stop()

	// Replace decode function with another which throws an error.
	n.carolChannelLink.cfg.DecodeOnion = func(r io.Reader, meta []byte) (
		HopIterator, error) {
		return nil, errors.New("some sphinx decode error")
	}

	carolBandwidthBefore := n.carolChannelLink.Bandwidth()
	firstBobBandwidthBefore := n.firstBobChannelLink.Bandwidth()
	secondBobBandwidthBefore := n.secondBobChannelLink.Bandwidth()
	aliceBandwidthBefore := n.aliceChannelLink.Bandwidth()

	var amount btcutil.Amount = btcutil.SatoshiPerBitcoin
	htlcAmt, totalTimelock, hops := generateHops(amount,
		n.firstBobChannelLink, n.carolChannelLink)

	invoice, err := n.makePayment(n.aliceServer, n.carolServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt, totalTimelock)
	if err == nil {
		t.Fatal("error haven't been received")
	} else if err.Error() != errors.New(lnwire.SphinxParseError).Error() {
		t.Fatalf("wrong error have been received: %v", err)
	}

	// Wait for Bob to receive the revocation.
	time.Sleep(100 * time.Millisecond)

	// Check that alice invoice wasn't settled and bandwidth of htlc
	// links hasn't been changed.
	if invoice.Terms.Settled {
		t.Fatal("alice invoice was settled")
	}

	if n.aliceChannelLink.Bandwidth() != aliceBandwidthBefore {
		t.Fatal("the bandwidth of alice channel link which handles " +
			"alice->bob channel should be the same")
	}

	if n.firstBobChannelLink.Bandwidth() != firstBobBandwidthBefore {
		t.Fatal("the bandwidth of bob channel link which handles " +
			"alice->bob channel should be the same")
	}

	if n.secondBobChannelLink.Bandwidth() != secondBobBandwidthBefore {
		t.Fatal("the bandwidth of bob channel link which handles " +
			"bob->carol channel should be the same")
	}

	if n.carolChannelLink.Bandwidth() != carolBandwidthBefore {
		t.Fatal("the bandwidth of carol channel link which handles " +
			"bob->carol channel should be the same")
	}
}

// TestChannelLinkSingleHopMessageOrdering test checks ordering of message which
// flying around between Alice and Bob are correct when Bob sends payments to
// Alice.
func TestChannelLinkSingleHopMessageOrdering(t *testing.T) {
	t.Parallel()

	n := newThreeHopNetwork(t,
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5,
	)

	chanPoint := n.aliceChannelLink.ChanID()

	// Append initial channel window revocation messages which occurs after
	// channel opening.
	var aliceOrder []lnwire.Message
	for i := 0; i < lnwallet.InitialRevocationWindow; i++ {
		aliceOrder = append(aliceOrder, &lnwire.RevokeAndAck{})
	}

	// The order in which Alice receives wire messages.
	aliceOrder = append(aliceOrder, []lnwire.Message{
		&lnwire.RevokeAndAck{},
		&lnwire.CommitSig{},
		&lnwire.UpdateFufillHTLC{},
		&lnwire.CommitSig{},
		&lnwire.RevokeAndAck{},
	}...)

	// Append initial channel window revocation messages which occurs after
	// channel channel opening.
	var bobOrder []lnwire.Message
	for i := 0; i < lnwallet.InitialRevocationWindow; i++ {
		bobOrder = append(bobOrder, &lnwire.RevokeAndAck{})
	}

	// The order in which Bob receives wire messages.
	bobOrder = append(bobOrder, []lnwire.Message{
		&lnwire.UpdateAddHTLC{},
		&lnwire.CommitSig{},
		&lnwire.RevokeAndAck{},
		&lnwire.RevokeAndAck{},
		&lnwire.CommitSig{},
	}...)

	debug := false
	if debug {
		// Log message that alice receives.
		n.aliceServer.record(createLogFunc("alice",
			n.aliceChannelLink.ChanID()))

		// Log message that bob receives.
		n.bobServer.record(createLogFunc("bob",
			n.firstBobChannelLink.ChanID()))
	}

	// Check that alice receives messages in right order.
	n.aliceServer.record(func(m lnwire.Message) {
		if getChanID(m) == chanPoint {
			if len(aliceOrder) == 0 {
				t.Fatal("redundant messages")
			}

			if reflect.TypeOf(aliceOrder[0]) != reflect.TypeOf(m) {
				t.Fatalf("alice received wrong message: \n"+
					"real: %v\n expected: %v", m.MsgType(),
					aliceOrder[0].MsgType())
			}
			aliceOrder = aliceOrder[1:]
		}
	})

	// Check that bob receives messages in right order.
	n.bobServer.record(func(m lnwire.Message) {
		if getChanID(m) == chanPoint {
			if len(bobOrder) == 0 {
				t.Fatal("redundant messages")
			}

			if reflect.TypeOf(bobOrder[0]) != reflect.TypeOf(m) {
				t.Fatalf("bob received wrong message: \n"+
					"real: %v\n expected: %v", m.MsgType(),
					bobOrder[0].MsgType())
			}
			bobOrder = bobOrder[1:]
		}
	})

	if err := n.start(); err != nil {
		t.Fatalf("can't start three hop network: %v", err)
	}
	defer n.stop()

	var amount btcutil.Amount = btcutil.SatoshiPerBitcoin
	htlcAmt, totalTimelock, hops := generateHops(amount, n.firstBobChannelLink)

	// Wait for:
	// * htlc add htlc request to be sent to alice
	// * alice<->bob commitment state to be updated
	// * settle request to be sent back from alice to bob
	// * alice<->bob commitment state to be updated
	_, err := n.makePayment(n.aliceServer, n.bobServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt, totalTimelock)
	if err != nil {
		t.Fatalf("unable to make the payment: %v", err)
	}
}
