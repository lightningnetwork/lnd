package htlcswitch

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"reflect"

	"io"

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

	// Wait for:
	// * HTLC add request to be sent to bob.
	// * alice<->bob commitment state to be updated.
	// * settle request to be sent back from bob to alice.
	// * alice<->bob commitment state to be updated.
	// * user notification to be sent.
	var amount btcutil.Amount = btcutil.SatoshiPerBitcoin
	invoice, err := n.makePayment([]Peer{
		n.aliceServer,
		n.bobServer,
	}, amount)
	if err != nil {
		t.Fatalf("unable to make the payment: %v", err)
	}

	// Wait for Bob to receive the revocation.
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

	// Send max available payment number in both sides, thereby testing
	// the property of channel link to cope with overflowing.
	errChan := make(chan error)
	count := 2 * lnwallet.MaxHTLCNumber
	for i := 0; i < count/2; i++ {
		go func() {
			_, err := n.makePayment([]Peer{
				n.aliceServer,
				n.bobServer,
			}, 10)
			errChan <- err
		}()
	}

	for i := 0; i < count/2; i++ {
		go func() {
			_, err := n.makePayment([]Peer{
				n.bobServer,
				n.aliceServer,
			}, 10)
			errChan <- err
		}()
	}

	// Check that alice invoice was settled and bandwidth of HTLC
	// links was changed.
	for i := 0; i < count; i++ {
		select {
		case err := <-errChan:
			if err != nil {
				t.Fatalf("unable to make the payment: %v", err)
			}
		case <-time.After(4 * time.Second):
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
}

// TestChannelLinkMultiHopPayment checks the ability to send payment over two
// hopes. In this test we send the payment from Carol to Alice over Bob peer.
// (Carol -> Bob -> Alice) and checking that HTLC was settled properly and
// balances were changed in two channels.
func TestChannelLinkMultiHopPayment(t *testing.T) {
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
	var amount btcutil.Amount = btcutil.SatoshiPerBitcoin
	invoice, err := n.makePayment([]Peer{
		n.aliceServer,
		n.bobServer,
		n.carolServer,
	}, amount)
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

	if aliceBandwidthBefore-amount != n.aliceChannelLink.Bandwidth() {
		t.Fatal("the bandwidth of alice channel link which handles " +
			"alice->bob channel wasn't decreased on htlc amount")
	}

	if firstBobBandwidthBefore+amount != n.firstBobChannelLink.Bandwidth() {
		t.Fatal("the bandwidth of bob channel link which handles " +
			"alice->bob channel wasn't increased on htlc amount")
	}

	if secondBobBandwidthBefore-amount != n.secondBobChannelLink.Bandwidth() {
		t.Fatal("the bandwidth of bob channel link which handles " +
			"bob->carol channel wasn't decreased on htlc amount")
	}

	if carolBandwidthBefore+amount != n.carolChannelLink.Bandwidth() {
		t.Fatal("the bandwidth of carol channel link which handles " +
			"carol->bob channel wasn't decreased on htlc amount")
	}
}

// TestChannelLinkMultiHopInsufficientPayment checks that we receive error if
// bob<->alice channel has insufficient BTC capacity/bandwidth. In this test we
// send the payment from Carol to Alice over Bob peer. (Carol -> Bob -> Alice)
func TestChannelLinkMultiHopInsufficientPayment(t *testing.T) {
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

	// Wait for:
	// * HTLC add request to be sent to from Alice to Bob.
	// * Alice<->Bob commitment states to be updated.
	// * Bob trying to add HTLC add request in Bob<->Carol channel.
	// * Cancel HTLC request to be sent back from Bob to Alice.
	// * user notification to be sent.
	var amount btcutil.Amount = 4 * btcutil.SatoshiPerBitcoin
	invoice, err := n.makePayment([]Peer{
		n.aliceServer,
		n.bobServer,
		n.carolServer,
	}, amount)
	if err == nil {
		t.Fatal("error haven't been received")
	} else if err.Error() != errors.New(lnwire.InsufficientCapacity).Error() {
		t.Fatalf("wrong error have been received: %v", err)
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

// TestChannelLinkMultiHopUnknownPaymentHash checks that we receive remote error
// from Alice if she received not suitable payment hash for htlc.
func TestChannelLinkMultiHopUnknownPaymentHash(t *testing.T) {
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

	// Generate route convert it to blob, and return next destination for
	// htlc add request.
	peers := []Peer{
		n.bobServer,
		n.carolServer,
	}
	firstNode, blob, err := generateRoute(peers)
	if err != nil {
		t.Fatal(err)
	}

	// Generate payment: invoice and htlc.
	invoice, htlc, err := generatePayment(amount, blob)
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
	if _, err := n.aliceServer.htlcSwitch.SendHTLC(firstNode,
		htlc); err == nil {
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
// yet)
func TestChannelLinkMultiHopUnknownNextHop(t *testing.T) {
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

	dave := newMockServer(t, "save")
	invoice, err := n.makePayment([]Peer{
		n.aliceServer,
		n.bobServer,
		dave,
	}, amount)
	if err == nil {
		t.Fatal("error haven't been received")
	} else if err.Error() != errors.New(lnwire.UnknownDestination).Error() {
		t.Fatalf("wrong error have been received: %v", err)
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

// TestChannelLinkMultiHopDecodeError checks that we send HTLC cancel if
// decoding of onion blob failed.
func TestChannelLinkMultiHopDecodeError(t *testing.T) {
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
	invoice, err := n.makePayment([]Peer{
		n.aliceServer,
		n.bobServer,
		n.carolServer,
	}, amount)
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
				t.Fatal("redudant messages")
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
				t.Fatal("redudant messages")
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

	// Wait for:
	// * htlc add htlc request to be sent to alice
	// * alice<->bob commitment state to be updated
	// * settle request to be sent back from alice to bob
	// * alice<->bob commitment state to be updated
	var amount btcutil.Amount = btcutil.SatoshiPerBitcoin
	if _, err := n.makePayment([]Peer{
		n.aliceServer,
		n.bobServer,
	}, amount); err != nil {
		t.Fatalf("unable to make the payment: %v", err)
	}
}
