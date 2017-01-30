package htlcswitch

import (
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"reflect"
	"testing"
	"time"
)

// TestSingleHopPayment in this test we checks the interaction between Alice and
// Bob within scope of one channel.
func TestSingleHopPayment(t *testing.T) {
	c := CreateCluster(t)
	if err := c.StartCluster(); err != nil {
		t.Fatal(err)
	}
	defer c.StopCluster()

	bobBandwidthBefore := c.firstBobHtlcManager.Bandwidth()
	aliceBandwidthBefore := c.aliceHtlcManager.Bandwidth()

	var amount btcutil.Amount = btcutil.SatoshiPerBitcoin
	paymentErrorChan, invoice, err := c.MakeBobToAlicePayment(amount)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for:
	// * HTLC add request to be sent to alice.
	// * alice<->bob commitment state to be updated.
	// * settle request to be sent back from alice to bob.
	// * alice<->bob commitment state to be updated.
	// * user notification to be sent.
	select {
	case err := <-paymentErrorChan:
		if err != nil {
			t.Fatalf("something wrong went when sending request: "+
				"%v", err)
		}
		break
	case <-time.After(time.Second):
		t.Fatal("htlc was no settled in time")
	}

	// Check that alice invoice was settled and bandwidth of HTLC
	// managers was changed.
	if !invoice.Terms.Settled {
		t.Fatal("invoice wasn't settled")
	}

	if aliceBandwidthBefore != c.aliceHtlcManager.Bandwidth()-amount {
		t.Fatal("alice bandwidth isn't match")
	}

	if bobBandwidthBefore != c.firstBobHtlcManager.Bandwidth()+amount {
		t.Fatal("bob bandwidth isn't match")
	}
}

// TestMultiHopPayment checks the ability to send payment over two hopes. In
// this test we send the payment from Carol to Alice over Bob peer. (Carol -> Bob -> Alice)
// and checking that HTLC was settled properly and balances were changed in
// two channels.
func TestMultiHopPayment(t *testing.T) {
	c := CreateCluster(t)
	if err := c.StartCluster(); err != nil {
		t.Fatal(err)
	}
	defer c.StopCluster()

	carolBandwidthBefore := c.carolHtlcManager.Bandwidth()
	firstBobBandwidthBefore := c.firstBobHtlcManager.Bandwidth()
	secondBobBandwidthBefore := c.secondBobHtlcManager.Bandwidth()
	aliceBandwidthBefore := c.aliceHtlcManager.Bandwidth()

	var amount btcutil.Amount = btcutil.SatoshiPerBitcoin
	paymentErrorChan, invoice, err := c.MakeCarolToAlicePayment(amount,
		false, false)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for:
	// * HTLC add request to be sent from Carol to Bob.
	// * Carol<->Bob commitment states to be updated.
	// * HTLC add request to be propagated to Alice.
	// * Alice<->Bob commitment state to be updated.
	// * settle request to be sent back from Alice to Bob.
	// * Alice<->Bob commitment state to be updated.
	// * settle request to be sent back from Bob to Carol.
	// * Carol<->Bob commitment states to be updated.
	// * user notification to be sent.
	select {
	case err := <-paymentErrorChan:
		if err != nil {
			t.Fatal(err)
		}
		break
	case <-time.After(time.Second):
		t.Fatal("htlc was no settled in time")
	}

	// Check that Alice invoice was settled and bandwidth of HTLC
	// managers were changed.
	if !invoice.Terms.Settled {
		t.Fatal("alice invoice wasn't settled")
	}

	if c.carolHtlcManager.Bandwidth() != carolBandwidthBefore-amount {
		t.Fatal("the bandwidth of carol htlc manager which handles " +
			"carol->bob channel wasn't decreased on htlc amount")
	}

	if c.secondBobHtlcManager.Bandwidth() != secondBobBandwidthBefore+amount {
		t.Fatal("the bandwidth of bob htlc manager which handles " +
			"carol->bob channel wasn't increased on htlc amount")
	}

	if c.firstBobHtlcManager.Bandwidth() != firstBobBandwidthBefore-amount {
		t.Fatal("the bandwidth of bob htlc manager which handles bob->alice channel " +
			"wasn't decreased on htlc amount")
	}

	if c.aliceHtlcManager.Bandwidth() != aliceBandwidthBefore+amount {
		t.Fatal("the bandwidth of alice htlc manager which handles " +
			"bob->alice channel wasn't incresed on htlc amount")
	}
}

// TestMultiHopInsufficientPayment checks that we receive error if bob<->alice
// channel has insufficient BTC capacity/bandwidth. In this test we send the
// payment from Carol to Alice over Bob peer. (Carol -> Bob -> Alice)
func TestMultiHopInsufficientPayment(t *testing.T) {
	c := CreateCluster(t)
	if err := c.StartCluster(); err != nil {
		t.Fatal(err)
	}
	defer c.StopCluster()

	carolBandwidthBefore := c.carolHtlcManager.Bandwidth()
	firstBobBandwidthBefore := c.firstBobHtlcManager.Bandwidth()
	secondBobBandwidthBefore := c.secondBobHtlcManager.Bandwidth()
	aliceBandwidthBefore := c.aliceHtlcManager.Bandwidth()

	var amount btcutil.Amount = 4 * btcutil.SatoshiPerBitcoin
	paymentErrorChan, invoice, err := c.MakeCarolToAlicePayment(amount,
		false, false)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for:
	// * HTLC add request to be sent to from Carol to Bob.
	// * Carol<->Bob commitment states to be updated.
	// * Bob trying to add HTLC add request in Bob<->Alice channel.
	// * Cancel HTLC request to be sent back from Bob to Carol.
	// * user notification to be sent.
	select {
	case err := <-paymentErrorChan:
		if err == nil {
			t.Fatal("error wasn't received")
		}
		break
	case <-time.After(time.Second):
		t.Fatal("error was no settled in time")
	}

	// Check that alice invoice wasn't settled and bandwidth of htlc
	// managers hasn't been changed.
	if invoice.Terms.Settled {
		t.Fatal("alice invoice was settled")
	}

	if c.carolHtlcManager.Bandwidth() != carolBandwidthBefore {
		t.Fatal("the bandwidth of carol htlc manager which handles " +
			"carol->bob channel wasn't decreased on htlc amount")
	}

	if c.secondBobHtlcManager.Bandwidth() != secondBobBandwidthBefore {
		t.Fatal("the bandwidth of bob htlc manager which handles " +
			"carol->bob channel was increased on htlc amount")
	}

	if c.firstBobHtlcManager.Bandwidth() != firstBobBandwidthBefore {
		t.Fatal("the bandwidth of bob htlc manager which handles bob->alice channel " +
			"was decreased on htlc amount")
	}

	if c.aliceHtlcManager.Bandwidth() != aliceBandwidthBefore {
		t.Fatal("the bandwidth of alice htlc manager which handles " +
			"bob->alice channel was increased on htlc amount")
	}
}

// TestMultiHopUnknownPaymentHash checks that we receive remote error from Alice if she
// received not suitable payment hash for htlc.
func TestMultiHopUnknownPaymentHash(t *testing.T) {
	c := CreateCluster(t)
	if err := c.StartCluster(); err != nil {
		t.Fatal(err)
	}
	defer c.StopCluster()

	carolBandwidthBefore := c.carolHtlcManager.Bandwidth()
	firstBobBandwidthBefore := c.firstBobHtlcManager.Bandwidth()
	secondBobBandwidthBefore := c.secondBobHtlcManager.Bandwidth()
	aliceBandwidthBefore := c.aliceHtlcManager.Bandwidth()

	var amount btcutil.Amount = btcutil.SatoshiPerBitcoin
	paymentErrorChan, invoice, err := c.MakeCarolToAlicePayment(amount,
		true, false)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-paymentErrorChan:
		if err == nil {
			t.Fatal("error wasn't received")
		}
		break
	case <-time.After(time.Second):
		t.Fatal("error was no settled in time")
	}

	// Check that alice invoice wasn't settled and bandwidth of htlc
	// managers hasn't been changed.
	if invoice.Terms.Settled {
		t.Fatal("alice invoice was settled")
	}

	if c.carolHtlcManager.Bandwidth() != carolBandwidthBefore {
		t.Fatal("the bandwidth of carol htlc manager which handles " +
			"carol->bob channel wasn't decreased on htlc amount")
	}

	if c.secondBobHtlcManager.Bandwidth() != secondBobBandwidthBefore {
		t.Fatal("the bandwidth of bob htlc manager which handles " +
			"carol->bob channel was increased on htlc amount")
	}

	if c.firstBobHtlcManager.Bandwidth() != firstBobBandwidthBefore {
		t.Fatal("the bandwidth of bob htlc manager which handles bob->alice channel " +
			"was decreased on htlc amount")
	}

	if c.aliceHtlcManager.Bandwidth() != aliceBandwidthBefore {
		t.Fatal("the bandwidth of alice htlc manager which handles " +
			"bob->alice channel was incresed on htlc amount")
	}
}

// TestMultiHopUnknownNextHop construct the chain of hops Carol<->Bob<->Alice
// and checks that we receive remote error from Bob if he has no idea about next
// hop (hop might goes down and routing info not updated yet)
func TestMultiHopUnknownNextHop(t *testing.T) {
	c := CreateCluster(t)
	if err := c.StartCluster(); err != nil {
		t.Fatal(err)
	}
	defer c.StopCluster()

	carolBandwidthBefore := c.carolHtlcManager.Bandwidth()
	firstBobBandwidthBefore := c.firstBobHtlcManager.Bandwidth()
	secondBobBandwidthBefore := c.secondBobHtlcManager.Bandwidth()
	aliceBandwidthBefore := c.aliceHtlcManager.Bandwidth()

	var amount btcutil.Amount = btcutil.SatoshiPerBitcoin
	paymentRemoteErrorChan, invoice, err := c.MakeCarolToAlicePayment(amount,
		false, true)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-paymentRemoteErrorChan:
		if err == nil {
			t.Fatal("error wasn't received")
		}
		break
	case <-time.After(time.Second):
		t.Fatal("error was no settled in time")
	}

	// Check that alice invoice wasn't settled and bandwidth of htlc
	// managers hasn't been changed.
	if invoice.Terms.Settled {
		t.Fatal("alice invoice was settled")
	}

	if c.carolHtlcManager.Bandwidth() != carolBandwidthBefore {
		t.Fatal("the bandwidth of carol htlc manager which handles " +
			"carol->bob channel wasn't decreased on htlc amount")
	}

	if c.secondBobHtlcManager.Bandwidth() != secondBobBandwidthBefore {
		t.Fatal("the bandwidth of bob htlc manager which handles " +
			"carol->bob channel was increased on htlc amount")
	}

	if c.firstBobHtlcManager.Bandwidth() != firstBobBandwidthBefore {
		t.Fatal("the bandwidth of bob htlc manager which handles bob->alice channel " +
			"was decreased on htlc amount")
	}

	if c.aliceHtlcManager.Bandwidth() != aliceBandwidthBefore {
		t.Fatal("the bandwidth of alice htlc manager which handles " +
			"bob->alice channel was incresed on htlc amount")
	}
}

// TestMultiHopDecodeError checks that we send HTLC cancel if decoding of
// onion blob failed/
func TestMultiHopDecodeError(t *testing.T) {
	c := CreateCluster(t)
	if err := c.StartCluster(); err != nil {
		t.Fatal(err)
	}
	defer c.StopCluster()

	// Replace decode function with another which throws an error.
	c.aliceHtlcManager.cfg.DecodeOnion = func(data []byte, meta []byte) (
		routing.HopIterator, error) {
		return nil, errors.New("some sphinx decode error!")
	}

	carolBandwidthBefore := c.carolHtlcManager.Bandwidth()
	firstBobBandwidthBefore := c.firstBobHtlcManager.Bandwidth()
	secondBobBandwidthBefore := c.secondBobHtlcManager.Bandwidth()
	aliceBandwidthBefore := c.aliceHtlcManager.Bandwidth()

	var amount btcutil.Amount = btcutil.SatoshiPerBitcoin
	paymentRemoteErrorChan, invoice, err := c.MakeCarolToAlicePayment(amount,
		false, false)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-paymentRemoteErrorChan:
		if err == nil {
			t.Fatal("error wasn't received")
		}
		break
	case <-time.After(time.Second):
		t.Fatal("error was no settled in time")
	}

	// Check that alice invoice wasn't settled and bandwidth of htlc
	// managers hasn't been changed.
	if invoice.Terms.Settled {
		t.Fatal("alice invoice was settled")
	}

	if c.carolHtlcManager.Bandwidth() != carolBandwidthBefore {
		t.Fatal("the bandwidth of carol htlc manager which handles " +
			"carol->bob channel wasn't decreased on htlc amount")
	}

	if c.secondBobHtlcManager.Bandwidth() != secondBobBandwidthBefore {
		t.Fatal("the bandwidth of bob htlc manager which handles " +
			"carol->bob channel was increased on htlc amount")
	}

	if c.firstBobHtlcManager.Bandwidth() != firstBobBandwidthBefore {
		t.Fatal("the bandwidth of bob htlc manager which handles bob->alice channel " +
			"was decreased on htlc amount")
	}

	if c.aliceHtlcManager.Bandwidth() != aliceBandwidthBefore {
		t.Fatal("the bandwidth of alice htlc manager which handles " +
			"bob->alice channel was incresed on htlc amount")
	}
}

func getChannelPoint(msg lnwire.Message) *wire.OutPoint {
	switch msg := msg.(type) {
	case *lnwire.HTLCAddRequest:
		return msg.ChannelPoint
	case *lnwire.HTLCSettleRequest:
		return msg.ChannelPoint
	case *lnwire.CancelHTLC:
		return msg.ChannelPoint
	case *lnwire.CommitRevocation:
		return msg.ChannelPoint
	case *lnwire.CommitSignature:
		return msg.ChannelPoint
	default:
		return nil
	}
}

// TestSingleHopMessageOrdering test checks ordering of message which flying
// around between Alice and Bob are correct when Bob sends payments to Alice.
func TestSingleHopMessageOrdering(t *testing.T) {
	c := CreateCluster(t)

	chanPoint := c.aliceHtlcManager.ID()

	// Append initial channel window revocation messages which occurs after
	// channel opening.
	var aliceOrder []lnwire.Message
	for i := 0; i < lnwallet.InitialRevocationWindow; i++ {
		aliceOrder = append(aliceOrder, &lnwire.CommitRevocation{})
	}

	aliceOrder = append(aliceOrder, []lnwire.Message{
		&lnwire.HTLCAddRequest{},
		&lnwire.CommitSignature{},
		&lnwire.CommitRevocation{},
		&lnwire.CommitSignature{},
		&lnwire.CommitRevocation{},
	}...)

	// Append initial channel window revocation messages which occurs after
	// channel channel opening.
	var bobOrder []lnwire.Message
	for i := 0; i < lnwallet.InitialRevocationWindow; i++ {
		bobOrder = append(bobOrder, &lnwire.CommitRevocation{})
	}

	bobOrder = append(bobOrder, []lnwire.Message{
		&lnwire.CommitSignature{},
		&lnwire.CommitRevocation{},
		&lnwire.HTLCSettleRequest{},
		&lnwire.CommitSignature{},
		&lnwire.CommitRevocation{},
	}...)

	// Check that alice receives messages in right order.
	c.aliceServer.Record(func(m lnwire.Message) {
		if getChannelPoint(m) == chanPoint {
			if reflect.TypeOf(aliceOrder[0]) != reflect.TypeOf(m) {
				t.Fatalf("wrong message: %v", lnwire.MessageToStringClosure(m))
			}
			aliceOrder = aliceOrder[1:]
		}

	})

	// Check that bob receives messages in right order.
	c.bobServer.Record(func(m lnwire.Message) {
		if getChannelPoint(m) == chanPoint {
			if reflect.TypeOf(bobOrder[0]) != reflect.TypeOf(m) {
				t.Fatalf("wrong message: %v", lnwire.MessageToStringClosure(m))
			}
			bobOrder = bobOrder[1:]
		}
	})

	if err := c.StartCluster(); err != nil {
		t.Fatal(err)
	}
	defer c.StopCluster()

	var amount btcutil.Amount = btcutil.SatoshiPerBitcoin
	paymentErrorChan, _, err := c.MakeBobToAlicePayment(amount)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for:
	// * htlc add htlc request to be sent to alice
	// * alice<->bob commitment state to be updated
	// * settle request to be sent back from alice to bob
	// * alice<->bob commitment state to be updated
	select {
	case err := <-paymentErrorChan:
		if err != nil {
			t.Fatalf("something wrong went when sending request: "+
				"%v", err)
		}
		break
	case <-time.After(time.Second):
		t.Fatal("htlc was no settled in time")
	}

}
