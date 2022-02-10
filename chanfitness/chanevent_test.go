package chanfitness

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// TestPeerLog tests the functionality of the peer log struct.
func TestPeerLog(t *testing.T) {
	clock := clock.NewTestClock(testNow)
	peerLog := newPeerLog(clock, 0, nil)

	// assertFlapCount is a helper that asserts that our peer's flap count
	// and timestamp is set to expected values.
	assertFlapCount := func(expectedCount int, expectedTs *time.Time) {
		flapCount, flapTs := peerLog.getFlapCount()
		require.Equal(t, expectedCount, flapCount)
		require.Equal(t, expectedTs, flapTs)
	}

	require.Zero(t, peerLog.channelCount())
	require.False(t, peerLog.online)
	assertFlapCount(0, nil)

	// Test that looking up an unknown channel fails.
	_, _, err := peerLog.channelUptime(wire.OutPoint{Index: 1})
	require.Error(t, err)

	lastFlap := clock.Now()

	// Add an offline event, since we have no channels, we do not expect
	// to have any online periods recorded for our peer. However, we should
	// increment our flap count for the peer.
	peerLog.onlineEvent(false)
	require.Len(t, peerLog.getOnlinePeriods(), 0)
	assertFlapCount(1, &lastFlap)

	// Bump our test clock's time by an hour so that we can create an online
	// event with a distinct time.
	lastFlap = testNow.Add(time.Hour)
	clock.SetTime(lastFlap)

	// Likewise, if we have an online event, nothing beyond the online state
	// of our peer log should change, but our flap count should change.
	peerLog.onlineEvent(true)
	require.Len(t, peerLog.getOnlinePeriods(), 0)
	assertFlapCount(2, &lastFlap)

	// Add a channel and assert that we have one channel listed. Since this
	// is the first channel we track for the peer, we expect an online
	// event to be added, however, our flap count should not change because
	// this is not a new online event, we are just copying one into our log
	// for our purposes.
	chan1 := wire.OutPoint{
		Index: 1,
	}
	require.NoError(t, peerLog.addChannel(chan1))
	require.Equal(t, 1, peerLog.channelCount())
	assertFlapCount(2, &lastFlap)

	// Assert that we can now successfully get our added channel.
	_, _, err = peerLog.channelUptime(chan1)
	require.NoError(t, err)

	// Bump our test clock's time so that our current time is different to
	// channel open time.
	lastFlap = clock.Now().Add(time.Hour)
	clock.SetTime(lastFlap)

	// Now that we have added a channel and an hour has passed, we expect
	// our uptime and lifetime to both equal an hour.
	lifetime, uptime, err := peerLog.channelUptime(chan1)
	require.NoError(t, err)
	require.Equal(t, time.Hour, lifetime)
	require.Equal(t, time.Hour, uptime)

	// Add an offline event for our peer and assert that our flap count is
	// incremented.
	peerLog.onlineEvent(false)
	assertFlapCount(3, &lastFlap)

	// Now we add another channel to our store and assert that we now report
	// two channels for this peer.
	chan2 := wire.OutPoint{
		Index: 2,
	}
	require.NoError(t, peerLog.addChannel(chan2))
	require.Equal(t, 2, peerLog.channelCount())

	// Progress our time again, so that our peer has now been offline for
	// two hours.
	now := lastFlap.Add(time.Hour * 2)
	clock.SetTime(now)

	// Our first channel should report as having been monitored for three
	// hours, but only online for one of those hours.
	lifetime, uptime, err = peerLog.channelUptime(chan1)
	require.NoError(t, err)
	require.Equal(t, time.Hour*3, lifetime)
	require.Equal(t, time.Hour, uptime)

	// Remove our first channel and check that we can still correctly query
	// uptime for the second channel.
	require.NoError(t, peerLog.removeChannel(chan1))
	require.Equal(t, 1, peerLog.channelCount())

	// Our second channel, which was created when our peer was offline,
	// should report as having been monitored for two hours, but have zero
	// uptime.
	lifetime, uptime, err = peerLog.channelUptime(chan2)
	require.NoError(t, err)
	require.Equal(t, time.Hour*2, lifetime)
	require.Equal(t, time.Duration(0), uptime)

	// Finally, remove our second channel and assert that our peer cleans
	// up its in memory set of events but keeps its flap count record.
	require.NoError(t, peerLog.removeChannel(chan2))
	require.Equal(t, 0, peerLog.channelCount())
	require.Len(t, peerLog.onlineEvents, 0)
	assertFlapCount(3, &lastFlap)

	require.Len(t, peerLog.listEvents(), 0)
	require.Nil(t, peerLog.stagedEvent)
}

// TestRateLimitAdd tests the addition of events to the event log with rate
// limiting in place.
func TestRateLimitAdd(t *testing.T) {
	// Create a mock clock specifically for this test so that we can
	// progress time without affecting the other tests.
	mockedClock := clock.NewTestClock(testNow)

	// Create a new peer log.
	peerLog := newPeerLog(mockedClock, 0, nil)
	require.Nil(t, peerLog.stagedEvent)

	// Create a channel for our peer log, otherwise it will not track online
	// events.
	require.NoError(t, peerLog.addChannel(wire.OutPoint{}))

	// First, we add an event to the event log. Since we have no previous
	// events, we expect this event to staged immediately.
	peerEvent := &event{
		timestamp: testNow,
		eventType: peerOfflineEvent,
	}

	peerLog.onlineEvent(false)
	require.Equal(t, peerEvent, peerLog.stagedEvent)

	// We immediately add another event to our event log. We expect our
	// staged event to be replaced with this new event, because insufficient
	// time has passed since our last event.
	peerEvent = &event{
		timestamp: testNow,
		eventType: peerOnlineEvent,
	}

	peerLog.onlineEvent(true)
	require.Equal(t, peerEvent, peerLog.stagedEvent)

	// We get the amount of time that we need to pass before we record an
	// event from our rate limiting tiers. We then progress our test clock
	// to just after this point.
	delta := getRateLimit(peerLog.flapCount)
	newNow := testNow.Add(delta + 1)
	mockedClock.SetTime(newNow)

	// Now, when we add an event, we expect our staged event to be added
	// to our events list and for our new event to be staged.
	newEvent := &event{
		timestamp: newNow,
		eventType: peerOfflineEvent,
	}
	peerLog.onlineEvent(false)

	require.Equal(t, []*event{peerEvent}, peerLog.onlineEvents)
	require.Equal(t, newEvent, peerLog.stagedEvent)

	// Now, we test the case where we add many events to our log. We expect
	// our set of events to be untouched, but for our staged event to be
	// updated.
	nextEvent := &event{
		timestamp: newNow,
		eventType: peerOnlineEvent,
	}

	for i := 0; i < 5; i++ {
		// We flip the kind of event for each type so that we can check
		// that our staged event is definitely changing each time.
		if i%2 == 0 {
			nextEvent.eventType = peerOfflineEvent
		} else {
			nextEvent.eventType = peerOnlineEvent
		}

		online := nextEvent.eventType == peerOnlineEvent

		peerLog.onlineEvent(online)
		require.Equal(t, []*event{peerEvent}, peerLog.onlineEvents)
		require.Equal(t, nextEvent, peerLog.stagedEvent)
	}

	// Now, we test the case where a peer's flap count is cooled down
	// because it has not flapped for a while. Set our peer's flap count so
	// that we fall within our second rate limiting tier and assert that we
	// are at this level.
	peerLog.flapCount = rateLimitScale + 1
	rateLimit := getRateLimit(peerLog.flapCount)
	require.Equal(t, rateLimits[1], rateLimit)

	// Progress our clock to the point where we will have our flap count
	// cooled.
	newNow = mockedClock.Now().Add(flapCountCooldownPeriod)
	mockedClock.SetTime(newNow)

	// Add an online event, and expect it to be staged.
	onlineEvent := &event{
		timestamp: newNow,
		eventType: peerOnlineEvent,
	}
	peerLog.onlineEvent(true)
	require.Equal(t, onlineEvent, peerLog.stagedEvent)

	// Progress our clock by the rate limit level that we will be on if
	// our flap rate is cooled down to a lower level.
	newNow = mockedClock.Now().Add(rateLimits[0] + 1)
	mockedClock.SetTime(newNow)

	// Add another event. We expect this event to be staged and our previous
	// event to be flushed to the event log (because our cooldown has been
	// applied).
	offlineEvent := &event{
		timestamp: newNow,
		eventType: peerOfflineEvent,
	}
	peerLog.onlineEvent(false)
	require.Equal(t, offlineEvent, peerLog.stagedEvent)

	flushedEventIdx := len(peerLog.onlineEvents) - 1
	require.Equal(
		t, onlineEvent, peerLog.onlineEvents[flushedEventIdx],
	)
}

// TestGetOnlinePeriod tests the getOnlinePeriod function. It tests the case
// where no events present, and the case where an additional online period
// must be added because the event log ends on an online event.
func TestGetOnlinePeriod(t *testing.T) {
	fourHoursAgo := testNow.Add(time.Hour * -4)
	threeHoursAgo := testNow.Add(time.Hour * -3)
	twoHoursAgo := testNow.Add(time.Hour * -2)

	tests := []struct {
		name           string
		events         []*event
		expectedOnline []*onlinePeriod
	}{
		{
			name: "no events",
		},
		{
			name: "start on online period",
			events: []*event{
				{
					timestamp: threeHoursAgo,
					eventType: peerOnlineEvent,
				},
				{
					timestamp: twoHoursAgo,
					eventType: peerOfflineEvent,
				},
			},
			expectedOnline: []*onlinePeriod{
				{
					start: threeHoursAgo,
					end:   twoHoursAgo,
				},
			},
		},
		{
			name: "start on offline period",
			events: []*event{
				{
					timestamp: fourHoursAgo,
					eventType: peerOfflineEvent,
				},
			},
		},
		{
			name: "end on an online period",
			events: []*event{
				{
					timestamp: fourHoursAgo,
					eventType: peerOnlineEvent,
				},
			},
			expectedOnline: []*onlinePeriod{
				{
					start: fourHoursAgo,
					end:   testNow,
				},
			},
		},
		{
			name: "duplicate online events",
			events: []*event{
				{
					timestamp: fourHoursAgo,
					eventType: peerOnlineEvent,
				},
				{
					timestamp: threeHoursAgo,
					eventType: peerOnlineEvent,
				},
			},
			expectedOnline: []*onlinePeriod{
				{
					start: fourHoursAgo,
					end:   testNow,
				},
			},
		},
		{
			name: "duplicate offline events",
			events: []*event{
				{
					timestamp: fourHoursAgo,
					eventType: peerOfflineEvent,
				},
				{
					timestamp: threeHoursAgo,
					eventType: peerOfflineEvent,
				},
			},
			expectedOnline: nil,
		},
		{
			name: "duplicate online then offline",
			events: []*event{
				{
					timestamp: fourHoursAgo,
					eventType: peerOnlineEvent,
				},
				{
					timestamp: threeHoursAgo,
					eventType: peerOnlineEvent,
				},
				{
					timestamp: twoHoursAgo,
					eventType: peerOfflineEvent,
				},
			},
			expectedOnline: []*onlinePeriod{
				{
					start: fourHoursAgo,
					end:   twoHoursAgo,
				},
			},
		},
		{
			name: "duplicate offline then online",
			events: []*event{
				{
					timestamp: fourHoursAgo,
					eventType: peerOfflineEvent,
				},
				{
					timestamp: threeHoursAgo,
					eventType: peerOfflineEvent,
				},
				{
					timestamp: twoHoursAgo,
					eventType: peerOnlineEvent,
				},
			},
			expectedOnline: []*onlinePeriod{
				{
					start: twoHoursAgo,
					end:   testNow,
				},
			},
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			score := &peerLog{
				onlineEvents: test.events,
				clock:        clock.NewTestClock(testNow),
			}

			online := score.getOnlinePeriods()

			require.Equal(t, test.expectedOnline, online)
		})
	}
}

// TestUptime tests channel uptime calculation based on its event log.
func TestUptime(t *testing.T) {
	fourHoursAgo := testNow.Add(time.Hour * -4)
	threeHoursAgo := testNow.Add(time.Hour * -3)
	twoHoursAgo := testNow.Add(time.Hour * -2)
	oneHourAgo := testNow.Add(time.Hour * -1)

	tests := []struct {
		name string

		// events is the set of event log that we are calculating uptime
		// for.
		events []*event

		// startTime is the beginning of the period that we are
		// calculating uptime for, it cannot have a zero value.
		startTime time.Time

		// endTime is the end of the period that we are calculating
		// uptime for, it cannot have a zero value.
		endTime time.Time

		// expectedUptime is the amount of uptime we expect to be
		// calculated over the period specified by startTime and
		// endTime.
		expectedUptime time.Duration

		// expectErr is set to true if we expect an error to be returned
		// when calling the uptime function.
		expectErr bool
	}{
		{
			name:      "End before start",
			endTime:   threeHoursAgo,
			startTime: testNow,
			expectErr: true,
		},
		{
			name:      "Zero end time",
			expectErr: true,
		},
		{
			name: "online event and no offline",
			events: []*event{
				{
					timestamp: fourHoursAgo,
					eventType: peerOnlineEvent,
				},
			},
			startTime:      fourHoursAgo,
			endTime:        testNow,
			expectedUptime: time.Hour * 4,
		},
		{
			name: "online then offline event",
			events: []*event{
				{
					timestamp: threeHoursAgo,
					eventType: peerOnlineEvent,
				},
				{
					timestamp: twoHoursAgo,
					eventType: peerOfflineEvent,
				},
			},
			startTime:      fourHoursAgo,
			endTime:        testNow,
			expectedUptime: time.Hour,
		},
		{
			name: "online event before uptime period",
			events: []*event{
				{
					timestamp: threeHoursAgo,
					eventType: peerOnlineEvent,
				},
			},
			startTime:      twoHoursAgo,
			endTime:        testNow,
			expectedUptime: time.Hour * 2,
		},
		{
			name: "offline event after uptime period",
			events: []*event{
				{
					timestamp: fourHoursAgo,
					eventType: peerOnlineEvent,
				},
				{
					timestamp: testNow.Add(time.Hour),
					eventType: peerOfflineEvent,
				},
			},
			startTime:      twoHoursAgo,
			endTime:        testNow,
			expectedUptime: time.Hour * 2,
		},
		{
			name: "all events within period",
			events: []*event{
				{
					timestamp: twoHoursAgo,
					eventType: peerOnlineEvent,
				},
			},
			startTime:      threeHoursAgo,
			endTime:        oneHourAgo,
			expectedUptime: time.Hour,
		},
		{
			name: "multiple online and offline",
			events: []*event{
				{
					timestamp: testNow.Add(time.Hour * -7),
					eventType: peerOnlineEvent,
				},
				{
					timestamp: testNow.Add(time.Hour * -6),
					eventType: peerOfflineEvent,
				},
				{
					timestamp: testNow.Add(time.Hour * -5),
					eventType: peerOnlineEvent,
				},
				{
					timestamp: testNow.Add(time.Hour * -4),
					eventType: peerOfflineEvent,
				},
				{
					timestamp: testNow.Add(time.Hour * -3),
					eventType: peerOnlineEvent,
				},
			},
			startTime:      testNow.Add(time.Hour * -8),
			endTime:        oneHourAgo,
			expectedUptime: time.Hour * 4,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			score := &peerLog{
				onlineEvents: test.events,
				clock:        clock.NewTestClock(testNow),
			}

			uptime, err := score.uptime(
				test.startTime, test.endTime,
			)
			require.Equal(t, test.expectErr, err != nil)
			require.Equal(t, test.expectedUptime, uptime)
		})
	}
}
