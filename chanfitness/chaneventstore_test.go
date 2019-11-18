package chanfitness

import (
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/peernotifier"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/subscribe"
)

// TestStartStoreError tests the starting of the store in cases where the setup
// functions fail. It does not test the mechanics of consuming events because
// these are covered in a separate set of tests.
func TestStartStoreError(t *testing.T) {
	// Ok and erroring subscribe functions are defined here to de-clutter tests.
	okSubscribeFunc := func() (*subscribe.Client, error) {
		return &subscribe.Client{
			Cancel: func() {},
		}, nil
	}

	errSubscribeFunc := func() (client *subscribe.Client, e error) {
		return nil, errors.New("intentional test err")
	}

	tests := []struct {
		name          string
		ChannelEvents func() (*subscribe.Client, error)
		PeerEvents    func() (*subscribe.Client, error)
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
			GetChannels: func() (channels []*channeldb.OpenChannel, e error) {
				return nil, errors.New("intentional test err")
			},
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			store := NewChannelEventStore(&Config{
				SubscribeChannelEvents: test.ChannelEvents,
				SubscribePeerEvents:    test.PeerEvents,
				GetOpenChannels:        test.GetChannels,
				GetOpenTime: func(blockHeight uint32) (i time.Time, e error) {
					return time.Now(), nil
				},
			})

			err := store.Start()
			// Check that we receive an error, because the test only checks for
			// error cases.
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
	privKey, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("Error getting pubkey: %v", err)
	}

	pubKey, err := route.NewVertexFromBytes(
		privKey.PubKey().SerializeCompressed(),
	)
	if err != nil {
		t.Fatalf("Could not create vertex: %v", err)
	}

	shortID := lnwire.ShortChannelID{
		BlockHeight: 1234,
		TxIndex:     2,
		TxPosition:  2,
	}

	tests := []struct {
		name string

		// generateEvents takes channels which represent the updates channels
		// for subscription clients and passes events in the desired order.
		// This function is intended to be blocking so that the test does not
		// have a data race with event consumption, so the channels should not
		// be buffered.
		generateEvents func(channelEvents, peerEvents chan<- interface{})

		// expectedEvents is the expected set of event types in the store.
		expectedEvents []eventType
	}{
		{
			name: "Channel opened, peer comes online",
			generateEvents: func(channelEvents, peerEvents chan<- interface{}) {
				// Add an open channel event
				channelEvents <- channelnotifier.OpenChannelEvent{
					Channel: &channeldb.OpenChannel{
						ShortChannelID: shortID,
						IdentityPub:    privKey.PubKey(),
					},
				}

				// Add a peer online event.
				peerEvents <- peernotifier.PeerOnlineEvent{PubKey: pubKey}
			},
			expectedEvents: []eventType{peerOnlineEvent},
		},
		{
			name: "Duplicate channel open events",
			generateEvents: func(channelEvents, peerEvents chan<- interface{}) {
				// Add an open channel event
				channelEvents <- channelnotifier.OpenChannelEvent{
					Channel: &channeldb.OpenChannel{
						ShortChannelID: shortID,
						IdentityPub:    privKey.PubKey(),
					},
				}

				// Add a peer online event.
				peerEvents <- peernotifier.PeerOnlineEvent{PubKey: pubKey}

				// Add a duplicate channel open event.
				channelEvents <- channelnotifier.OpenChannelEvent{
					Channel: &channeldb.OpenChannel{
						ShortChannelID: shortID,
						IdentityPub:    privKey.PubKey(),
					},
				}
			},
			expectedEvents: []eventType{peerOnlineEvent},
		},
		{
			name: "Channel opened, peer already online",
			generateEvents: func(channelEvents, peerEvents chan<- interface{}) {
				// Add a peer online event.
				peerEvents <- peernotifier.PeerOnlineEvent{PubKey: pubKey}

				// Add an open channel event
				channelEvents <- channelnotifier.OpenChannelEvent{
					Channel: &channeldb.OpenChannel{
						ShortChannelID: shortID,
						IdentityPub:    privKey.PubKey(),
					},
				}
			},
			expectedEvents: []eventType{peerOnlineEvent},
		},

		{
			name: "Channel opened, peer offline, closed",
			generateEvents: func(channelEvents, peerEvents chan<- interface{}) {
				// Add an open channel event
				channelEvents <- channelnotifier.OpenChannelEvent{
					Channel: &channeldb.OpenChannel{
						ShortChannelID: shortID,
						IdentityPub:    privKey.PubKey(),
					},
				}

				// Add a peer online event.
				peerEvents <- peernotifier.PeerOfflineEvent{PubKey: pubKey}

				// Add a close channel event.
				channelEvents <- channelnotifier.ClosedChannelEvent{
					CloseSummary: &channeldb.ChannelCloseSummary{
						ShortChanID: shortID,
					},
				}
			},
			expectedEvents: []eventType{peerOfflineEvent},
		},
		{
			name: "Event after channel close not recorded",
			generateEvents: func(channelEvents, peerEvents chan<- interface{}) {
				// Add an open channel event
				channelEvents <- channelnotifier.OpenChannelEvent{
					Channel: &channeldb.OpenChannel{
						ShortChannelID: shortID,
						IdentityPub:    privKey.PubKey(),
					},
				}

				// Add a close channel event.
				channelEvents <- channelnotifier.ClosedChannelEvent{
					CloseSummary: &channeldb.ChannelCloseSummary{
						ShortChanID: shortID,
					},
				}

				// Add a peer online event.
				peerEvents <- peernotifier.PeerOfflineEvent{PubKey: pubKey}
			},
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			// Create a store with the channels and online peers specified
			// by the test.
			store := NewChannelEventStore(&Config{})

			// Create channels which represent the subscriptions we have to peer
			// and client events.
			channelEvents := make(chan interface{})
			peerEvents := make(chan interface{})

			store.wg.Add(1)
			go store.consume(&subscriptions{
				channelUpdates: channelEvents,
				peerUpdates:    peerEvents,
				cancel:         func() {},
			})

			// Add events to the store then kill the goroutine using store.Stop.
			test.generateEvents(channelEvents, peerEvents)
			store.Stop()

			// Retrieve the eventLog for the channel and check that its
			// contents are as expected.
			eventLog, ok := store.channels[shortID.ToUint64()]
			if !ok {
				t.Fatalf("Expected to find event store")
			}

			for i, e := range eventLog.events {
				if test.expectedEvents[i] != e.eventType {
					t.Fatalf("Expected type: %v, got: %v",
						test.expectedEvents[i], e.eventType)
				}
			}
		})
	}
}

// TestGetLifetime tests the GetLifetime function for the cases where a channel
// is known and unknown to the store.
func TestGetLifetime(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name         string
		channelFound bool
		chanID       uint64
		opened       time.Time
		closed       time.Time
		expectErr    bool
	}{
		{
			name:         "Channel found",
			channelFound: true,
			opened:       now,
			closed:       now.Add(time.Hour * -1),
			expectErr:    false,
		},
		{
			name:      "Channel not found",
			expectErr: true,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			// Create and  empty events store for testing.
			store := NewChannelEventStore(&Config{})

			// Start goroutine which consumes GetTimestamps requests.
			store.wg.Add(1)
			go store.consume(&subscriptions{
				channelUpdates: make(chan interface{}),
				peerUpdates:    make(chan interface{}),
				cancel:         func() {},
			})

			// Stop the store's go routine.
			defer store.Stop()

			// Add channel to eventStore if the test indicates that it should
			// be present.
			if test.channelFound {
				store.channels[test.chanID] = &chanEventLog{
					ChannelTimestamps: ChannelTimestamps{
						OpenedAt: test.opened,
						ClosedAt: test.closed,
					},
				}
			}

			timestamps, err := store.GetTimestamps(test.chanID)
			gotError := err != nil
			if test.expectErr != gotError {
				t.Fatalf("Expected an error: %v, got an error: %v",
					test.expectErr, gotError)
			}

			// If we received an error, do not perform further validation.
			if err != nil {
				return
			}

			if timestamps.OpenedAt != test.opened {
				t.Errorf("Expected: %v, got %v", test.opened,
					timestamps.OpenedAt)
			}

			if timestamps.ClosedAt != test.closed {
				t.Errorf("Expected: %v, got %v", test.closed,
					timestamps.ClosedAt)
			}
		})
	}
}

// TestGetUptime tests the getUptime call for channels known to the event store.
// It does not test the trivial case where a channel is unknown to the store,
// because this is simply a zero return if an item is not found in a map. It
// tests the unexpected edge cases where a tracked channel does not have any
// events recorded, and when a zero time is specified for the uptime range.
func TestGetUptime(t *testing.T) {
	// Set time for deterministic unit tests.
	now := time.Now()

	twoHoursAgo := now.Add(time.Hour * -2)
	fourHoursAgo := now.Add(time.Hour * -4)

	tests := []struct {
		name string

		chanID uint64

		// events is the set of events we expect to find in the channel store.
		events []*channelEvent

		// monitoredSince is the time the channel is recorded as open by the store.
		monitoredSince time.Time

		// closedAt is the time the channel is recorded as closed by the store.
		// If the channel is still open, this value is zero.
		closedAt time.Time

		// channelFound is true if we expect to find the channel in the store.
		channelFound bool

		// startTime specifies the beginning of the uptime range we want to
		// calculate.
		startTime time.Time

		// endTime specified the end of the uptime range we want to calculate.
		endTime time.Time

		expectedUptime time.Duration
		expectErr      bool
	}{
		{
			name:         "No events",
			startTime:    twoHoursAgo,
			endTime:      now,
			channelFound: true,
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
			monitoredSince: fourHoursAgo,
			expectedUptime: time.Hour * 2,
			startTime:      fourHoursAgo,
			endTime:        now,
			channelFound:   true,
		},
		{
			name: "Zero start time",
			events: []*channelEvent{
				{
					timestamp: fourHoursAgo,
					eventType: peerOnlineEvent,
				},
			},
			monitoredSince: fourHoursAgo,
			expectedUptime: time.Hour * 4,
			endTime:        now,
			channelFound:   true,
		},
		{
			name:         "Channel not found",
			startTime:    twoHoursAgo,
			endTime:      now,
			channelFound: false,
			expectErr:    true,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			// Set up event store with the events specified for the test and
			// mocked time.
			store := NewChannelEventStore(&Config{})

			// Start goroutine which consumes GetUptime requests.
			store.wg.Add(1)
			go store.consume(&subscriptions{
				channelUpdates: make(chan interface{}),
				peerUpdates:    make(chan interface{}),
				cancel:         func() {},
			})

			// Stop the store's goroutine.
			defer store.Stop()

			// Add the channel to the store if it is intended to be found.
			if test.channelFound {
				store.channels[test.chanID] = &chanEventLog{
					events: test.events,
					ChannelTimestamps: ChannelTimestamps{
						ClosedAt:       test.closedAt,
						MonitoredSince: test.monitoredSince,
						now:            func() time.Time { return now },
					},
				}
			}

			uptime, err := store.GetUptime(test.chanID, test.startTime, test.endTime)
			if test.expectErr && err == nil {
				t.Fatal("Expected an error, got nil")
			}
			if !test.expectErr && err != nil {
				t.Fatalf("Expcted no error, got: %v", err)
			}

			if uptime != test.expectedUptime {
				t.Fatalf("Expected uptime percentage: %v, got %v",
					test.expectedUptime, uptime)
			}

		})
	}
}

// TestGetRevenue tests handling of requests for channel revenue when the store
// has knowledge of of the channel and when it is unknown.
func TestGetRevenue(t *testing.T) {
	// Set time for deterministic unit tests.
	now := time.Now()
	twoHoursAgo := now.Add(time.Hour * -2)

	ourID := lnwire.ShortChannelID{BlockHeight: 1, TxIndex: 2, TxPosition: 3}

	tests := []struct {
		name string

		chanID uint64

		events []channeldb.ForwardingEvent

		// channelFound is true if we expect to find the channel in the store.
		channelFound bool

		// startTime specifies the beginning of the uptime range we want to
		// calculate.
		startTime time.Time

		// endTime specified the end of the uptime range we want to calculate.
		endTime time.Time

		// expectedRevenue is the revenue we expect to be returned.
		// Revenue = sum[ (amt in - amt out) * 0.5 ]
		// By default we attribute revenue evenly between incoming and outgoing.
		expectedRevenue lnwire.MilliSatoshi

		expectErr bool
	}{
		{
			name:            "Channel found, no events",
			chanID:          ourID.ToUint64(),
			startTime:       twoHoursAgo,
			endTime:         now,
			channelFound:    true,
			expectedRevenue: 0,
			expectErr:       false,
		},
		{
			name:            "Channel not found, error",
			chanID:          ourID.ToUint64(),
			startTime:       twoHoursAgo,
			endTime:         now,
			channelFound:    false,
			expectedRevenue: 0,
			expectErr:       true,
		},
		{
			name:   "Channel not found, some has events",
			chanID: ourID.ToUint64(),
			events: []channeldb.ForwardingEvent{
				{
					IncomingChanID: ourID,
					OutgoingChanID: lnwire.ShortChannelID{},
					AmtIn:          12,
					AmtOut:         6,
				},
			},
			startTime:       twoHoursAgo,
			endTime:         now,
			channelFound:    true,
			expectedRevenue: 3,
			expectErr:       false,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			// Create and  empty events store for testing.
			store := NewChannelEventStore(&Config{
				QueryForwardLog: func(startTime, endTime time.Time) (events []channeldb.ForwardingEvent, e error) {
					return test.events, nil
				},
			})

			// Start goroutine which consumes GetTimestamps requests.
			store.wg.Add(1)
			go store.consume(&subscriptions{
				channelUpdates: make(chan interface{}),
				peerUpdates:    make(chan interface{}),
				cancel:         func() {},
			})

			// Stop the store's go routine.
			defer store.Stop()

			// Add channel to eventStore if the test indicates that it should
			// be present.
			if test.channelFound {
				store.channels[test.chanID] = &chanEventLog{}
			}

			revenue, err := store.GetRevenue(
				test.chanID, test.startTime, test.endTime,
			)
			hadErr := err != nil
			if hadErr != test.expectErr {
				t.Fatalf("Expected an error: %v, got an error: %v",
					test.expectErr, hadErr)
			}

			if revenue != test.expectedRevenue {
				t.Fatalf("Expected revenue: %v, got; %v",
					test.expectedRevenue, revenue)
			}
		})
	}
}
