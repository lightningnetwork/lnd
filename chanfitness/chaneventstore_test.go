package chanfitness

import (
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/subscribe"
	"github.com/stretchr/testify/require"
)

// testNow is the current time tests will use.
var testNow = time.Unix(1592465134, 0)

// TestStartStoreError tests the starting of the store in cases where the setup
// functions fail. It does not test the mechanics of consuming events because
// these are covered in a separate set of tests.
func TestStartStoreError(t *testing.T) {
	// Ok and erroring subscribe functions are defined here to de-clutter
	// tests.
	okSubscribeFunc := func() (subscribe.Subscription, error) {
		return newMockSubscription(t), nil
	}

	errSubscribeFunc := func() (subscribe.Subscription, error) {
		return nil, errors.New("intentional test err")
	}

	tests := []struct {
		name          string
		ChannelEvents func() (subscribe.Subscription, error)
		PeerEvents    func() (subscribe.Subscription, error)
		GetChannels   func() ([]*channeldb.OpenChannel, error)
	}{
		{
			name:          "Channel events fail",
			ChannelEvents: errSubscribeFunc,
		},
		{
			name:          "Peer events fail",
			ChannelEvents: okSubscribeFunc,
			PeerEvents:    errSubscribeFunc,
		},
		{
			name:          "Get open channels fails",
			ChannelEvents: okSubscribeFunc,
			PeerEvents:    okSubscribeFunc,
			GetChannels: func() ([]*channeldb.OpenChannel, error) {
				return nil, errors.New("intentional test err")
			},
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			clock := clock.NewTestClock(testNow)

			store := NewChannelEventStore(&Config{
				SubscribeChannelEvents: test.ChannelEvents,
				SubscribePeerEvents:    test.PeerEvents,
				GetOpenChannels:        test.GetChannels,
				Clock:                  clock,
			})

			err := store.Start()
			// Check that we receive an error, because the test only
			// checks for error cases.
			if err == nil {
				t.Fatalf("Expected error on startup, got: nil")
			}
		})
	}
}

// TestMonitorChannelEvents tests the store's handling of channel and peer
// events. It tests for the unexpected cases where we receive a channel open for
// an already known channel and but does not test for closing an unknown channel
// because it would require custom logic in the test to prevent iterating
// through an eventLog which does not exist. This test does not test handling
// of uptime and lifespan requests, as they are tested in their own tests.
func TestMonitorChannelEvents(t *testing.T) {
	var (
		pubKey = btcec.NewPublicKey(
			new(btcec.FieldVal).SetInt(0),
			new(btcec.FieldVal).SetInt(1),
		)

		chan1 = wire.OutPoint{Index: 1}
		chan2 = wire.OutPoint{Index: 2}
	)

	peer1, err := route.NewVertexFromBytes(pubKey.SerializeCompressed())
	require.NoError(t, err)

	t.Run("peer comes online after channel open", func(t *testing.T) {
		gen := func(ctx *chanEventStoreTestCtx) {
			ctx.sendChannelOpenedUpdate(pubKey, chan1)
			ctx.peerEvent(peer1, true)
		}

		testEventStore(t, gen, peer1, 1)
	})

	t.Run("duplicate channel open events", func(t *testing.T) {
		gen := func(ctx *chanEventStoreTestCtx) {
			ctx.sendChannelOpenedUpdate(pubKey, chan1)
			ctx.sendChannelOpenedUpdate(pubKey, chan1)
			ctx.peerEvent(peer1, true)
		}

		testEventStore(t, gen, peer1, 1)
	})

	t.Run("peer online before channel created", func(t *testing.T) {
		gen := func(ctx *chanEventStoreTestCtx) {
			ctx.peerEvent(peer1, true)
			ctx.sendChannelOpenedUpdate(pubKey, chan1)
		}

		testEventStore(t, gen, peer1, 1)
	})

	t.Run("multiple channels for peer", func(t *testing.T) {
		gen := func(ctx *chanEventStoreTestCtx) {
			ctx.peerEvent(peer1, true)
			ctx.sendChannelOpenedUpdate(pubKey, chan1)

			ctx.peerEvent(peer1, false)
			ctx.sendChannelOpenedUpdate(pubKey, chan2)
		}

		testEventStore(t, gen, peer1, 2)
	})

	t.Run("multiple channels for peer, one closed", func(t *testing.T) {
		gen := func(ctx *chanEventStoreTestCtx) {
			ctx.peerEvent(peer1, true)
			ctx.sendChannelOpenedUpdate(pubKey, chan1)

			ctx.peerEvent(peer1, false)
			ctx.sendChannelOpenedUpdate(pubKey, chan2)

			ctx.closeChannel(chan1, pubKey)
			ctx.peerEvent(peer1, true)
		}

		testEventStore(t, gen, peer1, 1)
	})
}

// testEventStore creates a new test contexts, generates a set of events for it
// and tests that it has the number of channels we expect.
func testEventStore(t *testing.T, generateEvents func(*chanEventStoreTestCtx),
	peer route.Vertex, expectedChannels int) {

	testCtx := newChanEventStoreTestCtx(t)
	testCtx.start()

	generateEvents(testCtx)

	// Shutdown the store so that we can safely access the maps in our event
	// store.
	testCtx.stop()

	// Get our peer and check that it has the channels we expect.
	monitor, ok := testCtx.store.peers[peer]
	require.True(t, ok)

	require.Equal(t, expectedChannels, monitor.channelCount())
}

// TestStoreFlapCount tests flushing of flap counts to disk on timer ticks and
// on store shutdown.
func TestStoreFlapCount(t *testing.T) {
	testCtx := newChanEventStoreTestCtx(t)
	testCtx.start()

	pubkey, _, _ := testCtx.createChannel()
	testCtx.peerEvent(pubkey, false)

	// Now, we tick our flap count ticker. We expect our main goroutine to
	// flush our tick count to disk.
	testCtx.tickFlapCount()

	// Since we just tracked an offline event, we expect two flaps for our
	// peer because the channel opening also records an online event.
	expectedUpdate := peerFlapCountMap{
		pubkey: {
			Count:    2,
			LastFlap: testCtx.clock.Now(),
		},
	}

	testCtx.assertFlapCountUpdated()
	testCtx.assertFlapCountUpdates(expectedUpdate)

	// Create three events for out peer, online/offline/online.
	testCtx.peerEvent(pubkey, true)
	testCtx.peerEvent(pubkey, false)
	testCtx.peerEvent(pubkey, true)

	// Trigger another write.
	testCtx.tickFlapCount()

	// Since we have processed 3 more events for our peer, we update our
	// expected online map to have a flap count of 5 for this peer.
	expectedUpdate[pubkey] = &channeldb.FlapCount{
		Count:    5,
		LastFlap: testCtx.clock.Now(),
	}
	testCtx.assertFlapCountUpdated()
	testCtx.assertFlapCountUpdates(expectedUpdate)

	testCtx.stop()
}

// TestGetChanInfo tests the GetChanInfo function for the cases where a channel
// is known and unknown to the store.
func TestGetChanInfo(t *testing.T) {
	ctx := newChanEventStoreTestCtx(t)
	ctx.start()

	// Make a note of the time that our mocked clock starts on.
	now := ctx.clock.Now()

	// Create mock vars for a channel but do not add them to our store yet.
	peer, pk, channel := ctx.newChannel()

	// Send an online event for our peer, although we do not yet have an
	// open channel - this event will be ignored.
	ctx.peerEvent(peer, true)

	// Try to get info for a channel that has not been opened yet, we
	// expect to get an error.
	_, err := ctx.store.GetChanInfo(channel, peer)
	require.ErrorIs(t, err, ErrPeerNotFound)

	// Now we send our store a notification that a channel has been opened.
	ctx.sendChannelOpenedUpdate(pk, channel)

	// Wait for our channel to be recognized by our store. We need to wait
	// for the channel to be created so that we do not update our time
	// before the channel open is processed.
	require.Eventually(t, func() bool {
		_, err = ctx.store.GetChanInfo(channel, peer)
		return err == nil
	}, timeout, time.Millisecond*20)

	// Increment our test clock by an hour.
	now = now.Add(time.Hour)
	ctx.clock.SetTime(now)

	// At this stage our channel has been open and online for an hour.
	info, err := ctx.store.GetChanInfo(channel, peer)
	require.NoError(t, err)
	require.Equal(t, time.Hour, info.Lifetime)
	require.Equal(t, time.Hour, info.Uptime)

	// Now we send a peer offline event for our channel.
	ctx.peerEvent(peer, false)

	// Since we have not bumped our mocked time, our uptime calculations
	// should be the same, even though we've just processed an offline
	// event.
	info, err = ctx.store.GetChanInfo(channel, peer)
	require.NoError(t, err)
	require.Equal(t, time.Hour, info.Lifetime)
	require.Equal(t, time.Hour, info.Uptime)

	// Progress our time again. This time, our peer is currently tracked as
	// being offline, so we expect our channel info to reflect that the peer
	// has been offline for this period.
	now = now.Add(time.Hour)
	ctx.clock.SetTime(now)

	info, err = ctx.store.GetChanInfo(channel, peer)
	require.NoError(t, err)
	require.Equal(t, time.Hour*2, info.Lifetime)
	require.Equal(t, time.Hour, info.Uptime)

	ctx.stop()
}

// TestFlapCount tests querying the store for peer flap counts, covering the
// case where the peer is tracked in memory, and the case where we need to
// lookup the peer on disk.
func TestFlapCount(t *testing.T) {
	clock := clock.NewTestClock(testNow)

	var (
		peer          = route.Vertex{9, 9, 9}
		peerFlapCount = 3
		lastFlap      = clock.Now()
	)

	// Create a test context with one peer's flap count already recorded,
	// which mocks it already having its flap count stored on disk.
	ctx := newChanEventStoreTestCtx(t)
	ctx.flapUpdates[peer] = &channeldb.FlapCount{
		Count:    uint32(peerFlapCount),
		LastFlap: lastFlap,
	}

	ctx.start()

	// Create a valid public key for our peer.
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubkeyBytes := priv.PubKey().SerializeCompressed()

	var peer1 route.Vertex
	copy(peer1[:], pubkeyBytes)

	// First, query for a peer that we have no record of in memory or on
	// disk and confirm that we indicate that the peer was not found.
	_, ts, err := ctx.store.FlapCount(peer1)
	require.NoError(t, err)
	require.Nil(t, ts)

	// Send an online event for our peer - given this peer doesn't have a
	// channel with, this event will be ignored.
	ctx.peerEvent(peer1, true)

	// Assert the event is ignored.
	_, ts, err = ctx.store.FlapCount(peer1)
	require.NoError(t, err)
	require.Nil(t, ts)

	// Create a channel for the peer so that it will be tracked by the
	// store.
	pk, err := btcec.ParsePubKey(peer1[:])
	require.NoError(t, err)
	ctx.sendChannelOpenedUpdate(pk, wire.OutPoint{Index: 9})

	// Assert that we now find a record of the peer with flap count = 1,
	// because the channel open fires an online event.
	count, ts, err := ctx.store.FlapCount(peer1)
	require.NoError(t, err)
	require.Equal(t, lastFlap, *ts)
	require.Equal(t, 1, count)

	// Make a request for our peer that not tracked in memory, but does
	// have its flap count stored on disk.
	count, ts, err = ctx.store.FlapCount(peer)
	require.NoError(t, err)
	require.Equal(t, lastFlap, *ts)
	require.Equal(t, peerFlapCount, count)

	ctx.stop()
}
