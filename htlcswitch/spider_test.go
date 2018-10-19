package htlcswitch

import (
		//"strings"
		"testing"
		"time"
		"github.com/btcsuite/btcutil"
		"github.com/lightningnetwork/lnd/lnwire"
)

// bob<->alice channel has insufficient BTC capacity/bandwidth. In this test we
// send the payment from Carol to Alice over Bob peer. (Carol -> Bob -> Alice)
// Right now this payment returns an error immediately. Instead, we want it to
// be queued on Bob -> Alice channel, until Alice sends a payment upstream to
// Bob, and then this payment should be processed.
func TestSpiderInsufficentFunds (t *testing.T) {
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
		debug := false
		if debug {
			// Log message that alice receives.
			n.aliceServer.intersect(createLogFunc("alice",
			n.aliceChannelLink.ChanID()))

			// Log message that bob receives.
			n.bobServer.intersect(createLogFunc("bob",
			n.firstBobChannelLink.ChanID()))
		}

		// TODO: uncomment when I add tests for these.
		//carolBandwidthBefore := n.carolChannelLink.Bandwidth()
		//firstBobBandwidthBefore := n.firstBobChannelLink.Bandwidth()
		//secondBobBandwidthBefore := n.secondBobChannelLink.Bandwidth()
		//aliceBandwidthBefore := n.aliceChannelLink.Bandwidth()

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
		// * TODO: what should happen next?

		firstHop := n.firstBobChannelLink.ShortChanID()
		_, err = n.makePayment(
			n.aliceServer, n.carolServer, firstHop, hops, amount, htlcAmt,
			totalTimelock,
		).Wait(30 * time.Second)

		// modifications on existing test. We do not want it to fail here.
		if err != nil {
			t.Fatal("error has been received in first payment, alice->bob")
		}
		// TODO: maybe should add more sleep time here?
		time.Sleep(100 * time.Millisecond)
		// Send money from carol -> bob so this stops failing.
		amount = lnwire.NewMSatFromSatoshis(2 * btcutil.SatoshiPerBitcoin)
		// FIXME: check the last arg, maybe should be carolChannelLink?
		htlcAmt, totalTimelock, hops = generateHops(amount, testStartingHeight,
		n.secondBobChannelLink)

		firstHop = n.secondBobChannelLink.ShortChanID()
		_, err = n.makePayment(
			n.carolServer, n.bobServer, firstHop, hops, amount, htlcAmt,
			totalTimelock,
		).Wait(30 * time.Second)

		if err != nil {
			t.Fatal("error has been received in second payment carol->bob")
		}

		// sleep some time again
		time.Sleep(100 * time.Millisecond)

		// by now the queries should be updated.
		// TODO: test all updates.
}




