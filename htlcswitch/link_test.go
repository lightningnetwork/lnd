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
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

const (
	testStartingHeight = 100
)

// concurrentTester is a thread-safe wrapper around the Fatalf method of a
// *testing.T instance. With this wrapper multiple goroutines can safely
// attempt to fail a test concurrently.
type concurrentTester struct {
	mtx sync.Mutex
	*testing.T
}

func newConcurrentTester(t *testing.T) *concurrentTester {
	return &concurrentTester{
		T: t,
	}
}

func (c *concurrentTester) Fatalf(format string, args ...interface{}) {
	c.mtx.Lock()
	defer c.mtx.Unlock()

	c.T.Fatalf(format, args)
}

// messageToString is used to produce less spammy log messages in trace mode by
// setting the 'Curve" parameter to nil. Doing this avoids printing out each of
// the field elements in the curve parameters for secp256k1.
func messageToString(msg lnwire.Message) string {
	switch m := msg.(type) {
	case *lnwire.RevokeAndAck:
		m.NextRevocationKey.Curve = nil
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

// expectedMessage struct holds the message which travels from one peer to
// another, and additional information like, should this message we skipped for
// handling.
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
			fmt.Printf("---------------------- \n %v received: "+
				"%v", name, messageToString(m))
		}
		return false, nil
	}
}

// createInterceptorFunc creates the function by the given set of messages
// which, checks the order of the messages and skip the ones which were
// indicated to be intercepted.
func createInterceptorFunc(prefix, receiver string, messages []expectedMessage,
	chanID lnwire.ChannelID, debug bool) messageInterceptor {

	// Filter message which should be received with given peer name.
	var expectToReceive []expectedMessage
	for _, message := range messages {
		if message.to == receiver {
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
				return false, errors.Errorf("%v received "+
					"unexpected message out of range: %v",
					receiver, m.MsgType())
			}

			expectedMessage := expectToReceive[0]
			expectToReceive = expectToReceive[1:]

			if expectedMessage.message.MsgType() != m.MsgType() {
				return false, errors.Errorf("%v received wrong message: \n"+
					"real: %v\nexpected: %v", receiver, m.MsgType(),
					expectedMessage.message.MsgType())
			}

			if debug {
				var postfix string
				if revocation, ok := m.(*lnwire.RevokeAndAck); ok {
					var zeroHash chainhash.Hash
					if bytes.Equal(zeroHash[:], revocation.Revocation[:]) {
						postfix = "- empty revocation"
					}
				}

				if expectedMessage.skip {
					fmt.Printf("skipped: %v: %v %v \n", prefix,
						m.MsgType(), postfix)
				} else {
					fmt.Printf("%v: %v %v \n", prefix, m.MsgType(), postfix)
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

	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5)
	if err != nil {
		t.Fatalf("unable to create channel: %v", err)
	}
	defer cleanUp()

	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight)
	if err := n.start(); err != nil {
		t.Fatal(err)
	}
	defer n.stop()

	aliceBandwidthBefore := n.aliceChannelLink.Bandwidth()
	bobBandwidthBefore := n.firstBobChannelLink.Bandwidth()

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
	receiver := n.bobServer
	rhash, err := n.makePayment(n.aliceServer, receiver,
		n.bobServer.PubKey(), hops, amount, htlcAmt,
		totalTimelock).Wait(30 * time.Second)
	if err != nil {
		t.Fatalf("unable to make the payment: %v", err)
	}

	// Wait for Bob to receive the revocation.
	//
	// TODO(roasbeef); replace with select over returned err chan
	time.Sleep(100 * time.Millisecond)

	// Check that alice invoice was settled and bandwidth of HTLC
	// links was changed.
	invoice, err := receiver.registry.LookupInvoice(rhash)
	if err != nil {
		t.Fatalf("unable to get invoice: %v", err)
	}
	if !invoice.Terms.Settled {
		t.Fatal("alice invoice wasn't settled")
	}

	if aliceBandwidthBefore-amount != n.aliceChannelLink.Bandwidth() {
		t.Fatal("alice bandwidth should have decrease on payment " +
			"amount")
	}

	if bobBandwidthBefore+amount != n.firstBobChannelLink.Bandwidth() {
		t.Fatalf("bob bandwidth isn't match: expected %v, got %v",
			bobBandwidthBefore+amount,
			n.firstBobChannelLink.Bandwidth())
	}
}

// TestChannelLinkBidirectionalOneHopPayments tests the ability of channel
// link to cope with bigger number of payment updates that commitment
// transaction may consist.
func TestChannelLinkBidirectionalOneHopPayments(t *testing.T) {
	t.Parallel()

	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5)
	if err != nil {
		t.Fatalf("unable to create channel: %v", err)
	}
	defer cleanUp()

	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight)
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
				totalTimelock).Wait(5 * time.Minute)
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
				totalTimelock).Wait(5 * time.Minute)
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
				t.Fatalf("unable to make payment: %v", r.err)
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

	// TODO(roasbeef): should instead consume async notifications from both
	// links
	time.Sleep(time.Second * 2)

	// At the end Bob and Alice balances should be the same as previous,
	// because they sent the equal amount of money to each other.
	if aliceBandwidthBefore != n.aliceChannelLink.Bandwidth() {
		t.Fatalf("alice bandwidth shouldn't have changed: expected %v, got %x",
			aliceBandwidthBefore, n.aliceChannelLink.Bandwidth())
	}

	if bobBandwidthBefore != n.firstBobChannelLink.Bandwidth() {
		t.Fatalf("bob bandwidth shouldn't have changed: expected %v, got %v",
			bobBandwidthBefore, n.firstBobChannelLink.Bandwidth())
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

	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5)
	if err != nil {
		t.Fatalf("unable to create channel: %v", err)
	}
	defer cleanUp()

	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight)
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
	receiver := n.carolServer
	rhash, err := n.makePayment(n.aliceServer, n.carolServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt,
		totalTimelock).Wait(30 * time.Second)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// Wait for Bob to receive the revocation.
	time.Sleep(100 * time.Millisecond)

	// Check that Carol invoice was settled and bandwidth of HTLC
	// links were changed.
	invoice, err := receiver.registry.LookupInvoice(rhash)
	if err != nil {
		t.Fatalf("unable to get invoice: %v", err)
	}
	if !invoice.Terms.Settled {
		t.Fatal("carol invoice haven't been settled")
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

	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*5,
		btcutil.SatoshiPerBitcoin*5)
	if err != nil {
		t.Fatalf("unable to create channel: %v", err)
	}
	defer cleanUp()

	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight)
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

	_, err = n.makePayment(n.aliceServer, n.bobServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt,
		htlcExpiry).Wait(30 * time.Second)
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

	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*5,
		btcutil.SatoshiPerBitcoin*5)
	if err != nil {
		t.Fatalf("unable to create channel: %v", err)
	}
	defer cleanUp()

	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight)
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

	_, err = n.makePayment(n.aliceServer, n.bobServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt,
		htlcExpiry).Wait(30 * time.Second)
	if err == nil {
		t.Fatalf("payment should have failed but didn't")
	} else if err.Error() != lnwire.CodeIncorrectPaymentAmount.String() {
		// TODO(roasbeef): use proper error after error propagation is
		// in
		t.Fatalf("incorrect error, expected insufficient value, "+
			"instead have: %v", err)
	}
}

// TestLinkForwardTimelockPolicyMismatch tests that if a node is an
// intermediate node in a multi-hop payment, and receives an HTLC which
// violates its specified multi-hop policy, then the HTLC is rejected.
func TestLinkForwardTimelockPolicyMismatch(t *testing.T) {
	t.Parallel()

	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*5,
		btcutil.SatoshiPerBitcoin*5)
	if err != nil {
		t.Fatalf("unable to create channel: %v", err)
	}
	defer cleanUp()

	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight)
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
	_, err = n.makePayment(n.aliceServer, n.carolServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt,
		htlcExpiry).Wait(30 * time.Second)
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

	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5)
	if err != nil {
		t.Fatalf("unable to create channel: %v", err)
	}
	defer cleanUp()

	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight)
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
	_, err = n.makePayment(n.aliceServer, n.bobServer,
		n.bobServer.PubKey(), hops, amountNoFee, amountNoFee,
		htlcExpiry).Wait(30 * time.Second)

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

	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*5,
		btcutil.SatoshiPerBitcoin*5)
	if err != nil {
		t.Fatalf("unable to create channel: %v", err)
	}
	defer cleanUp()

	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight)
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
	_, err = n.makePayment(n.aliceServer, n.bobServer,
		n.bobServer.PubKey(), hops, amountNoFee, htlcAmt,
		htlcExpiry).Wait(30 * time.Second)

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

	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*5,
		btcutil.SatoshiPerBitcoin*5)
	if err != nil {
		t.Fatalf("unable to create channel: %v", err)
	}
	defer cleanUp()

	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight)
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
	payResp, err := n.makePayment(n.aliceServer, n.carolServer,
		n.bobServer.PubKey(), hops, amountNoFee, htlcAmt,
		htlcExpiry).Wait(30 * time.Second)
	if err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}

	// Carol's invoice should now be shown as settled as the payment
	// succeeded.
	invoice, err := n.carolServer.registry.LookupInvoice(payResp)
	if err != nil {
		t.Fatalf("unable to get invoice: %v", err)
	}
	if !invoice.Terms.Settled {
		t.Fatal("carol invoice haven't been settled")
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
		htlcExpiry).Wait(30 * time.Second)
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

	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5)
	if err != nil {
		t.Fatalf("unable to create channel: %v", err)
	}
	defer cleanUp()

	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight)
	if err := n.start(); err != nil {
		t.Fatalf("unable to start three hop network: %v", err)
	}
	defer n.stop()

	carolBandwidthBefore := n.carolChannelLink.Bandwidth()
	firstBobBandwidthBefore := n.firstBobChannelLink.Bandwidth()
	secondBobBandwidthBefore := n.secondBobChannelLink.Bandwidth()
	aliceBandwidthBefore := n.aliceChannelLink.Bandwidth()

	// We'll attempt to send 4 BTC although the alice-to-bob channel only
	// has 3 BTC total capacity. As a result, this payment should be
	// rejected.
	amount := lnwire.NewMSatFromSatoshis(4 * btcutil.SatoshiPerBitcoin)
	htlcAmt, totalTimelock, hops := generateHops(amount, testStartingHeight,
		n.firstBobChannelLink, n.carolChannelLink)

	// Wait for:
	// * HTLC add request to be sent to from Alice to Bob.
	// * Alice<->Bob commitment states to be updated.
	// * Bob trying to add HTLC add request in Bob<->Carol channel.
	// * Cancel HTLC request to be sent back from Bob to Alice.
	// * user notification to be sent.

	receiver := n.carolServer
	rhash, err := n.makePayment(n.aliceServer, n.carolServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt,
		totalTimelock).Wait(30 * time.Second)
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
	invoice, err := receiver.registry.LookupInvoice(rhash)
	if err != nil {
		t.Fatalf("unable to get invoice: %v", err)
	}
	if invoice.Terms.Settled {
		t.Fatal("carol invoice have been settled")
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

	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*5,
		btcutil.SatoshiPerBitcoin*5)
	if err != nil {
		t.Fatalf("unable to create channel: %v", err)
	}
	defer cleanUp()

	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight)
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
	if err := n.carolServer.registry.AddInvoice(*invoice); err != nil {
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

	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*5,
		btcutil.SatoshiPerBitcoin*5)
	if err != nil {
		t.Fatalf("unable to create channel: %v", err)
	}
	defer cleanUp()

	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight)
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

	davePub := newMockServer(t, "dave").PubKey()
	receiver := n.bobServer
	rhash, err := n.makePayment(n.aliceServer, n.bobServer, davePub, hops,
		amount, htlcAmt, totalTimelock).Wait(30 * time.Second)
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
	invoice, err := receiver.registry.LookupInvoice(rhash)
	if err != nil {
		t.Fatalf("unable to get invoice: %v", err)
	}
	if invoice.Terms.Settled {
		t.Fatal("carol invoice have been settled")
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

	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5)
	if err != nil {
		t.Fatalf("unable to create channel: %v", err)
	}
	defer cleanUp()

	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight)
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

	receiver := n.carolServer
	rhash, err := n.makePayment(n.aliceServer, n.carolServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt,
		totalTimelock).Wait(30 * time.Second)
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
	invoice, err := receiver.registry.LookupInvoice(rhash)
	if err != nil {
		t.Fatalf("unable to get invoice: %v", err)
	}
	if invoice.Terms.Settled {
		t.Fatal("carol invoice have been settled")
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
	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5)
	if err != nil {
		t.Fatalf("unable to create channel: %v", err)
	}
	defer cleanUp()

	const startingHeight = 200
	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, startingHeight)
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
	_, err = n.makePayment(n.aliceServer, n.bobServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt,
		totalTimelock).Wait(30 * time.Second)

	// The payment should've failed as the time lock value was in the
	// _past_.
	if err == nil {
		t.Fatalf("payment should have failed due to a too early " +
			"time lock value")
	}

	ferr, ok := err.(*ForwardingError)
	if !ok {
		t.Fatalf("expected a ForwardingError, instead got: %T %v",
			err, err)
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
	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5)
	if err != nil {
		t.Fatalf("unable to create channel: %v", err)
	}
	defer cleanUp()

	const startingHeight = 200
	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, startingHeight)
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
	_, err = n.makePayment(n.aliceServer, n.bobServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt,
		totalTimelock).Wait(30 * time.Second)

	// The payment should've failed as the time lock value was in the
	// _past_.
	if err == nil {
		t.Fatalf("payment should have failed due to a too early " +
			"time lock value")
	}

	ferr, ok := err.(*ForwardingError)
	if !ok {
		t.Fatalf("expected a ForwardingError, instead got: %T: %v", err, err)
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

	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5)
	if err != nil {
		t.Fatalf("unable to create channel: %v", err)
	}
	defer cleanUp()

	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight)

	chanID := n.aliceChannelLink.ChanID()

	messages := []expectedMessage{
		{"alice", "bob", &lnwire.ChannelReestablish{}, false},
		{"bob", "alice", &lnwire.ChannelReestablish{}, false},

		{"alice", "bob", &lnwire.FundingLocked{}, false},
		{"bob", "alice", &lnwire.FundingLocked{}, false},

		{"alice", "bob", &lnwire.UpdateAddHTLC{}, false},
		{"alice", "bob", &lnwire.CommitSig{}, false},
		{"bob", "alice", &lnwire.RevokeAndAck{}, false},
		{"bob", "alice", &lnwire.CommitSig{}, false},
		{"alice", "bob", &lnwire.RevokeAndAck{}, false},

		{"bob", "alice", &lnwire.UpdateFulfillHTLC{}, false},
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
	n.aliceServer.intersect(createInterceptorFunc("[alice] <-- [bob]",
		"alice", messages, chanID, false))

	// Check that bob receives messages in right order.
	n.bobServer.intersect(createInterceptorFunc("[alice] --> [bob]",
		"bob", messages, chanID, false))

	if err := n.start(); err != nil {
		t.Fatalf("unable to start three hop network: %v", err)
	}
	defer n.stop()

	amount := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)
	htlcAmt, totalTimelock, hops := generateHops(amount, testStartingHeight,
		n.firstBobChannelLink)

	// Wait for:
	// * HTLC add request to be sent to bob.
	// * alice<->bob commitment state to be updated.
	// * settle request to be sent back from bob to alice.
	// * alice<->bob commitment state to be updated.
	// * user notification to be sent.
	_, err = n.makePayment(n.aliceServer, n.bobServer,
		n.bobServer.PubKey(), hops, amount, htlcAmt,
		totalTimelock).Wait(30 * time.Second)
	if err != nil {
		t.Fatalf("unable to make the payment: %v", err)
	}
}

type mockPeer struct {
	sync.Mutex
	sentMsgs chan lnwire.Message
	quit     chan struct{}
}

func (m *mockPeer) SendMessage(msg lnwire.Message) error {
	select {
	case m.sentMsgs <- msg:
	case <-m.quit:
		return fmt.Errorf("mockPeer shutting down")
	}
	return nil
}
func (m *mockPeer) WipeChannel(*wire.OutPoint) error {
	return nil
}
func (m *mockPeer) PubKey() [33]byte {
	return [33]byte{}
}
func (m *mockPeer) Disconnect(reason error) {
}

var _ Peer = (*mockPeer)(nil)

func newSingleLinkTestHarness(chanAmt btcutil.Amount) (ChannelLink,
	*lnwallet.LightningChannel, chan time.Time, func(), error) {
	globalEpoch := &chainntnfs.BlockEpochEvent{
		Epochs: make(chan *chainntnfs.BlockEpoch),
		Cancel: func() {
		},
	}

	chanID := lnwire.NewShortChanIDFromInt(4)
	aliceChannel, bobChannel, fCleanUp, _, err := createTestChannel(
		alicePrivKey, bobPrivKey, chanAmt, chanAmt, chanID,
	)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	var (
		invoiceRegistry = newMockRegistry()
		decoder         = &mockIteratorDecoder{}
		obfuscator      = newMockObfuscator()
		alicePeer       = &mockPeer{
			sentMsgs: make(chan lnwire.Message, 2000),
			quit:     make(chan struct{}),
		}

		globalPolicy = ForwardingPolicy{
			MinHTLC:       lnwire.NewMSatFromSatoshis(5),
			BaseFee:       lnwire.NewMSatFromSatoshis(1),
			TimeLockDelta: 6,
		}
	)

	pCache := &mockPreimageCache{
		// hash -> preimage
		preimageMap: make(map[[32]byte][]byte),
	}

	t := make(chan time.Time)
	ticker := &mockTicker{t}
	aliceCfg := ChannelLinkConfig{
		FwrdingPolicy:     globalPolicy,
		Peer:              alicePeer,
		Switch:            New(Config{}),
		DecodeHopIterator: decoder.DecodeHopIterator,
		DecodeOnionObfuscator: func(io.Reader) (ErrorEncrypter, lnwire.FailCode) {
			return obfuscator, lnwire.CodeNone
		},
		GetLastChannelUpdate: mockGetChanUpdateMessage,
		PreimageCache:        pCache,
		UpdateContractSignals: func(*contractcourt.ContractSignals) error {
			return nil
		},
		Registry:    invoiceRegistry,
		ChainEvents: &contractcourt.ChainEventSubscription{},
		BlockEpochs: globalEpoch,
		BatchTicker: ticker,
		// Make the BatchSize large enough to not
		// trigger commit update automatically during tests.
		BatchSize: 10000,
	}

	const startingHeight = 100
	aliceLink := NewChannelLink(aliceCfg, aliceChannel, startingHeight)
	if err := aliceLink.Start(); err != nil {
		return nil, nil, nil, nil, err
	}
	go func() {
		for {
			select {
			case <-aliceLink.(*channelLink).htlcUpdates:
			case <-aliceLink.(*channelLink).quit:
				return
			}
		}
	}()

	cleanUp := func() {
		close(alicePeer.quit)
		defer fCleanUp()
		defer aliceLink.Stop()
		defer bobChannel.Stop()
	}

	return aliceLink, bobChannel, t, cleanUp, nil
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

// handleStateUpdate handles the messages sent from the link after
// the batch ticker has triggered a state update.
func handleStateUpdate(link *channelLink,
	remoteChannel *lnwallet.LightningChannel) error {
	sentMsgs := link.cfg.Peer.(*mockPeer).sentMsgs
	var msg lnwire.Message
	select {
	case msg = <-sentMsgs:
	case <-time.After(20 * time.Second):
		return fmt.Errorf("did not receive CommitSig from Alice")
	}

	// The link should be sending a commit sig at this point.
	commitSig, ok := msg.(*lnwire.CommitSig)
	if !ok {
		return fmt.Errorf("expected CommitSig, got %T", msg)
	}

	// Let the remote channel receive the commit sig, and
	// respond with a revocation + commitsig.
	err := remoteChannel.ReceiveNewCommitment(
		commitSig.CommitSig, commitSig.HtlcSigs)
	if err != nil {
		return err
	}

	remoteRev, _, err := remoteChannel.RevokeCurrentCommitment()
	if err != nil {
		return err
	}
	link.HandleChannelUpdate(remoteRev)

	remoteSig, remoteHtlcSigs, err := remoteChannel.SignNextCommitment()
	if err != nil {
		return err
	}
	commitSig = &lnwire.CommitSig{
		CommitSig: remoteSig,
		HtlcSigs:  remoteHtlcSigs,
	}
	link.HandleChannelUpdate(commitSig)

	// This should make the link respond with a revocation.
	select {
	case msg = <-sentMsgs:
	case <-time.After(20 * time.Second):
		return fmt.Errorf("did not receive RevokeAndAck from Alice")
	}

	revoke, ok := msg.(*lnwire.RevokeAndAck)
	if !ok {
		return fmt.Errorf("expected RevokeAndAck got %T", msg)
	}
	_, err = remoteChannel.ReceiveRevocation(revoke)
	if err != nil {
		return fmt.Errorf("unable to recieve "+
			"revocation: %v", err)
	}

	return nil
}

// updateState is used exchange the messages necessary to do a full state
// transition. If initiateUpdate=true, then this call will make the link
// trigger an update by sending on the batchTick channel, if not, it will
// make the remoteChannel initiate the state update.
func updateState(batchTick chan time.Time, link *channelLink,
	remoteChannel *lnwallet.LightningChannel,
	initiateUpdate bool) error {
	sentMsgs := link.cfg.Peer.(*mockPeer).sentMsgs

	if initiateUpdate {
		// Trigger update by ticking the batchTicker.
		select {
		case batchTick <- time.Now():
		case <-link.quit:
			return fmt.Errorf("link shuttin down")
		}
		return handleStateUpdate(link, remoteChannel)
	}

	// The remote is triggering the state update, emulate this by
	// signing and sending CommitSig to the link.
	remoteSig, remoteHtlcSigs, err := remoteChannel.SignNextCommitment()
	if err != nil {
		return err
	}

	commitSig := &lnwire.CommitSig{
		CommitSig: remoteSig,
		HtlcSigs:  remoteHtlcSigs,
	}
	link.HandleChannelUpdate(commitSig)

	// The link should respond with a revocation + commit sig.
	var msg lnwire.Message
	select {
	case msg = <-sentMsgs:
	case <-time.After(20 * time.Second):
		return fmt.Errorf("did not receive RevokeAndAck from Alice")
	}

	revoke, ok := msg.(*lnwire.RevokeAndAck)
	if !ok {
		return fmt.Errorf("expected RevokeAndAck got %T",
			msg)
	}
	_, err = remoteChannel.ReceiveRevocation(revoke)
	if err != nil {
		return fmt.Errorf("unable to recieve "+
			"revocation: %v", err)
	}
	select {
	case msg = <-sentMsgs:
	case <-time.After(20 * time.Second):
		return fmt.Errorf("did not receive CommitSig from Alice")
	}

	commitSig, ok = msg.(*lnwire.CommitSig)
	if !ok {
		return fmt.Errorf("expected CommitSig, got %T", msg)
	}

	err = remoteChannel.ReceiveNewCommitment(
		commitSig.CommitSig, commitSig.HtlcSigs)
	if err != nil {
		return err
	}

	// Lastly, send a revocation back to the link.
	remoteRev, _, err := remoteChannel.RevokeCurrentCommitment()
	if err != nil {
		return err
	}
	link.HandleChannelUpdate(remoteRev)

	// Sleep to make sure Alice has handled the remote revocation.
	time.Sleep(500 * time.Millisecond)

	return nil
}

// TestChannelLinkBandwidthConsistency ensures that the reported bandwidth of a
// given ChannelLink is properly updated in response to downstream messages
// from the switch, and upstream messages from its channel peer.
//
// TODO(roasbeef): add sync hook into packet processing so can eliminate all
// sleep in this test and the one below
func TestChannelLinkBandwidthConsistency(t *testing.T) {
	t.Parallel()

	// TODO(roasbeef): replace manual bit twiddling with concept of
	// resource cost for packets?
	//  * or also able to consult link

	// We'll start the test by creating a single instance of
	const chanAmt = btcutil.SatoshiPerBitcoin * 5
	link, bobChannel, tmr, cleanUp, err := newSingleLinkTestHarness(chanAmt)
	if err != nil {
		t.Fatalf("unable to create link: %v", err)
	}
	defer cleanUp()

	var (
		mockBlob               [lnwire.OnionPacketSize]byte
		aliceLink              = link.(*channelLink)
		aliceChannel           = aliceLink.channel
		defaultCommitFee       = aliceChannel.StateSnapshot().CommitFee
		aliceStartingBandwidth = aliceLink.Bandwidth()
		aliceMsgs              = aliceLink.cfg.Peer.(*mockPeer).sentMsgs
	)

	// We put Alice into HodlHTLC mode, such that she won't settle
	// incoming HTLCs automatically.
	aliceLink.cfg.HodlHTLC = true
	aliceLink.cfg.DebugHTLC = true

	estimator := &lnwallet.StaticFeeEstimator{
		FeeRate: 24,
	}
	feePerWeight, err := estimator.EstimateFeePerWeight(1)
	if err != nil {
		t.Fatalf("unable to query fee estimator: %v", err)
	}
	feePerKw := feePerWeight * 1000
	htlcFee := lnwire.NewMSatFromSatoshis(
		btcutil.Amount((int64(feePerKw) * lnwallet.HtlcWeight) / 1000),
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
	time.Sleep(time.Millisecond * 500)

	// The resulting bandwidth should reflect that Alice is paying the
	// htlc amount in addition to the htlc fee.
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth-htlcAmt-htlcFee)

	// Alice should send the HTLC to Bob.
	var msg lnwire.Message
	select {
	case msg = <-aliceMsgs:
	case <-time.After(2 * time.Second):
		t.Fatalf("did not receive message")
	}

	addHtlc, ok := msg.(*lnwire.UpdateAddHTLC)
	if !ok {
		t.Fatalf("expected UpdateAddHTLC, got %T", msg)
	}

	bobIndex, err := bobChannel.ReceiveHTLC(addHtlc)
	if err != nil {
		t.Fatalf("bob failed receiving htlc: %v", err)
	}

	// Lock in the HTLC.
	if err := updateState(tmr, aliceLink, bobChannel, true); err != nil {
		t.Fatalf("unable to update state: %v", err)
	}

	// Locking in the HTLC should not change Alice's bandwidth.
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth-htlcAmt-htlcFee)

	// If we now send in a valid HTLC settle for the prior HTLC we added,
	// then the bandwidth should remain unchanged as the remote party will
	// gain additional channel balance.
	err = bobChannel.SettleHTLC(invoice.Terms.PaymentPreimage, bobIndex)
	if err != nil {
		t.Fatalf("unable to settle htlc: %v", err)
	}
	htlcSettle := &lnwire.UpdateFulfillHTLC{
		ID:              bobIndex,
		PaymentPreimage: invoice.Terms.PaymentPreimage,
	}
	aliceLink.HandleChannelUpdate(htlcSettle)
	time.Sleep(time.Millisecond * 500)

	// Since the settle is not locked in yet, Alice's bandwidth should still
	// reflect that she has to pay the fee.
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth-htlcAmt-htlcFee)

	// Lock in the settle.
	if err := updateState(tmr, aliceLink, bobChannel, false); err != nil {
		t.Fatalf("unable to update state: %v", err)
	}

	// Now that it is settled, Alice should have gotten the htlc fee back.
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
	time.Sleep(time.Millisecond * 500)

	// Again, Alice's bandwidth decreases by htlcAmt+htlcFee.
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth-2*htlcAmt-htlcFee)

	// Alice will send the HTLC to Bob.
	select {
	case msg = <-aliceMsgs:
	case <-time.After(2 * time.Second):
		t.Fatalf("did not receive message")
	}
	addHtlc, ok = msg.(*lnwire.UpdateAddHTLC)
	if !ok {
		t.Fatalf("expected UpdateAddHTLC, got %T", msg)
	}

	bobIndex, err = bobChannel.ReceiveHTLC(addHtlc)
	if err != nil {
		t.Fatalf("bob failed receiving htlc: %v", err)
	}

	// Lock in the HTLC, which should not affect the bandwidth.
	if err := updateState(tmr, aliceLink, bobChannel, true); err != nil {
		t.Fatalf("unable to update state: %v", err)
	}

	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth-htlcAmt*2-htlcFee)

	// With that processed, we'll now generate an HTLC fail (sent by the
	// remote peer) to cancel the HTLC we just added. This should return us
	// back to the bandwidth of the link right before the HTLC was sent.
	err = bobChannel.FailHTLC(bobIndex, []byte("nop"))
	if err != nil {
		t.Fatalf("unable to fail htlc: %v", err)
	}
	failMsg := &lnwire.UpdateFailHTLC{
		ID:     bobIndex,
		Reason: lnwire.OpaqueReason([]byte("nop")),
	}
	aliceLink.HandleChannelUpdate(failMsg)
	time.Sleep(time.Millisecond * 500)

	// Before the Fail gets locked in, the bandwidth should remain unchanged.
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth-htlcAmt*2-htlcFee)

	// Lock in the Fail.
	if err := updateState(tmr, aliceLink, bobChannel, false); err != nil {
		t.Fatalf("unable to update state: %v", err)
	}

	// Now the bancdwidth should reflect the failed HTLC.
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth-htlcAmt)

	// Moving along, we'll now receive a new HTLC from the remote peer,
	// with an ID of 0 as this is their first HTLC. The bandwidth should
	// remain unchanged (but Alice will need to pay the fee for the extra
	// HTLC).
	htlcAmt, totalTimelock, hops := generateHops(htlcAmt, testStartingHeight,
		aliceLink)
	blob, err := generateRoute(hops...)
	if err != nil {
		t.Fatalf("unable to gen route: %v", err)
	}
	invoice, htlc, err = generatePayment(htlcAmt, htlcAmt,
		totalTimelock, blob)
	if err != nil {
		t.Fatalf("unable to create payment: %v", err)
	}

	// We must add the invoice to the registry, such that Alice expects
	// this payment.
	err = aliceLink.cfg.Registry.(*mockInvoiceRegistry).AddInvoice(*invoice)
	if err != nil {
		t.Fatalf("unable to add invoice to registry: %v", err)
	}

	bobIndex, err = bobChannel.AddHTLC(htlc)
	if err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
	aliceLink.HandleChannelUpdate(htlc)

	// Alice's balance remains unchanged until this HTLC is locked in.
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth-htlcAmt)

	// Lock in the HTLC.
	if err := updateState(tmr, aliceLink, bobChannel, false); err != nil {
		t.Fatalf("unable to update state: %v", err)
	}

	// Since Bob is adding this HTLC, Alice only needs to pay the fee.
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth-htlcAmt-htlcFee)

	// Next, we'll settle the HTLC with our knowledge of the pre-image that
	// we eventually learn (simulating a multi-hop payment). The bandwidth
	// of the channel should now be re-balanced to the starting point.
	settlePkt := htlcPacket{
		htlc: &lnwire.UpdateFulfillHTLC{
			ID:              bobIndex,
			PaymentPreimage: invoice.Terms.PaymentPreimage,
		},
	}

	aliceLink.HandleSwitchPacket(&settlePkt)
	time.Sleep(time.Millisecond * 500)

	// Settling this HTLC gives Alice all her original bandwidth back.
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth)

	// Alice wil send the Settle to Bob.
	select {
	case msg = <-aliceMsgs:
	case <-time.After(2 * time.Second):
		t.Fatalf("did not receive message")
	}

	settleHtlc, ok := msg.(*lnwire.UpdateFulfillHTLC)
	if !ok {
		t.Fatalf("expected UpdateFulfillHTLC, got %T", msg)
	}
	pre := settleHtlc.PaymentPreimage
	idx := settleHtlc.ID
	err = bobChannel.ReceiveHTLCSettle(pre, idx)
	if err != nil {
		t.Fatalf("unable to receive settle: %v", err)
	}

	// After a settle the link should do a state transition automatically,
	// so we don't have to trigger it.
	if err := handleStateUpdate(aliceLink, bobChannel); err != nil {
		t.Fatalf("unable to update state: %v", err)
	}
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth)

	// Finally, we'll test the scenario of failing an HTLC received from the
	// remote node. This should result in no perceived bandwidth changes.
	htlcAmt, totalTimelock, hops = generateHops(htlcAmt, testStartingHeight,
		aliceLink)
	blob, err = generateRoute(hops...)
	if err != nil {
		t.Fatalf("unable to gen route: %v", err)
	}
	invoice, htlc, err = generatePayment(htlcAmt, htlcAmt, totalTimelock, blob)
	if err != nil {
		t.Fatalf("unable to create payment: %v", err)
	}
	if err := aliceLink.cfg.Registry.(*mockInvoiceRegistry).AddInvoice(*invoice); err != nil {
		t.Fatalf("unable to add invoice to registry: %v", err)
	}

	// Since we are not using the link to handle HTLC IDs for the
	// remote channel, we must set this manually. This is the second
	// HTLC we add, hence it should have an ID of 1 (Alice's channel
	// link will set this automatically for her side).
	htlc.ID = 1
	bobIndex, err = bobChannel.AddHTLC(htlc)
	if err != nil {
		t.Fatalf("unable to add htlc: %v", err)
	}
	aliceLink.HandleChannelUpdate(htlc)
	time.Sleep(time.Millisecond * 500)

	// No changes before the HTLC is locked in.
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth)
	if err := updateState(tmr, aliceLink, bobChannel, false); err != nil {
		t.Fatalf("unable to update state: %v", err)
	}

	// After lock-in, Alice will have to pay the htlc fee.
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth-htlcFee)

	// Now fail this HTLC.
	failPkt := htlcPacket{
		incomingHTLCID: bobIndex,
		htlc: &lnwire.UpdateFailHTLC{
			ID: bobIndex,
		},
	}
	aliceLink.HandleSwitchPacket(&failPkt)
	time.Sleep(time.Millisecond * 500)

	// Alice should get all her bandwidth back.
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth)

	// Message should be sent to Bob.
	select {
	case msg = <-aliceMsgs:
	case <-time.After(2 * time.Second):
		t.Fatalf("did not receive message")
	}
	failMsg, ok = msg.(*lnwire.UpdateFailHTLC)
	if !ok {
		t.Fatalf("expected UpdateFailHTLC, got %T", msg)
	}
	err = bobChannel.ReceiveFailHTLC(failMsg.ID, []byte("fail"))
	if err != nil {
		t.Fatalf("failed receiving fail htlc: %v", err)
	}

	// After failing an HTLC, the link will automatically trigger
	// a state update.
	if err := handleStateUpdate(aliceLink, bobChannel); err != nil {
		t.Fatalf("unable to update state: %v", err)
	}
	assertLinkBandwidth(t, aliceLink, aliceStartingBandwidth)
}

// TestChannelLinkBandwidthConsistencyOverflow tests that in the case of a
// commitment overflow (no more space for new HTLC's), the bandwidth is updated
// properly as items are being added and removed from the overflow queue.
func TestChannelLinkBandwidthConsistencyOverflow(t *testing.T) {
	t.Parallel()

	var mockBlob [lnwire.OnionPacketSize]byte

	const chanAmt = btcutil.SatoshiPerBitcoin * 5
	aliceLink, bobChannel, batchTick, cleanUp, err := newSingleLinkTestHarness(chanAmt)
	if err != nil {
		t.Fatalf("unable to create link: %v", err)
	}
	defer cleanUp()

	var (
		coreLink               = aliceLink.(*channelLink)
		defaultCommitFee       = coreLink.channel.StateSnapshot().CommitFee
		aliceStartingBandwidth = aliceLink.Bandwidth()
		aliceMsgs              = coreLink.cfg.Peer.(*mockPeer).sentMsgs
	)

	estimator := &lnwallet.StaticFeeEstimator{
		FeeRate: 24,
	}
	feePerWeight, err := estimator.EstimateFeePerWeight(1)
	if err != nil {
		t.Fatalf("unable to query fee estimator: %v", err)
	}
	feePerKw := feePerWeight * 1000

	// The starting bandwidth of the channel should be exactly the amount
	// that we created the channel between her and Bob.
	expectedBandwidth := lnwire.NewMSatFromSatoshis(chanAmt - defaultCommitFee)
	assertLinkBandwidth(t, aliceLink, expectedBandwidth)

	addLinkHTLC := func(amt lnwire.MilliSatoshi) [32]byte {
		invoice, htlc, err := generatePayment(amt, amt, 5, mockBlob)
		if err != nil {
			t.Fatalf("unable to create payment: %v", err)
		}
		aliceLink.HandleSwitchPacket(&htlcPacket{
			htlc:   htlc,
			amount: amt,
		})
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

	// The HTLCs should all be sent to the remote.
	var msg lnwire.Message
	for i := 0; i < numHTLCs; i++ {
		select {
		case msg = <-aliceMsgs:
		case <-time.After(2 * time.Second):
			t.Fatalf("did not receive message")
		}

		addHtlc, ok := msg.(*lnwire.UpdateAddHTLC)
		if !ok {
			t.Fatalf("expected UpdateAddHTLC, got %T", msg)
		}

		_, err := bobChannel.ReceiveHTLC(addHtlc)
		if err != nil {
			t.Fatalf("bob failed receiving htlc: %v", err)
		}
	}

	select {
	case msg = <-aliceMsgs:
		t.Fatalf("unexpected message: %T", msg)
	case <-time.After(20 * time.Millisecond):
	}

	// TODO(roasbeef): increase sleep
	time.Sleep(time.Second * 1)
	commitWeight := lnwallet.CommitWeight + lnwallet.HtlcWeight*numHTLCs
	htlcFee := lnwire.NewMSatFromSatoshis(
		btcutil.Amount((int64(feePerKw) * commitWeight) / 1000),
	)
	expectedBandwidth = aliceStartingBandwidth - totalHtlcAmt - htlcFee
	expectedBandwidth += lnwire.NewMSatFromSatoshis(defaultCommitFee)
	assertLinkBandwidth(t, aliceLink, expectedBandwidth)

	// The overflow queue should be empty at this point, as the commitment
	// transaction should be full, but not yet overflown.
	if coreLink.overflowQueue.Length() != 0 {
		t.Fatalf("wrong overflow queue length: expected %v, got %v", 0,
			coreLink.overflowQueue.Length())
	}

	// At this point, the commitment transaction should now be fully
	// saturated. We'll continue adding HTLC's, and asserting that the
	// bandwidth accounting is done properly.
	const numOverFlowHTLCs = 20
	for i := 0; i < numOverFlowHTLCs; i++ {
		preImage := addLinkHTLC(htlcAmt)
		preImages = append(preImages, preImage)

		totalHtlcAmt += htlcAmt
	}

	// No messages should be sent to the remote at this point.
	select {
	case msg = <-aliceMsgs:
		t.Fatalf("unexpected message: %T", msg)
	case <-time.After(20 * time.Millisecond):
	}

	time.Sleep(time.Second * 2)
	expectedBandwidth -= (numOverFlowHTLCs * htlcAmt)
	assertLinkBandwidth(t, aliceLink, expectedBandwidth)

	// With the extra HTLC's added, the overflow queue should now be
	// populated with our 20 additional HTLC's.
	if coreLink.overflowQueue.Length() != numOverFlowHTLCs {
		t.Fatalf("wrong overflow queue length: expected %v, got %v",
			numOverFlowHTLCs,
			coreLink.overflowQueue.Length())
	}

	// We trigger a state update to lock in the HTLCs. This should
	// not change Alice's bandwidth.
	if err := updateState(batchTick, coreLink, bobChannel, true); err != nil {
		t.Fatalf("unable to update state: %v", err)
	}
	time.Sleep(time.Millisecond * 500)
	assertLinkBandwidth(t, aliceLink, expectedBandwidth)

	// At this point, we'll now settle enough HTLCs to empty the overflow
	// queue. The resulting bandwidth change should be non-existent as this
	// will simply transfer over funds to the remote party. However, the
	// size of the overflow queue should be decreasing
	for i := 0; i < numOverFlowHTLCs; i++ {
		err = bobChannel.SettleHTLC(preImages[i], uint64(i))
		if err != nil {
			t.Fatalf("unable to settle htlc: %v", err)
		}

		htlcSettle := &lnwire.UpdateFulfillHTLC{
			ID:              uint64(i),
			PaymentPreimage: preImages[i],
		}

		aliceLink.HandleChannelUpdate(htlcSettle)
		time.Sleep(time.Millisecond * 50)
	}
	time.Sleep(time.Millisecond * 500)
	assertLinkBandwidth(t, aliceLink, expectedBandwidth)

	// We trigger a state update to lock in the Settles.
	if err := updateState(batchTick, coreLink, bobChannel, false); err != nil {
		t.Fatalf("unable to update state: %v", err)
	}

	// After the state update is done, Alice should start sending
	// HTLCs from the overflow queue.
	for i := 0; i < numOverFlowHTLCs; i++ {
		var msg lnwire.Message
		select {
		case msg = <-aliceMsgs:
		case <-time.After(2 * time.Second):
			t.Fatalf("did not receive message")
		}

		addHtlc, ok := msg.(*lnwire.UpdateAddHTLC)
		if !ok {
			t.Fatalf("expected UpdateAddHTLC, got %T", msg)
		}

		_, err := bobChannel.ReceiveHTLC(addHtlc)
		if err != nil {
			t.Fatalf("bob failed receiving htlc: %v", err)
		}
	}

	select {
	case msg = <-aliceMsgs:
		t.Fatalf("unexpected message: %T", msg)
	case <-time.After(20 * time.Millisecond):
	}

	assertLinkBandwidth(t, aliceLink, expectedBandwidth)

	// Finally, at this point, the queue itself should be fully empty. As
	// enough slots have been drained from the commitment transaction to
	// allocate the queue items to.
	time.Sleep(time.Millisecond * 500)
	if coreLink.overflowQueue.Length() != 0 {
		t.Fatalf("wrong overflow queue length: expected %v, got %v", 0,
			coreLink.overflowQueue.Length())
	}
}

// TestChannelRetransmission tests the ability of the channel links to
// synchronize theirs states after abrupt disconnect.
func TestChannelRetransmission(t *testing.T) {
	t.Parallel()

	retransmissionTests := []struct {
		name     string
		messages []expectedMessage
	}{
		{
			// Tests the ability of the channel links states to be
			// synchronized after remote node haven't receive
			// revoke and ack message.
			name: "intercept last alice revoke_and_ack",
			messages: []expectedMessage{
				// First initialization of the channel.
				{"alice", "bob", &lnwire.ChannelReestablish{}, false},
				{"bob", "alice", &lnwire.ChannelReestablish{}, false},

				{"alice", "bob", &lnwire.FundingLocked{}, false},
				{"bob", "alice", &lnwire.FundingLocked{}, false},

				// Send payment from Alice to Bob and intercept
				// the last revocation message, in this case
				// Bob should not proceed the payment farther.
				{"alice", "bob", &lnwire.UpdateAddHTLC{}, false},
				{"alice", "bob", &lnwire.CommitSig{}, false},
				{"bob", "alice", &lnwire.RevokeAndAck{}, false},
				{"bob", "alice", &lnwire.CommitSig{}, false},
				{"alice", "bob", &lnwire.RevokeAndAck{}, true},

				// Reestablish messages exchange on nodes restart.
				{"alice", "bob", &lnwire.ChannelReestablish{}, false},
				{"bob", "alice", &lnwire.ChannelReestablish{}, false},

				// Alice should resend the revoke_and_ack
				// message to Bob because Bob claimed it in the
				// re-establish message.
				{"alice", "bob", &lnwire.RevokeAndAck{}, false},

				// Proceed the payment farther by sending the
				// fulfilment message and trigger the state
				// update.
				{"bob", "alice", &lnwire.UpdateFulfillHTLC{}, false},
				{"bob", "alice", &lnwire.CommitSig{}, false},
				{"alice", "bob", &lnwire.RevokeAndAck{}, false},
				{"alice", "bob", &lnwire.CommitSig{}, false},
				{"bob", "alice", &lnwire.RevokeAndAck{}, false},
			},
		},
		{
			// Tests the ability of the channel links states to be
			// synchronized after remote node haven't receive
			// revoke and ack message.
			name: "intercept bob revoke_and_ack commit_sig messages",
			messages: []expectedMessage{
				{"alice", "bob", &lnwire.ChannelReestablish{}, false},
				{"bob", "alice", &lnwire.ChannelReestablish{}, false},

				{"alice", "bob", &lnwire.FundingLocked{}, false},
				{"bob", "alice", &lnwire.FundingLocked{}, false},

				// Send payment from Alice to Bob and intercept
				// the last revocation message, in this case
				// Bob should not proceed the payment farther.
				{"alice", "bob", &lnwire.UpdateAddHTLC{}, false},
				{"alice", "bob", &lnwire.CommitSig{}, false},

				// Intercept bob commit sig and revoke and ack
				// messages.
				{"bob", "alice", &lnwire.RevokeAndAck{}, true},
				{"bob", "alice", &lnwire.CommitSig{}, true},

				// Reestablish messages exchange on nodes restart.
				{"alice", "bob", &lnwire.ChannelReestablish{}, false},
				{"bob", "alice", &lnwire.ChannelReestablish{}, false},

				// Bob should resend previously intercepted messages.
				{"bob", "alice", &lnwire.RevokeAndAck{}, false},
				{"bob", "alice", &lnwire.CommitSig{}, false},

				// Proceed the payment farther by sending the
				// fulfilment message and trigger the state
				// update.
				{"alice", "bob", &lnwire.RevokeAndAck{}, false},
				{"bob", "alice", &lnwire.UpdateFulfillHTLC{}, false},
				{"bob", "alice", &lnwire.CommitSig{}, false},
				{"alice", "bob", &lnwire.RevokeAndAck{}, false},
				{"alice", "bob", &lnwire.CommitSig{}, false},
				{"bob", "alice", &lnwire.RevokeAndAck{}, false},
			},
		},
		{
			// Tests the ability of the channel links states to be
			// synchronized after remote node haven't receive
			// update and commit sig messages.
			name: "intercept update add htlc and commit sig messages",
			messages: []expectedMessage{
				{"alice", "bob", &lnwire.ChannelReestablish{}, false},
				{"bob", "alice", &lnwire.ChannelReestablish{}, false},

				{"alice", "bob", &lnwire.FundingLocked{}, false},
				{"bob", "alice", &lnwire.FundingLocked{}, false},

				// Attempt make a payment from Alice to Bob,
				// which is intercepted, emulating the Bob
				// server abrupt stop.
				{"alice", "bob", &lnwire.UpdateAddHTLC{}, true},
				{"alice", "bob", &lnwire.CommitSig{}, true},

				// Restart of the nodes, and after that nodes
				// should exchange the reestablish messages.
				{"alice", "bob", &lnwire.ChannelReestablish{}, false},
				{"bob", "alice", &lnwire.ChannelReestablish{}, false},

				{"alice", "bob", &lnwire.FundingLocked{}, false},
				{"bob", "alice", &lnwire.FundingLocked{}, false},

				// After Bob has notified Alice that he didn't
				// receive updates Alice should re-send them.
				{"alice", "bob", &lnwire.UpdateAddHTLC{}, false},
				{"alice", "bob", &lnwire.CommitSig{}, false},

				{"bob", "alice", &lnwire.RevokeAndAck{}, false},
				{"bob", "alice", &lnwire.CommitSig{}, false},
				{"alice", "bob", &lnwire.RevokeAndAck{}, false},

				{"bob", "alice", &lnwire.UpdateFulfillHTLC{}, false},
				{"bob", "alice", &lnwire.CommitSig{}, false},
				{"alice", "bob", &lnwire.RevokeAndAck{}, false},
				{"alice", "bob", &lnwire.CommitSig{}, false},
				{"bob", "alice", &lnwire.RevokeAndAck{}, false},
			},
		},
	}
	paymentWithRestart := func(t *testing.T, messages []expectedMessage) {
		channels, cleanUp, restoreChannelsFromDb, err := createClusterChannels(
			btcutil.SatoshiPerBitcoin*5,
			btcutil.SatoshiPerBitcoin*5)
		if err != nil {
			t.Fatalf("unable to create channel: %v", err)
		}
		defer cleanUp()

		chanID := lnwire.NewChanIDFromOutPoint(channels.aliceToBob.ChannelPoint())
		serverErr := make(chan error, 4)

		aliceInterceptor := createInterceptorFunc("[alice] <-- [bob]",
			"alice", messages, chanID, false)
		bobInterceptor := createInterceptorFunc("[alice] --> [bob]",
			"bob", messages, chanID, false)

		ct := newConcurrentTester(t)

		// Add interceptor to check the order of Bob and Alice
		// messages.
		n := newThreeHopNetwork(ct,
			channels.aliceToBob, channels.bobToAlice,
			channels.bobToCarol, channels.carolToBob,
			testStartingHeight,
		)
		n.aliceServer.intersect(aliceInterceptor)
		n.bobServer.intersect(bobInterceptor)
		if err := n.start(); err != nil {
			ct.Fatalf("unable to start three hop network: %v", err)
		}
		defer n.stop()

		bobBandwidthBefore := n.firstBobChannelLink.Bandwidth()
		aliceBandwidthBefore := n.aliceChannelLink.Bandwidth()

		amount := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)
		htlcAmt, totalTimelock, hops := generateHops(amount, testStartingHeight,
			n.firstBobChannelLink)

		// Send payment which should fail because we intercept the
		// update and commit messages.
		//
		// TODO(roasbeef); increase timeout?
		receiver := n.bobServer
		rhash, err := n.makePayment(n.aliceServer, receiver,
			n.bobServer.PubKey(), hops, amount, htlcAmt,
			totalTimelock).Wait(time.Second * 5)
		if err == nil {
			ct.Fatalf("payment shouldn't haven been finished")
		}

		// Stop network cluster and create new one, with the old
		// channels states. Also do the *hack* - save the payment
		// receiver to pass it in new channel link, otherwise payment
		// will be failed because of the unknown payment hash. Hack
		// will be removed with sphinx payment.
		bobRegistry := n.bobServer.registry
		n.stop()

		channels, err = restoreChannelsFromDb()
		if err != nil {
			ct.Fatalf("unable to restore channels from database: %v", err)
		}

		n = newThreeHopNetwork(ct, channels.aliceToBob, channels.bobToAlice,
			channels.bobToCarol, channels.carolToBob, testStartingHeight)
		n.firstBobChannelLink.cfg.Registry = bobRegistry
		n.aliceServer.intersect(aliceInterceptor)
		n.bobServer.intersect(bobInterceptor)

		if err := n.start(); err != nil {
			ct.Fatalf("unable to start three hop network: %v", err)
		}
		defer n.stop()

		// Wait for reestablishment to be proceeded and invoice to be settled.
		// TODO(andrew.shvv) Will be removed if we move the notification center
		// to the channel link itself.

		var invoice channeldb.Invoice
		for i := 0; i < 20; i++ {
			select {
			case <-time.After(time.Millisecond * 200):
			case serverErr := <-serverErr:
				ct.Fatalf("server error: %v", serverErr)
			}

			// Check that alice invoice wasn't settled and
			// bandwidth of htlc links hasn't been changed.
			invoice, err = receiver.registry.LookupInvoice(rhash)
			if err != nil {
				err = errors.Errorf("unable to get invoice: %v", err)
				continue
			}
			if !invoice.Terms.Settled {
				err = errors.Errorf("alice invoice haven't been settled")
				continue
			}

			aliceExpectedBandwidth := aliceBandwidthBefore - htlcAmt
			if aliceExpectedBandwidth != n.aliceChannelLink.Bandwidth() {
				err = errors.Errorf("expected alice to have %v, instead has %v",
					aliceExpectedBandwidth, n.aliceChannelLink.Bandwidth())
				continue
			}

			bobExpectedBandwidth := bobBandwidthBefore + htlcAmt
			if bobExpectedBandwidth != n.firstBobChannelLink.Bandwidth() {
				err = errors.Errorf("expected bob to have %v, instead has %v",
					bobExpectedBandwidth, n.firstBobChannelLink.Bandwidth())
				continue
			}

			break
		}

		if err != nil {
			ct.Fatal(err)
		}
	}

	for _, test := range retransmissionTests {
		passed := t.Run(test.name, func(t *testing.T) {
			paymentWithRestart(t, test.messages)
		})

		if !passed {
			break
		}
	}

}

// TestShouldAdjustCommitFee tests the shouldAdjustCommitFee pivot function to
// ensure that ie behaves properly. We should only update the fee if it
// deviates from our current fee by more 10% or more.
func TestShouldAdjustCommitFee(t *testing.T) {
	tests := []struct {
		netFee       btcutil.Amount
		chanFee      btcutil.Amount
		shouldAdjust bool
	}{

		// The network fee is 3x lower than the current commitment
		// transaction. As a result, we should adjust our fee to match
		// it.
		{
			netFee:       100,
			chanFee:      3000,
			shouldAdjust: true,
		},

		// The network fee is lower than the current commitment fee,
		// but only slightly so, so we won't update the commitment fee.
		{
			netFee:       2999,
			chanFee:      3000,
			shouldAdjust: false,
		},

		// The network fee is lower than the commitment fee, but only
		// right before it crosses our current threshold.
		{
			netFee:       1000,
			chanFee:      1099,
			shouldAdjust: false,
		},

		// The network fee is lower than the commitment fee, and within
		// our range of adjustment, so we should adjust.
		{
			netFee:       1000,
			chanFee:      1100,
			shouldAdjust: true,
		},

		// The network fee is 2x higher than our commitment fee, so we
		// should adjust upwards.
		{
			netFee:       2000,
			chanFee:      1000,
			shouldAdjust: true,
		},

		// The network fee is higher than our commitment fee, but only
		// slightly so, so we won't update.
		{
			netFee:       1001,
			chanFee:      1000,
			shouldAdjust: false,
		},

		// The network fee is higher than our commitment fee, but
		// hasn't yet crossed our activation threshold.
		{
			netFee:       1100,
			chanFee:      1099,
			shouldAdjust: false,
		},

		// The network fee is higher than our commitment fee, and
		// within our activation threshold, so we should update our
		// fee.
		{
			netFee:       1100,
			chanFee:      1000,
			shouldAdjust: true,
		},

		// Our fees match exactly, so we shouldn't update it at all.
		{
			netFee:       1000,
			chanFee:      1000,
			shouldAdjust: false,
		},
	}

	for i, test := range tests {
		adjustedFee := shouldAdjustCommitFee(
			test.netFee, test.chanFee,
		)

		if adjustedFee && !test.shouldAdjust {
			t.Fatalf("test #%v failed: net_fee=%v, "+
				"chan_fee=%v, adjust_expect=%v, adjust_returned=%v",
				i, test.netFee, test.chanFee, test.shouldAdjust,
				adjustedFee)
		}
	}
}

// TestChannelLinkUpdateCommitFee tests that when a new block comes in, the
// channel link properly checks to see if it should update the commitment fee.
func TestChannelLinkUpdateCommitFee(t *testing.T) {
	t.Parallel()

	// First, we'll create our traditional three hop network. We'll only be
	// interacting with and asserting the state of two of the end points
	// for this test.
	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5)
	if err != nil {
		t.Fatalf("unable to create channel: %v", err)
	}
	defer cleanUp()

	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight)

	// First, we'll set up some message interceptors to ensure that the
	// proper messages are sent when updating fees.
	chanID := n.aliceChannelLink.ChanID()
	messages := []expectedMessage{
		{"alice", "bob", &lnwire.ChannelReestablish{}, false},
		{"bob", "alice", &lnwire.ChannelReestablish{}, false},

		{"alice", "bob", &lnwire.FundingLocked{}, false},
		{"bob", "alice", &lnwire.FundingLocked{}, false},

		{"alice", "bob", &lnwire.UpdateFee{}, false},

		{"alice", "bob", &lnwire.CommitSig{}, false},
		{"bob", "alice", &lnwire.RevokeAndAck{}, false},
		{"bob", "alice", &lnwire.CommitSig{}, false},
		{"alice", "bob", &lnwire.RevokeAndAck{}, false},
	}
	n.aliceServer.intersect(createInterceptorFunc("[alice] <-- [bob]",
		"alice", messages, chanID, false))
	n.bobServer.intersect(createInterceptorFunc("[alice] --> [bob]",
		"bob", messages, chanID, false))

	if err := n.start(); err != nil {
		t.Fatal(err)
	}
	defer n.stop()
	defer n.feeEstimator.Stop()

	// First, we'll start off all channels at "height" 9000 by sending a
	// new epoch to all the clients.
	select {
	case n.aliceBlockEpoch <- &chainntnfs.BlockEpoch{
		Height: 9000,
	}:
	case <-time.After(time.Second * 5):
		t.Fatalf("link didn't read block epoch")
	}
	select {
	case n.bobFirstBlockEpoch <- &chainntnfs.BlockEpoch{
		Height: 9000,
	}:
	case <-time.After(time.Second * 5):
		t.Fatalf("link didn't read block epoch")
	}

	startingFeeRate := channels.aliceToBob.CommitFeeRate()

	// Next, we'll send the first fee rate response to Alice.
	select {
	case n.feeEstimator.weightFeeIn <- startingFeeRate / 1000:
	case <-time.After(time.Second * 5):
		t.Fatalf("alice didn't query for the new " +
			"network fee")
	}

	time.Sleep(time.Millisecond * 500)

	// The fee rate on the alice <-> bob channel should still be the same
	// on both sides.
	aliceFeeRate := channels.aliceToBob.CommitFeeRate()
	bobFeeRate := channels.bobToAlice.CommitFeeRate()
	if aliceFeeRate != bobFeeRate {
		t.Fatalf("fee rates don't match: expected %v got %v",
			aliceFeeRate, bobFeeRate)
	}
	if aliceFeeRate != startingFeeRate {
		t.Fatalf("alice's fee rate shouldn't have changed: "+
			"expected %v, got %v", aliceFeeRate, startingFeeRate)
	}
	if bobFeeRate != startingFeeRate {
		t.Fatalf("bob's fee rate shouldn't have changed: "+
			"expected %v, got %v", bobFeeRate, startingFeeRate)
	}

	// Now we'll send a new block update to all end points, with a new
	// height THAT'S OVER 9000!!!
	select {
	case n.aliceBlockEpoch <- &chainntnfs.BlockEpoch{
		Height: 9001,
	}:
	case <-time.After(time.Second * 5):
		t.Fatalf("link didn't read block epoch")
	}
	select {
	case n.bobFirstBlockEpoch <- &chainntnfs.BlockEpoch{
		Height: 9001,
	}:
	case <-time.After(time.Second * 5):
		t.Fatalf("link didn't read block epoch")
	}

	// Next, we'll set up a deliver a fee rate that's triple the current
	// fee rate. This should cause the Alice (the initiator) to trigger a
	// fee update.
	newFeeRate := startingFeeRate * 3
	select {
	case n.feeEstimator.weightFeeIn <- newFeeRate:
	case <-time.After(time.Second * 5):
		t.Fatalf("alice didn't query for the new " +
			"network fee")
	}

	time.Sleep(time.Second * 2)

	// At this point, Alice should've triggered a new fee update that
	// increased the fee rate to match the new rate.
	//
	// We'll scale the new fee rate by 100 as we deal with units of fee
	// per-kw.
	expectedFeeRate := newFeeRate * 1000
	aliceFeeRate = channels.aliceToBob.CommitFeeRate()
	bobFeeRate = channels.bobToAlice.CommitFeeRate()
	if aliceFeeRate != expectedFeeRate {
		t.Fatalf("alice's fee rate didn't change: expected %v, got %v",
			expectedFeeRate, aliceFeeRate)
	}
	if bobFeeRate != expectedFeeRate {
		t.Fatalf("bob's fee rate didn't change: expected %v, got %v",
			expectedFeeRate, aliceFeeRate)
	}
	if aliceFeeRate != bobFeeRate {
		t.Fatalf("fee rates don't match: expected %v got %v",
			aliceFeeRate, bobFeeRate)
	}
}

// TestChannelLinkRejectDuplicatePayment tests that if a link receives an
// incoming HTLC for a payment we have already settled, then it rejects the
// HTLC. We do this as we want to enforce the fact that invoices are only to be
// used _once.
func TestChannelLinkRejectDuplicatePayment(t *testing.T) {
	t.Parallel()

	// First, we'll create our traditional three hop network. We'll only be
	// interacting with and asserting the state of two of the end points
	// for this test.
	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*3,
		btcutil.SatoshiPerBitcoin*5)
	if err != nil {
		t.Fatalf("unable to create channel: %v", err)
	}
	defer cleanUp()

	n := newThreeHopNetwork(t, channels.aliceToBob, channels.bobToAlice,
		channels.bobToCarol, channels.carolToBob, testStartingHeight)
	if err := n.start(); err != nil {
		t.Fatalf("unable to start three hop network: %v", err)
	}
	defer n.stop()

	amount := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)

	// We'll start off by making a payment from Alice to Carol. We'll
	// manually generate this request so we can control all the parameters.
	htlcAmt, totalTimelock, hops := generateHops(amount, testStartingHeight,
		n.firstBobChannelLink, n.carolChannelLink)
	blob, err := generateRoute(hops...)
	if err != nil {
		t.Fatal(err)
	}
	invoice, htlc, err := generatePayment(amount, htlcAmt, totalTimelock,
		blob)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.carolServer.registry.AddInvoice(*invoice); err != nil {
		t.Fatalf("unable to add invoice in carol registry: %v", err)
	}

	// With the invoice now added to Carol's registry, we'll send the
	// payment. It should succeed w/o any issues as it has been crafted
	// properly.
	_, err = n.aliceServer.htlcSwitch.SendHTLC(n.bobServer.PubKey(), htlc,
		newMockDeobfuscator())
	if err != nil {
		t.Fatalf("unable to send payment to carol: %v", err)
	}

	// Now, if we attempt to send the payment *again* it should be rejected
	// as it's a duplicate request.
	_, err = n.aliceServer.htlcSwitch.SendHTLC(n.bobServer.PubKey(), htlc,
		newMockDeobfuscator())
	if err.Error() != lnwire.CodeUnknownPaymentHash.String() {
		t.Fatal("error haven't been received")
	}
}

// TODO(roasbeef): add test for re-sending after hodl mode, to settle any lingering
