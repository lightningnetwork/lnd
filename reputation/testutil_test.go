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

// sync blocks until the worker has processed every event enqueued before this
// call. It sends a barrier event (blocking, so it is never dropped) and waits
// for the worker to close its done channel, giving tests a deterministic point
// at which all prior forward/settle/fail events have been applied.
func (m *Manager) sync() {
	done := make(chan struct{})
	m.events <- event{kind: evBarrier, done: done}
	<-done
}

// circuit builds a CircuitKey from an scid int and htlc id.
func circuit(scid, htlcID uint64) models.CircuitKey {
	return models.CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(scid),
		HtlcID: htlcID,
	}
}
