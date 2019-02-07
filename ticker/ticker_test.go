package ticker_test

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/ticker"
)

const interval = 50 * time.Millisecond
const numActiveTicks = 3

var tickers = []struct {
	name   string
	ticker ticker.Ticker
}{
	{
		"default ticker",
		ticker.New(interval),
	},
	{
		"mock ticker",
		ticker.NewForce(interval),
	},
}

// TestTickers verifies that both our production and mock tickers exhibit the
// same principle behaviors when accessed via the ticker.Ticker interface
// methods.
func TestInterfaceTickers(t *testing.T) {
	for _, test := range tickers {
		t.Run(test.name, func(t *testing.T) {
			testTicker(t, test.ticker)
		})
	}
}

// testTicker asserts the behavior of a freshly initialized ticker.Ticker.
func testTicker(t *testing.T, ticker ticker.Ticker) {
	// Newly initialized ticker should start off inactive.
	select {
	case <-ticker.Ticks():
		t.Fatalf("ticker should not have ticked before calling Resume")
	case <-time.After(2 * interval):
	}

	// Resume, ticker should be active and start sending ticks.
	ticker.Resume()

	for i := 0; i < numActiveTicks; i++ {
		select {
		case <-ticker.Ticks():
		case <-time.After(2 * interval):
			t.Fatalf(
				"ticker should have ticked after calling Resume",
			)
		}
	}

	// Pause, check that ticker is inactive and sends no ticks.
	ticker.Pause()

	select {
	case <-ticker.Ticks():
		t.Fatalf("ticker should not have ticked after calling Pause")
	case <-time.After(2 * interval):
	}

	// Pause again, expect same behavior as after first invocation.
	ticker.Pause()

	select {
	case <-ticker.Ticks():
		t.Fatalf("ticker should not have ticked after calling Pause again")
	case <-time.After(2 * interval):
	}

	// Resume again, should result in normal active behavior.
	ticker.Resume()

	for i := 0; i < numActiveTicks; i++ {
		select {
		case <-ticker.Ticks():
		case <-time.After(2 * interval):
			t.Fatalf(
				"ticker should have ticked after calling Resume",
			)
		}
	}

	// Stop the ticker altogether, should render it inactive.
	ticker.Stop()

	select {
	case <-ticker.Ticks():
		t.Fatalf("ticker should not have ticked after calling Stop")
	case <-time.After(2 * interval):
	}
}
