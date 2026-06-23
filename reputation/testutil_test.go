package reputation

import (
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
)

// testClock is a deterministic, settable Clock for tests.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock(unix int64) *testClock {
	return &testClock{t: time.Unix(unix, 0)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.t = c.t.Add(d)
}

// circuit builds a CircuitKey from an scid int and htlc id.
func circuit(scid, htlcID uint64) models.CircuitKey {
	return models.CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(scid),
		HtlcID: htlcID,
	}
}

// fakeChannelSource is an in-memory ChannelSource for tests.
type fakeChannelSource struct {
	channels []ChannelInfo
	events   chan ChannelEvent
}

func newFakeChannelSource(infos ...ChannelInfo) *fakeChannelSource {
	return &fakeChannelSource{
		channels: infos,
		events:   make(chan ChannelEvent, 16),
	}
}

func (f *fakeChannelSource) ActiveChannels() ([]ChannelInfo, error) {
	return f.channels, nil
}

func (f *fakeChannelSource) SubscribeChannelEvents() (<-chan ChannelEvent,
	func(), error) {

	return f.events, func() {}, nil
}

// chanInfo builds a ChannelInfo for tests.
func chanInfo(scid uint64, maxHTLCs uint16,
	maxInFlight lnwire.MilliSatoshi) ChannelInfo {

	return ChannelInfo{
		SCID:             lnwire.NewShortChanIDFromInt(scid),
		MaxAcceptedHTLCs: maxHTLCs,
		MaxInFlightMsat:  maxInFlight,
	}
}
