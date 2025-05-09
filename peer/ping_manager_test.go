package peer

import (
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestPingManager tests three main properties about the ping manager. It
// ensures that if the pong response exceeds the timeout, that a failure is
// emitted on the failure channel. It ensures that if the Pong response is
// not congruent with the outstanding ping then a failure is emitted on the
// failure channel, and otherwise the failure channel remains empty.
func TestPingManager(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		delay    int
		pongSize uint16
		result   bool
	}{
		{
			name:     "happy Path",
			delay:    0,
			pongSize: 4,
			result:   true,
		},
		{
			name:     "bad Pong",
			delay:    0,
			pongSize: 3,
			result:   false,
		},
		{
			name:     "timeout",
			delay:    2,
			pongSize: 4,
			result:   false,
		},
	}

	payload := make([]byte, 4)
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			// Set up PingManager.
			var pingOnce sync.Once
			pingSent := make(chan struct{})
			disconnected := make(chan struct{})
			mgr := NewPingManager(&PingManagerConfig{
				NewPingPayload: func() []byte {
					return payload
				},
				NewPongSize: func() uint16 {
					return 4
				},
				IntervalDuration: time.Second * 2,
				TimeoutDuration:  time.Second,
				SendPing: func(ping *lnwire.Ping) {
					pingOnce.Do(func() {
						close(pingSent)
					})
				},
				OnPongFailure: func(err error,
					_ time.Duration, _ time.Duration) {

					close(disconnected)
				},
			})
			require.NoError(
				t, mgr.Start(), "Could not start pingManager",
			)

			// Wait for initial Ping.
			<-pingSent

			// Wait for pre-determined time before sending Pong
			// response.
			time.Sleep(time.Duration(test.delay) * time.Second)

			// Send Pong back.
			res := lnwire.Pong{
				PongBytes: make([]byte, test.pongSize),
			}
			mgr.ReceivedPong(&res)

			select {
			case <-time.NewTimer(time.Second / 2).C:
				require.True(t, test.result)
			case <-disconnected:
				require.False(t, test.result)
			}

			mgr.Stop()
		})
	}
}
