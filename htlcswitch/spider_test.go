package htlcswitch

import (
		"testing"
		"time"
		"github.com/btcsuite/btcutil"
		"github.com/lightningnetwork/lnd/lnwire"
		"fmt"
)

// bob<->alice channel has insufficient BTC capacity/bandwidth. In this test we
// sduration the payment from Carol to Alice over Bob peer. (Carol -> Bob -> Alice)
// Right now this payment returns an error immediately. Instead, we want it to
// be queued on Bob -> Alice channel, until Alice sdurations a payment upstream to
// Bob, and then this payment should be processed.
func TestSpiderInsufficentFunds (t *testing.T) {
	t.Parallel()

	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*5,
		btcutil.SatoshiPerBitcoin*3)
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

		go func() {
			fmt.Println("in the carol->bob payment routine. Will sleep first")
			time.Sleep(1 * time.Second)
			fmt.Println("woke up in the carol->bob payment routine");
			amount := lnwire.NewMSatFromSatoshis(2 * btcutil.SatoshiPerBitcoin)
			htlcAmt, totalTimelock, hops := generateHops(amount, testStartingHeight,
			n.secondBobChannelLink)

			firstHop := n.secondBobChannelLink.ShortChanID()
			_, err := n.makePayment(
				n.carolServer, n.bobServer, firstHop, hops, amount, htlcAmt,
				totalTimelock,
			).Wait(30 * time.Second)

			if err != nil {
				t.Fatal("carol->bob failed")
			}
			fmt.Println("carol->bob successful")
		}()

		amount := lnwire.NewMSatFromSatoshis(4 * btcutil.SatoshiPerBitcoin)
		htlcAmt, totalTimelock, hops := generateHops(amount, testStartingHeight,
					n.firstBobChannelLink, n.carolChannelLink)

		// What we expect to happen:
		// * HTLC add request to be sent to from Alice to Bob.
		// * Alice<->Bob commitment states to be updated.
		// * Bob trying to add HTLC add request in Bob<->Carol channel.
		// * Not enough funds for that, so it gets added to overFlowQueue in
		// Bob->Carol channel.
		// * Carol sends Bob money.
		// * Bob -> Carol channel now has enough money, and the Bob -> Carol
		// payment should succeed, thereby letting the Alice -> Carol payment to
		// succeed as well.

		firstHop := n.firstBobChannelLink.ShortChanID()
		// launch second payment here, which sleeps for a bit, and then pays from
		// carol to bob.
		_, err = n.makePayment(
			n.aliceServer, n.carolServer, firstHop, hops, amount, htlcAmt,
			totalTimelock,
		).Wait(80 * time.Second)
		// modifications on existing test. We do not want it to fail here.
		if err != nil {
			fmt.Println(err)
			t.Fatal("error has been received in first payment, alice->bob")
		}
		// if we reach this point, then all the payments have succeeded.
}


func NotTestSpiderThroughput (t *testing.T) {
	t.Parallel()
	var NUM_PAYMENTS = int(10000)
  // FIXME: do we even care about the second channel?
	channels, cleanUp, _, err := createClusterChannels(
		btcutil.SatoshiPerBitcoin*5000000,
		btcutil.SatoshiPerBitcoin*5000000)
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
		var startTime = time.Now()
		for i := 0; i < NUM_PAYMENTS; i++ {
			go func() {
				amount := lnwire.NewMSatFromSatoshis(0.01*btcutil.SatoshiPerBitcoin)
				htlcAmt, totalTimelock, hops := generateHops(amount, testStartingHeight,
									n.firstBobChannelLink)
				firstHop := n.firstBobChannelLink.ShortChanID()
				//totalTimelock = 108
				_, err := n.makePayment(
					n.aliceServer, n.bobServer, firstHop, hops, amount, htlcAmt,
					totalTimelock,
				).Wait(200 * time.Second)

				if err != nil {
					t.Fatal("alice->bob FAILED")
				}
			}()
		}
		duration :=  time.Since(startTime)
		fmt.Printf("generating all the payments took: %s\n", duration)
		ms := float64(duration / time.Millisecond)
		//fmt.Println(seconds)
		//fmt.Println(float64(seconds))
		fmt.Printf("throughput: %f/s\n", (float64(NUM_PAYMENTS) / (float64(ms)/1000.00)))
		time.Sleep(250 * time.Second)
}
