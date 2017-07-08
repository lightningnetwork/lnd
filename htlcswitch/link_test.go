package htlcswitch

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"io"

	"math"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcutil"
)

const (
	testStartingHeight = 100
)

// messageToString is used to produce less spammy log messages in trace mode by
// setting the 'Curve" parameter to nil. Doing this avoids printing out each of
// the field elements in the curve parameters for secp256k1.
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
	case *lnwire.AcceptChannel:
		m.FundingKey.Curve = nil
		m.RevocationPoint.Curve = nil
		m.PaymentPoint.Curve = nil
		m.DelayedPaymentPoint.Curve = nil
		m.FirstCommitmentPoint.Curve = nil
	case *lnwire.OpenChannel:
		m.FundingKey.Curve = nil
		m.RevocationPoint.Curve = nil
		m.PaymentPoint.Curve = nil
		m.DelayedPaymentPoint.Curve = nil
		m.FirstCommitmentPoint.Curve = nil
	case *lnwire.FundingLocked:
		m.NextPerCommitmentPoint.Curve = nil
	}

	return spew.Sdump(msg)
}

// expectedMessage struct hols the message which travels from one peer to
// another, and additional information like, should this message we skipped
// for handling.
type expectedMessage struct {
	from    string
	to      string
	message lnwire.Message
	skip    bool
}

// createLogFunc is a helper function which returns the function which will be
// used for logging message are received from another peer.
func createLogFunc(name string, channelID lnwire.ChannelID) messageInterceptor {
	return func(m lnwire.Message) (bool, error) {
		chanID, err := getChanID(m)
		if err != nil {
			return false, err
		}

		if chanID == channelID {
			// Skip logging of extend revocation window messages.
			switch m := m.(type) {
			case *lnwire.RevokeAndAck:
				var zeroHash chainhash.Hash
				if bytes.Equal(zeroHash[:], m.Revocation[:]) {
					return false, nil
				}
			}

			fmt.Printf("---------------------- \n %v received: "+
				"%v", name, messageToString(m))
		}
		return false, nil
	}
}

// createInterceptorFunc creates the function by the given set of messages
// which, checks the order of the messages and skip the ones which were
// indicated to be intercepted.
func createInterceptorFunc(peer string, messages []expectedMessage,
	chanID lnwire.ChannelID, debug bool) messageInterceptor {

	// Filter message which should be received with given peer name.
	var expectToReceive []expectedMessage
	for _, message := range messages {
		if message.to == peer {
			expectToReceive = append(expectToReceive, message)
		}
	}

	// Return function which checks the message order and skip the
	// messages.
	return func(m lnwire.Message) (bool, error) {
		messageChanID, err := getChanID(m)
		if err != nil {
			return false, err
		}

		if messageChanID == chanID {
			if len(expectToReceive) == 0 {
				return false, errors.Errorf("received unexpected message out "+
					"of range: %v", m.MsgType())
			}

			expectedMessage := expectToReceive[0]
			expectToReceive = expectToReceive[1:]

			if expectedMessage.message.MsgType() != m.MsgType() {
				return false, errors.Errorf("%v received wrong message: \n"+
					"real: %v\nexpected: %v", peer, m.MsgType(),
					expectedMessage.message.MsgType())
			}

			if debug {
				if expectedMessage.skip {
					fmt.Printf("'%v' skiped the received message: %v \n",
						peer, m.MsgType())
				} else {
					fmt.Printf("'%v' received message: %v \n", peer,
						m.MsgType())
				}
			}

			return expectedMessage.skip, nil
		}
		return false, nil
	}
}

// TestChannelLinkSingleHopPayment in this test we checks the interaction
// between Alice and Bob within scope of one channel.
func TestChannelLinkSingleHopPayment(t *testing.T) {
	t.Parallel()

	n := newThreeHopNetwork(t,
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5,
		testStartingHeight,
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
		n.aliceServer.intersect(createLogFunc("alice",
			n.aliceChannelLink.ChanID()))

		// Log message that bob receives.
		n.bobServer.intersect(createLogFunc("bob",
			n.firstBobChannelLink.ChanID()))
	}

	amount := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)
	htlcAmt, totalTimelock, hops := generateHops(amount, testStartingHeight,
		n.firstBobChannelLink)

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
		t.Fatal("alice bandwidth should have decrease on payment " +
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
		testStartingHeight,
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
		n.aliceServer.intersect(createLogFunc("alice",
			n.aliceChannelLink.ChanID()))

		// Log message that bob receives.
		n.bobServer.intersect(createLogFunc("bob",
			n.firstBobChannelLink.ChanID()))
	}

	amt := lnwire.NewMSatFromSatoshis(20000)

	htlcAmt, totalTimelock, hopsForwards := generateHops(amt,
		testStartingHeight, n.firstBobChannelLink)
	_, _, hopsBackwards := generateHops(amt,
		testStartingHeight, n.aliceChannelLink)

	type result struct {
		err    error
		start  time.Time
		number int
		sender string
	}

	// Send max available payment number in both sides, thereby testing
	// the property of channel link to cope with overflowing.
	count := 2 * lnwallet.MaxHTLCNumber
	resultChan := make(chan *result, count)
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

		case <-time.After(5 * time.Minute):
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
		testStartingHeight,
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
		n.aliceServer.intersect(createLogFunc("[alice]<-bob<-carol: ",
			n.aliceChannelLink.ChanID()))

		// Log messages that bob receives from alice.
		n.bobServer.intersect(createLogFunc("alice->[bob]->carol: ",
			n.firstBobChannelLink.ChanID()))

		// Log messages that bob receives from carol.
		n.bobServer.intersect(createLogFunc("alice<-[bob]<-carol: ",
			n.secondBobChannelLink.ChanID()))

		// Log messages that carol receives from bob.
		n.carolServer.intersect(createLogFunc("alice->bob->[carol]",
			n.carolChannelLink.ChanID()))
	}

	amount := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)
	htlcAmt, totalTimelock, hops := generateHops(amount,
		testStartingHeight,
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
		testStartingHeight,
	)
	if err := n.start(); err != nil {
		t.Fatal(err)
	}
	defer n.stop()

	const amount = btcutil.SatoshiPerBitcoin
	htlcAmt, htlcExpiry, hops := generateHops(amount,
		testStartingHeight, n.firstBobChannelLink)

	// In order to exercise this case, we'll now _manually_ modify the
	// per-hop payload for outgoing time lock to be the incorrect value.
	// The proper value of the outgoing CLTV should be the policy set by
	// the receiving node, instead we set it to be a random value.
	hops[0].OutgoingCTLV = 500

	_, err := n.makePayment(n.aliceServer, n.bobServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt, htlcExpiry)
	if err == nil {
		t.Fatalf("payment should have failed but didn't")
	}

	ferr, ok := err.(*ForwardingError)
	if !ok {
		t.Fatalf("expected a ForwardingError, instead got: %T", err)
	}

	switch ferr.FailureMessage.(type) {
	case *lnwire.FailFinalIncorrectCltvExpiry:
	default:
		t.Fatalf("incorrect error, expected incorrect cltv expiry, "+
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
		testStartingHeight,
	)
	if err := n.start(); err != nil {
		t.Fatal(err)
	}
	defer n.stop()

	const amount = btcutil.SatoshiPerBitcoin
	htlcAmt, htlcExpiry, hops := generateHops(amount, testStartingHeight,
		n.firstBobChannelLink)

	// In order to exercise this case, we'll now _manually_ modify the
	// per-hop payload for amount to be the incorrect value.  The proper
	// value of the amount to forward should be the amount that the
	// receiving node expects to receive.
	hops[0].AmountToForward = 1

	_, err := n.makePayment(n.aliceServer, n.bobServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt, htlcExpiry)
	if err == nil {
		t.Fatalf("payment should have failed but didn't")
	} else if err.Error() != lnwire.CodeIncorrectPaymentAmount.String() {
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
		testStartingHeight,
	)
	if err := n.start(); err != nil {
		t.Fatal(err)
	}
	defer n.stop()

	// We'll be sending 1 BTC over a 2-hop (3 vertex) route.
	amount := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)

	// Generate the route over two hops, ignoring the total time lock that
	// we'll need to use for the first HTLC in order to have a sufficient
	// time-lock value to account for the decrements over the entire route.
	htlcAmt, htlcExpiry, hops := generateHops(amount, testStartingHeight,
		n.firstBobChannelLink, n.carolChannelLink)
	htlcExpiry -= 2

	// Next, we'll make the payment which'll send an HTLC with our
	// specified parameters to the first hop in the route.
	_, err := n.makePayment(n.aliceServer, n.carolServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt, htlcExpiry)

	// We should get an error, and that error should indicate that the HTLC
	// should be rejected due to a policy violation.
	if err == nil {
		t.Fatalf("payment should have failed but didn't")
	}

	ferr, ok := err.(*ForwardingError)
	if !ok {
		t.Fatalf("expected a ForwardingError, instead got: %T", err)
	}

	switch ferr.FailureMessage.(type) {
	case *lnwire.FailIncorrectCltvExpiry:
	default:
		t.Fatalf("incorrect error, expected incorrect cltv expiry, "+
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
		testStartingHeight,
	)
	if err := n.start(); err != nil {
		t.Fatal(err)
	}
	defer n.stop()

	// We'll be sending 1 BTC over a 2-hop (3 vertex) route. Given the
	// current default fee of 1 SAT, if we just send a single BTC over in
	// an HTLC, it should be rejected.
	amountNoFee := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)

	// Generate the route over two hops, ignoring the amount we _should_
	// actually send in order to be able to cover fees.
	_, htlcExpiry, hops := generateHops(amountNoFee, testStartingHeight,
		n.firstBobChannelLink, n.carolChannelLink)

	// Next, we'll make the payment which'll send an HTLC with our
	// specified parameters to the first hop in the route.
	_, err := n.makePayment(n.aliceServer, n.bobServer,
		n.bobServer.PubKey(), hops, amountNoFee, amountNoFee,
		htlcExpiry)

	// We should get an error, and that error should indicate that the HTLC
	// should be rejected due to a policy violation.
	if err == nil {
		t.Fatalf("payment should have failed but didn't")
	}

	ferr, ok := err.(*ForwardingError)
	if !ok {
		t.Fatalf("expected a ForwardingError, instead got: %T", err)
	}

	switch ferr.FailureMessage.(type) {
	case *lnwire.FailFeeInsufficient:
	default:
		t.Fatalf("incorrect error, expected fee insufficient, "+
			"instead have: %T", err)
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
		testStartingHeight,
	)
	if err := n.start(); err != nil {
		t.Fatal(err)
	}
	defer n.stop()

	// The current default global min HTLC policy set in the default config
	// for the three-hop-network is 5 SAT. So in order to trigger this
	// failure mode, we'll create an HTLC with 1 satoshi.
	amountNoFee := lnwire.NewMSatFromSatoshis(1)

	// With the amount set, we'll generate a route over 2 hops within the
	// network that attempts to pay out our specified amount.
	htlcAmt, htlcExpiry, hops := generateHops(amountNoFee, testStartingHeight,
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
	}

	ferr, ok := err.(*ForwardingError)
	if !ok {
		t.Fatalf("expected a ForwardingError, instead got: %T", err)
	}

	switch ferr.FailureMessage.(type) {
	case *lnwire.FailAmountBelowMinimum:
	default:
		t.Fatalf("incorrect error, expected amount below minimum, "+
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
		testStartingHeight,
	)
	if err := n.start(); err != nil {
		t.Fatal(err)
	}
	defer n.stop()

	carolBandwidthBefore := n.carolChannelLink.Bandwidth()
	firstBobBandwidthBefore := n.firstBobChannelLink.Bandwidth()
	secondBobBandwidthBefore := n.secondBobChannelLink.Bandwidth()
	aliceBandwidthBefore := n.aliceChannelLink.Bandwidth()

	amountNoFee := lnwire.NewMSatFromSatoshis(10)
	htlcAmt, htlcExpiry, hops := generateHops(amountNoFee,
		testStartingHeight,
		n.firstBobChannelLink, n.carolChannelLink)

	// First, send this 1 BTC payment over the three hops, the payment
	// should succeed, and all balances should be updated accordingly.
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
	newPolicy.BaseFee = lnwire.NewMSatFromSatoshis(1000)
	n.firstBobChannelLink.UpdateForwardingPolicy(newPolicy)

	// Next, we'll send the payment again, using the exact same per-hop
	// payload for each node. This payment should fail as it wont' factor
	// in Bob's new fee policy.
	_, err = n.makePayment(n.aliceServer, n.carolServer,
		n.bobServer.PubKey(), hops, amountNoFee, htlcAmt,
		htlcExpiry)
	if err == nil {
		t.Fatalf("payment should've been rejected")
	}

	ferr, ok := err.(*ForwardingError)
	if !ok {
		t.Fatalf("expected a ForwardingError, instead got: %T", err)
	}
	switch ferr.FailureMessage.(type) {
	case *lnwire.FailFeeInsufficient:
	default:
		t.Fatalf("expected FailFeeInsufficient instead got: %v", err)
	}
}

// TestChannelLinkMultiHopInsufficientPayment checks that we receive error if
// bob<->alice channel has insufficient BTC capacity/bandwidth. In this test we
// send the payment from Carol to Alice over Bob peer. (Carol -> Bob -> Alice)
func TestChannelLinkMultiHopInsufficientPayment(t *testing.T) {
	t.Parallel()

	n := newThreeHopNetwork(t,
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5,
		testStartingHeight,
	)
	if err := n.start(); err != nil {
		t.Fatalf("unable to start three hop network: %v", err)
	}
	defer n.stop()

	carolBandwidthBefore := n.carolChannelLink.Bandwidth()
	firstBobBandwidthBefore := n.firstBobChannelLink.Bandwidth()
	secondBobBandwidthBefore := n.secondBobChannelLink.Bandwidth()
	aliceBandwidthBefore := n.aliceChannelLink.Bandwidth()

	amount := lnwire.NewMSatFromSatoshis(4 * btcutil.SatoshiPerBitcoin)
	htlcAmt, totalTimelock, hops := generateHops(amount, testStartingHeight,
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
	} else if !strings.Contains(err.Error(), "insufficient capacity") {
		t.Fatalf("wrong error has been received: %v", err)
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
		testStartingHeight,
	)
	if err := n.start(); err != nil {
		t.Fatalf("unable to start three hop network: %v", err)
	}
	defer n.stop()

	carolBandwidthBefore := n.carolChannelLink.Bandwidth()
	firstBobBandwidthBefore := n.firstBobChannelLink.Bandwidth()
	secondBobBandwidthBefore := n.secondBobChannelLink.Bandwidth()
	aliceBandwidthBefore := n.aliceChannelLink.Bandwidth()

	amount := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)

	htlcAmt, totalTimelock, hops := generateHops(amount, testStartingHeight,
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
		t.Fatalf("unable to add invoice in carol registry: %v", err)
	}

	// Send payment and expose err channel.
	_, err = n.aliceServer.htlcSwitch.SendHTLC(n.bobServer.PubKey(), htlc,
		newMockDeobfuscator())
	if err.Error() != lnwire.CodeUnknownPaymentHash.String() {
		t.Fatal("error haven't been received")
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
		testStartingHeight,
	)
	if err := n.start(); err != nil {
		t.Fatal(err)
	}
	defer n.stop()

	carolBandwidthBefore := n.carolChannelLink.Bandwidth()
	firstBobBandwidthBefore := n.firstBobChannelLink.Bandwidth()
	secondBobBandwidthBefore := n.secondBobChannelLink.Bandwidth()
	aliceBandwidthBefore := n.aliceChannelLink.Bandwidth()

	amount := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)
	htlcAmt, totalTimelock, hops := generateHops(amount, testStartingHeight,
		n.firstBobChannelLink, n.carolChannelLink)

	davePub := newMockServer(t, "save").PubKey()
	invoice, err := n.makePayment(n.aliceServer, n.bobServer, davePub, hops,
		amount, htlcAmt, totalTimelock)
	if err == nil {
		t.Fatal("error haven't been received")
	} else if err.Error() != lnwire.CodeUnknownNextPeer.String() {
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
		testStartingHeight,
	)
	if err := n.start(); err != nil {
		t.Fatalf("unable to start three hop network: %v", err)
	}
	defer n.stop()

	// Replace decode function with another which throws an error.
	n.carolChannelLink.cfg.DecodeOnionObfuscator = func(
		r io.Reader) (ErrorEncrypter, lnwire.FailCode) {
		return nil, lnwire.CodeInvalidOnionVersion
	}

	carolBandwidthBefore := n.carolChannelLink.Bandwidth()
	firstBobBandwidthBefore := n.firstBobChannelLink.Bandwidth()
	secondBobBandwidthBefore := n.secondBobChannelLink.Bandwidth()
	aliceBandwidthBefore := n.aliceChannelLink.Bandwidth()

	amount := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)
	htlcAmt, totalTimelock, hops := generateHops(amount, testStartingHeight,
		n.firstBobChannelLink, n.carolChannelLink)

	invoice, err := n.makePayment(n.aliceServer, n.carolServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt, totalTimelock)
	if err == nil {
		t.Fatal("error haven't been received")
	}

	ferr, ok := err.(*ForwardingError)
	if !ok {
		t.Fatalf("expected a ForwardingError, instead got: %T", err)
	}

	switch ferr.FailureMessage.(type) {
	case *lnwire.FailInvalidOnionVersion:
	default:
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

// TestChannelLinkExpiryTooSoonExitNode tests that if we send an HTLC to a node
// with an expiry that is already expired, or too close to the current block
// height, then it will cancel the HTLC.
func TestChannelLinkExpiryTooSoonExitNode(t *testing.T) {
	t.Parallel()

	// The starting height for this test will be 200. So we'll base all
	// HTLC starting points off of that.
	const startingHeight = 200
	n := newThreeHopNetwork(t,
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5,
		startingHeight,
	)
	if err := n.start(); err != nil {
		t.Fatalf("unable to start three hop network: %v", err)
	}
	defer n.stop()

	amount := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)

	// We'll craft an HTLC packet, but set the starting height to 10 blocks
	// before the current true height.
	htlcAmt, totalTimelock, hops := generateHops(amount,
		startingHeight-10, n.firstBobChannelLink)

	// Now we'll send out the payment from Alice to Bob.
	_, err := n.makePayment(n.aliceServer, n.bobServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt, totalTimelock)

	// The payment should've failed as the time lock value was in the
	// _past_.
	if err == nil {
		t.Fatalf("payment should have failed due to a too early " +
			"time lock value")
	}

	ferr, ok := err.(*ForwardingError)
	if !ok {
		t.Fatalf("expected a ForwardingError, instead got: %T", err)
	}

	switch ferr.FailureMessage.(type) {
	case *lnwire.FailFinalIncorrectCltvExpiry:
	default:
		t.Fatalf("incorrect error, expected final time lock too "+
			"early, instead have: %v", err)
	}
}

// TestChannelLinkExpiryTooSoonExitNode tests that if we send a multi-hop HTLC,
// and the time lock is too early for an intermediate node, then they cancel
// the HTLC back to the sender.
func TestChannelLinkExpiryTooSoonMidNode(t *testing.T) {
	t.Parallel()

	// The starting height for this test will be 200. So we'll base all
	// HTLC starting points off of that.
	const startingHeight = 200
	n := newThreeHopNetwork(t,
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5,
		startingHeight,
	)
	if err := n.start(); err != nil {
		t.Fatalf("unable to start three hop network: %v", err)
	}
	defer n.stop()

	amount := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)

	// We'll craft an HTLC packet, but set the starting height to 10 blocks
	// before the current true height. The final route will be three hops,
	// so the middle hop should detect the issue.
	htlcAmt, totalTimelock, hops := generateHops(amount,
		startingHeight-10, n.firstBobChannelLink, n.carolChannelLink)

	// Now we'll send out the payment from Alice to Bob.
	_, err := n.makePayment(n.aliceServer, n.bobServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt, totalTimelock)

	// The payment should've failed as the time lock value was in the
	// _past_.
	if err == nil {
		t.Fatalf("payment should have failed due to a too early " +
			"time lock value")
	}

	ferr, ok := err.(*ForwardingError)
	if !ok {
		t.Fatalf("expected a ForwardingError, instead got: %T", err)
	}

	switch ferr.FailureMessage.(type) {
	case *lnwire.FailExpiryTooSoon:
	default:
		t.Fatalf("incorrect error, expected final time lock too "+
			"early, instead have: %v", err)
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
		testStartingHeight,
	)

	chanID := n.aliceChannelLink.ChanID()

	messages := []expectedMessage{
		{"alice", "bob", &lnwire.UpdateAddHTLC{}, false},
		{"alice", "bob", &lnwire.CommitSig{}, false},
		{"bob", "alice", &lnwire.RevokeAndAck{}, false},
		{"bob", "alice", &lnwire.CommitSig{}, false},
		{"alice", "bob", &lnwire.RevokeAndAck{}, false},

		{"bob", "alice", &lnwire.UpdateFufillHTLC{}, false},
		{"bob", "alice", &lnwire.CommitSig{}, false},
		{"alice", "bob", &lnwire.RevokeAndAck{}, false},
		{"alice", "bob", &lnwire.CommitSig{}, false},
		{"bob", "alice", &lnwire.RevokeAndAck{}, false},
	}

	debug := false
	if debug {
		// Log message that alice receives.
		n.aliceServer.intersect(createLogFunc("alice",
			n.aliceChannelLink.ChanID()))

		// Log message that bob receives.
		n.bobServer.intersect(createLogFunc("bob",
			n.firstBobChannelLink.ChanID()))
	}

	// Check that alice receives messages in right order.
	n.aliceServer.intersect(createInterceptorFunc("alice", messages, chanID,
		false))

	// Check that bob receives messages in right order.
	n.bobServer.intersect(createInterceptorFunc("bob", messages, chanID,
		false))

	if err := n.start(); err != nil {
		t.Fatalf("unable to start three hop network: %v", err)
	}
	defer n.stop()

	amount := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)
	htlcAmt, totalTimelock, hops := generateHops(amount, testStartingHeight,
		n.firstBobChannelLink)

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

type mockPeer struct {
	sync.Mutex
	sentMsgs []lnwire.Message
}

func (m *mockPeer) SendMessage(msg lnwire.Message) error {
	m.Lock()
	m.sentMsgs = append(m.sentMsgs, msg)
	m.Unlock()
	return nil
}
func (m *mockPeer) WipeChannel(*lnwallet.LightningChannel) error {
	return nil
}
func (m *mockPeer) PubKey() [33]byte {
	return [33]byte{}
}
func (m *mockPeer) Disconnect(reason error) {
}
func (m *mockPeer) popSentMsg() lnwire.Message {
	m.Lock()
	msg := m.sentMsgs[0]
	m.sentMsgs[0] = nil
	m.sentMsgs = m.sentMsgs[1:]
	m.Unlock()

	return msg
}

var _ Peer = (*mockPeer)(nil)

func newSingleLinkTestHarness(chanAmt btcutil.Amount) (ChannelLink, func(), error) {
	globalEpoch := &chainntnfs.BlockEpochEvent{
		Epochs: make(chan *chainntnfs.BlockEpoch),
		Cancel: func() {
		},
	}

	chanID := lnwire.NewShortChanIDFromInt(4)
	aliceChannel, _, fCleanUp, err := createTestChannel(
		alicePrivKey, bobPrivKey, chanAmt, chanAmt, chanID,
	)
	if err != nil {
		return nil, nil, err
	}

	var (
		invoiveRegistry = newMockRegistry()
		decoder         = &mockIteratorDecoder{}
		obfuscator      = newMockObfuscator()
		alicePeer       mockPeer

		globalPolicy = ForwardingPolicy{
			MinHTLC:       lnwire.NewMSatFromSatoshis(5),
			BaseFee:       lnwire.NewMSatFromSatoshis(1),
			TimeLockDelta: 6,
		}
	)

	aliceCfg := ChannelLinkConfig{
		FwrdingPolicy:     globalPolicy,
		Peer:              &alicePeer,
		Switch:            nil,
		DecodeHopIterator: decoder.DecodeHopIterator,
		DecodeOnionObfuscator: func(io.Reader) (ErrorEncrypter, lnwire.FailCode) {
			return obfuscator, lnwire.CodeNone
		},
		GetLastChannelUpdate: mockGetChanUpdateMessage,
		Registry:             invoiveRegistry,
		BlockEpochs:          globalEpoch,
	}

	const startingHeight = 100
	aliceLink := NewChannelLink(aliceCfg, aliceChannel, startingHeight)
	if err := aliceLink.Start(); err != nil {
		return nil, nil, err
	}

	cleanUp := func() {
		defer fCleanUp()
		defer aliceLink.Stop()
	}

	return aliceLink, cleanUp, nil
}

func assertLinkBandwidth(t *testing.T, link ChannelLink,
	expected lnwire.MilliSatoshi) {

	currentBandwidth := link.Bandwidth()
	_, _, line, _ := runtime.Caller(1)
	if currentBandwidth != expected {
		t.Fatalf("line %v: alice's link bandwidth is incorrect: "+
			"expected %v, got %v", line, expected, currentBandwidth)
	}
}

// TestChannelLinkBandwidthConsistency ensures that the reported bandwidth of a
// given ChannelLink is properly updated in response to downstream messages
// from the switch, and upstream messages from its channel peer.
//
// TODO(roasbeef): add sync hook into packet processing so can eliminate all
// sleep in this test and the one below
func TestChannelLinkBandwidthConsistency(t *testing.T) {
	t.Parallel()

	// We'll start the test by creating a single instance of
	const chanAmt = btcutil.SatoshiPerBitcoin * 5
	aliceLink, cleanUp, err := newSingleLinkTestHarness(chanAmt)
	if err != nil {
		t.Fatalf("unable to create link: %v", err)
	}
	defer cleanUp()

	var (
		mockBlob               [lnwire.OnionPacketSize]byte
		coreChan               = aliceLink.(*channelLink).channel
		defaultCommitFee       = coreChan.StateSnapshot().CommitFee
		aliceStartingBandwidth = aliceLink.Bandwidth()
	)

	// The starting bandwidth of the channel should be exactly the amount
	// that we created the channel between her and Bob.
	expectedBandwidth := lnwire.NewMSatFromSatoshis(chanAmt - defaultCommitFee)
	assertLinkBandwidth(t, aliceLink, expectedBandwidth)

	// Next, we'll create an HTLC worth 1 BTC, and send it into the link as
	// a switch initiated payment.  The resulting bandwidth should
	// now be decremented to reflect the new HTLC.
	htlcAmt := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)
	invoice, htlc, err := generatePayment(htlcAmt, htlcAmt, 5, mockBlob)
	if err != nil {
		t.Fatalf("unable to create payment: %v", err)
	}
	addPkt := htlcPacket{
		htlc: htlc,
	}
	aliceLink.HandleSwitchPacket(&addPkt)
	time.Sleep(time.Millisecond * 100)
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth-htlcAmt)

	// If we now send in a valid HTLC settle for the prior HTLC we added,
	// then the bandwidth should remain unchanged as the remote party will
	// gain additional channel balance.
	htlcSettle := &lnwire.UpdateFufillHTLC{
		ID:              0,
		PaymentPreimage: invoice.Terms.PaymentPreimage,
	}
	aliceLink.HandleChannelUpdate(htlcSettle)
	time.Sleep(time.Millisecond * 100)
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth-htlcAmt)

	// Next, we'll add another HTLC initiated by the switch (of the same
	// amount as the prior one).
	invoice, htlc, err = generatePayment(htlcAmt, htlcAmt, 5, mockBlob)
	if err != nil {
		t.Fatalf("unable to create payment: %v", err)
	}
	addPkt = htlcPacket{
		htlc: htlc,
	}
	aliceLink.HandleSwitchPacket(&addPkt)
	time.Sleep(time.Millisecond * 100)
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth-htlcAmt*2)

	// With that processed, we'll now generate an HTLC fail (sent by the
	// remote peer) to cancel the HTLC we just added. This should return us
	// back to the bandwidth of the link right before the HTLC was sent.
	failMsg := &lnwire.UpdateFailHTLC{
		ID:     1, // As this is the second HTLC.
		Reason: lnwire.OpaqueReason([]byte("nop")),
	}
	aliceLink.HandleChannelUpdate(failMsg)
	time.Sleep(time.Millisecond * 100)
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth-htlcAmt)

	// Moving along, we'll now receive a new HTLC from the remote peer,
	// with an ID of 0 as this is their first HTLC. The bandwidth should
	// remain unchanged.
	updateMsg := &lnwire.UpdateAddHTLC{
		Amount:      htlcAmt,
		Expiry:      9,
		PaymentHash: htlc.PaymentHash, // Re-using the same payment hash.
	}
	aliceLink.HandleChannelUpdate(updateMsg)
	time.Sleep(time.Millisecond * 100)
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth-htlcAmt)

	// Next, we'll settle the HTLC with our knowledge of the pre-image that
	// we eventually learn (simulating a multi-hop payment). The bandwidth
	// of the channel should now be re-balanced to the starting point.
	settlePkt := htlcPacket{
		htlc: &lnwire.UpdateFufillHTLC{
			ID:              2,
			PaymentPreimage: invoice.Terms.PaymentPreimage,
		},
	}
	aliceLink.HandleSwitchPacket(&settlePkt)
	time.Sleep(time.Millisecond * 100)
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth)

	// Finally, we'll test the scenario of failing an HTLC received by the
	// remote node. This should result in no perceived bandwidth changes.
	htlcAdd := &lnwire.UpdateAddHTLC{
		Amount:      htlcAmt,
		Expiry:      9,
		PaymentHash: htlc.PaymentHash,
	}
	aliceLink.HandleChannelUpdate(htlcAdd)
	time.Sleep(time.Millisecond * 100)
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth)
	failPkt := htlcPacket{
		htlc: &lnwire.UpdateFailHTLC{
			ID: 3,
		},
		payHash: htlc.PaymentHash,
	}
	aliceLink.HandleSwitchPacket(&failPkt)
	time.Sleep(time.Millisecond * 100)
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth)
}

// TestChannelLinkBandwidthConsistencyOverflow tests that in the case of a
// commitment overflow (no more space for new HTLC's), the bandwidth is updated
// properly as items are being added and removed from the overflow queue.
func TestChannelLinkBandwidthConsistencyOverflow(t *testing.T) {
	t.Parallel()

	var mockBlob [lnwire.OnionPacketSize]byte

	const chanAmt = btcutil.SatoshiPerBitcoin * 5
	aliceLink, cleanUp, err := newSingleLinkTestHarness(chanAmt)
	if err != nil {
		t.Fatalf("unable to create link: %v", err)
	}
	defer cleanUp()

	var (
		coreLink               = aliceLink.(*channelLink)
		aliceStartingBandwidth = aliceLink.Bandwidth()
	)

	addLinkHTLC := func(amt lnwire.MilliSatoshi) [32]byte {
		invoice, htlc, err := generatePayment(amt, amt, 5, mockBlob)
		if err != nil {
			t.Fatalf("unable to create payment: %v", err)
		}
		addPkt := htlcPacket{
			htlc: htlc,
		}
		aliceLink.HandleSwitchPacket(&addPkt)

		return invoice.Terms.PaymentPreimage
	}

	// We'll first start by adding enough HTLC's to overflow the commitment
	// transaction, checking the reported link bandwidth for proper
	// consistency along the way
	htlcAmt := lnwire.NewMSatFromSatoshis(100000)
	totalHtlcAmt := lnwire.MilliSatoshi(0)
	const numHTLCs = lnwallet.MaxHTLCNumber / 2
	var preImages [][32]byte
	for i := 0; i < numHTLCs; i++ {
		preImage := addLinkHTLC(htlcAmt)
		preImages = append(preImages, preImage)

		totalHtlcAmt += htlcAmt
	}

	time.Sleep(time.Millisecond * 100)
	expectedBandwidth := aliceStartingBandwidth - totalHtlcAmt
	assertLinkBandwidth(t, aliceLink, expectedBandwidth)

	// The overflow queue should be empty at this point, as the commitment
	// transaction should be full, but not yet overflown.
	if coreLink.overflowQueue.Length() != 0 {
		t.Fatalf("wrong overflow queue length: expected %v, got %v", 0,
			coreLink.overflowQueue.Length())
	}

	// At this point, the commitment transaction should now be fully
	// saturated. We'll continue adding HTLC's, and asserting that the
	// bandwidth account is done properly.
	const numOverFlowHTLCs = 20
	for i := 0; i < numOverFlowHTLCs; i++ {
		preImage := addLinkHTLC(htlcAmt)
		preImages = append(preImages, preImage)

		totalHtlcAmt += htlcAmt
	}

	time.Sleep(time.Millisecond * 100)
	expectedBandwidth = aliceStartingBandwidth - totalHtlcAmt
	assertLinkBandwidth(t, aliceLink, expectedBandwidth)

	aliceEndBandwidth := aliceLink.Bandwidth()

	// With the extra HTLC's added, the overflow queue should now be
	// populated with our 10 additional HTLC's.
	if coreLink.overflowQueue.Length() != numOverFlowHTLCs {
		t.Fatalf("wrong overflow queue length: expected %v, got %v",
			numOverFlowHTLCs,
			coreLink.overflowQueue.Length())
	}

	// At this point, we'll now settle one of the HTLC's that were added.
	// The resulting bandwidth change should be non-existent as this will
	// simply transfer over funds to the remote party. However, the size of
	// the overflow queue should be decreasing
	for i := 0; i < numOverFlowHTLCs; i++ {
		htlcSettle := &lnwire.UpdateFufillHTLC{
			ID:              uint64(i),
			PaymentPreimage: preImages[i],
		}

		aliceLink.HandleChannelUpdate(htlcSettle)
		time.Sleep(time.Millisecond * 50)
		assertLinkBandwidth(t, aliceLink, aliceEndBandwidth)

		// As we're not actually initiating a full state update, we'll
		// trigger a free-slot signal manually here.
		coreLink.overflowQueue.SignalFreeSlot()
	}

	// Finally, at this point, the queue itself should be fully empty. As
	// enough slots have been drained from the commitment transaction to
	// allocate the queue items to.
	time.Sleep(time.Millisecond * 100)
	if coreLink.overflowQueue.Length() != 0 {
		t.Fatalf("wrong overflow queue length: expected %v, got %v", 0,
			coreLink.overflowQueue.Length())
	}
}
