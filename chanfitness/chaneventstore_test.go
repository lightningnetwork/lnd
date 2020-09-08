package chanfitness

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
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
		pubKey = &btcec.PublicKey{
			X:     big.NewInt(0),
			Y:     big.NewInt(1),
			Curve: btcec.S256(),
		}

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

		testEventStore(t, gen, 1, map[route.Vertex]bool{
			peer1: true,
		})
	})

	t.Run("duplicate channel open events", func(t *testing.T) {
		gen := func(ctx *chanEventStoreTestCtx) {
			ctx.sendChannelOpenedUpdate(pubKey, chan1)
			ctx.sendChannelOpenedUpdate(pubKey, chan1)
			ctx.peerEvent(peer1, true)
		}

		testEventStore(t, gen, 1, map[route.Vertex]bool{
			peer1: true,
		})
	})

	t.Run("peer online before channel created", func(t *testing.T) {
		gen := func(ctx *chanEventStoreTestCtx) {
			ctx.peerEvent(peer1, true)
			ctx.sendChannelOpenedUpdate(pubKey, chan1)
		}

		testEventStore(t, gen, 1, map[route.Vertex]bool{
			peer1: true,
		})
	})

	t.Run("multiple channels for peer", func(t *testing.T) {
		gen := func(ctx *chanEventStoreTestCtx) {
			ctx.peerEvent(peer1, true)
			ctx.sendChannelOpenedUpdate(pubKey, chan1)

			ctx.peerEvent(peer1, false)
			ctx.sendChannelOpenedUpdate(pubKey, chan2)
		}

		testEventStore(t, gen, 2, map[route.Vertex]bool{
			peer1: false,
		})
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

		testEventStore(t, gen, 2, map[route.Vertex]bool{
			peer1: true,
		})
	})

}

// testEventStore creates a new test contexts, generates a set of events for it
// and tests that it has the number of channels and online state for peers that
// we expect.
func testEventStore(t *testing.T, generateEvents func(*chanEventStoreTestCtx),
	expectedChannels int, expectedPeers map[route.Vertex]bool) {

	testCtx := newChanEventStoreTestCtx(t)
	testCtx.start()

	generateEvents(testCtx)

	// Shutdown the store so that we can safely access the maps in our event
	// store.
	testCtx.stop()
	require.Len(t, testCtx.store.channels, expectedChannels)
	require.Equal(t, expectedPeers, testCtx.store.peers)
}

// TestGetLifetime tests the GetLifetime function for the cases where a channel
// is known and unknown to the store.
func TestGetLifetime(t *testing.T) {
	tests := []struct {
		name          string
		channelFound  bool
		channelPoint  wire.OutPoint
		opened        time.Time
		closed        time.Time
		expectedError error
	}{
		{
			name:          "Channel found",
			channelFound:  true,
			opened:        testNow,
			closed:        testNow.Add(time.Hour * -1),
			expectedError: nil,
		},
		{
			name:          "Channel not found",
			expectedError: ErrChannelNotFound,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			ctx := newChanEventStoreTestCtx(t)

			// Add channel to eventStore if the test indicates that
			// it should be present.
			if test.channelFound {
				ctx.store.channels[test.channelPoint] =
					&chanEventLog{
						openedAt: test.opened,
						closedAt: test.closed,
					}
			}

			ctx.start()

			open, close, err := ctx.store.GetLifespan(test.channelPoint)
			require.Equal(t, test.expectedError, err)

			require.Equal(t, test.opened, open)
			require.Equal(t, test.closed, close)

			ctx.stop()
		})
	}
}

// TestGetUptime tests the getUptime call for channels known to the event store.
// It does not test the trivial case where a channel is unknown to the store,
// because this is simply a zero return if an item is not found in a map. It
// tests the unexpected edge cases where a tracked channel does not have any
// events recorded, and when a zero time is specified for the uptime range.
func TestGetUptime(t *testing.T) {
	twoHoursAgo := testNow.Add(time.Hour * -2)
	fourHoursAgo := testNow.Add(time.Hour * -4)

	tests := []struct {
		name string

		channelPoint wire.OutPoint

		// events is the set of events we expect to find in the channel
		// store.
		events []*channelEvent

		// openedAt is the time the channel is recorded as open by the
		// store.
		openedAt time.Time

		// closedAt is the time the channel is recorded as closed by the
		// store. If the channel is still open, this value is zero.
		closedAt time.Time

		// channelFound is true if we expect to find the channel in the
		// store.
		channelFound bool

		// startTime specifies the beginning of the uptime range we want
		// to calculate.
		startTime time.Time

		// endTime specified the end of the uptime range we want to
		// calculate.
		endTime time.Time

		expectedUptime time.Duration

		expectedError error
	}{
		{
			name:          "No events",
			startTime:     twoHoursAgo,
			endTime:       testNow,
			channelFound:  true,
			expectedError: nil,
		},
		{
			name: "50% Uptime",
			events: []*channelEvent{
				{
					timestamp: fourHoursAgo,
					eventType: peerOnlineEvent,
				},
				{
					timestamp: twoHoursAgo,
					eventType: peerOfflineEvent,
				},
			},
			openedAt:       fourHoursAgo,
			expectedUptime: time.Hour * 2,
			startTime:      fourHoursAgo,
			endTime:        testNow,
			channelFound:   true,
			expectedError:  nil,
		},
		{
			name: "Zero start time",
			events: []*channelEvent{
				{
					timestamp: fourHoursAgo,
					eventType: peerOnlineEvent,
				},
			},
			openedAt:       fourHoursAgo,
			expectedUptime: time.Hour * 4,
			endTime:        testNow,
			channelFound:   true,
			expectedError:  nil,
		},
		{
			name:          "Channel not found",
			startTime:     twoHoursAgo,
			endTime:       testNow,
			channelFound:  false,
			expectedError: ErrChannelNotFound,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			ctx := newChanEventStoreTestCtx(t)

			// If we're supposed to find the channel for this test,
			// add events for it to the store.
			if test.channelFound {
				eventLog := &chanEventLog{
					events:   test.events,
					clock:    clock.NewTestClock(testNow),
					openedAt: test.openedAt,
					closedAt: test.closedAt,
				}
				ctx.store.channels[test.channelPoint] = eventLog
			}

			ctx.start()

			uptime, err := ctx.store.GetUptime(
				test.channelPoint, test.startTime, test.endTime,
			)
			require.Equal(t, test.expectedError, err)
			require.Equal(t, test.expectedUptime, uptime)

			ctx.stop()
		})
	}
}

// TestAddChannel tests that channels are added to the event store with
// appropriate timestamps. This test addresses a bug where offline channels
// did not have an opened time set, and checks that an online event is set for
// peers that are online at the time that a channel is opened.
func TestAddChannel(t *testing.T) {
	ctx := newChanEventStoreTestCtx(t)
	ctx.start()

	// Create a channel for a peer that is not online yet.
	_, _, channel1 := ctx.createChannel()

	// Get a set of values for another channel, but do not create it yet.
	//
	peer2, pubkey2, channel2 := ctx.newChannel()
	ctx.peerEvent(peer2, true)
	ctx.sendChannelOpenedUpdate(pubkey2, channel2)

	ctx.stop()

	// Assert that our peer that was offline on connection has no events
	// and our peer that was online on connection has one.
	require.Len(t, ctx.store.channels[channel1].events, 0)

	chan2Events := ctx.store.channels[channel2].events
	require.Len(t, chan2Events, 1)
	require.Equal(t, peerOnlineEvent, chan2Events[0].eventType)
}
