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
	peerLog := newPeerLog(clock)

	require.Zero(t, peerLog.channelCount())

	// Test that looking up an unknown channel fails.
	_, err := peerLog.channelLifetime(wire.OutPoint{Index: 1})
	require.Error(t, err)

	// Bump our test clock's time by an hour so that we can create an online
	// event with a distinct time.
	lastFlap := testNow.Add(time.Hour)
	clock.SetTime(lastFlap)

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

	// Assert that we can now successfully get our added channel.
	_, err = peerLog.channelLifetime(chan1)
	require.NoError(t, err)

	// Bump our test clock's time so that our current time is different to
	// channel open time.
	lastFlap = clock.Now().Add(time.Hour)
	clock.SetTime(lastFlap)

	// Now that we have added a channel and an hour has passed, we expect
	// our uptime and lifetime to both equal an hour.
	lifetime, err := peerLog.channelLifetime(chan1)
	require.NoError(t, err)
	require.Equal(t, time.Hour, lifetime)

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
	lifetime, err = peerLog.channelLifetime(chan1)
	require.NoError(t, err)
	require.Equal(t, time.Hour*3, lifetime)

	// Remove our first channel and check that we can still correctly query
	// uptime for the second channel.
	require.NoError(t, peerLog.removeChannel(chan1))
	require.Equal(t, 1, peerLog.channelCount())

	// Our second channel, which was created when our peer was offline,
	// should report as having been monitored for two hours, but have zero
	// uptime.
	lifetime, err = peerLog.channelLifetime(chan2)
	require.NoError(t, err)
	require.Equal(t, time.Hour*2, lifetime)

	// Finally, remove our second channel and assert that our peer cleans
	// up its in memory set of events but keeps its flap count record.
	require.NoError(t, peerLog.removeChannel(chan2))
	require.Equal(t, 0, peerLog.channelCount())
}
