package peer

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
)

// PingManagerConfig is a structure containing various parameters that govern
// how the PingManager behaves.
type PingManagerConfig struct {
	// NewPingPayload is a closure that returns the payload to be packaged
	// in the Ping message.
	NewPingPayload func() []byte

	// NewPongSize is a closure that returns a random value between
	// [0, lnwire.MaxPongBytes]. This random value helps to more effectively
	// pair Pong messages with Ping.
	NewPongSize func() uint16

	// IntervalDuration is the Duration between attempted pings.
	IntervalDuration time.Duration

	// TimeoutDuration is the Duration we wait before declaring a ping
	// attempt failed.
	TimeoutDuration time.Duration

	// SendPing is a closure that is responsible for sending the Ping
	// message out to our peer
	SendPing func(ping *lnwire.Ping)

	// OnPongFailure is a closure that is responsible for executing the
	// logic when a Pong message is either late or does not match our
	// expectations for that Pong
	OnPongFailure func(error)
}

// PingManager is a structure that is designed to manage the internal state
// of the ping pong lifecycle with the remote peer. We assume there is only one
// ping outstanding at once.
//
// NOTE: This structure MUST be initialized with NewPingManager.
type PingManager struct {
	cfg *PingManagerConfig

	// pingTime is a rough estimate of the RTT (round-trip-time) between us
	// and the connected peer.
	// To be used atomically.
	// TODO(roasbeef): also use a WMA or EMA?
	pingTime atomic.Pointer[time.Duration]

	// pingLastSend is the time when we sent our last ping message.
	// To be used atomically.
	pingLastSend *time.Time

	// outstandingPongSize is the current size of the requested pong
	// payload.  This value can only validly range from [0,65531]. Any
	// value < 0 is interpreted as if there is no outstanding ping message.
	outstandingPongSize int32

	// pingTicker is a pointer to a Ticker that fires on every ping
	// interval.
	pingTicker *time.Ticker

	// pingTimeout is a Timer that will fire when we want to time out a
	// ping
	pingTimeout *time.Timer

	// pongChan is the channel on which the pingManager will write Pong
	// messages it is evaluating
	pongChan chan *lnwire.Pong

	started sync.Once
	stopped sync.Once

	quit chan struct{}
	wg   sync.WaitGroup
}

// NewPingManager constructs a pingManager in a valid state. It must be started
// before it does anything useful, though.
func NewPingManager(cfg *PingManagerConfig) *PingManager {
	m := PingManager{
		cfg:                 cfg,
		outstandingPongSize: -1,
		pongChan:            make(chan *lnwire.Pong, 1),
		quit:                make(chan struct{}),
	}

	return &m
}

// Start launches the primary goroutine that is owned by the pingManager.
func (m *PingManager) Start() error {
	var err error
	m.started.Do(func() {
		m.pingTicker = time.NewTicker(m.cfg.IntervalDuration)
		m.pingTimeout = time.NewTimer(0)

		m.wg.Add(1)
		go m.pingHandler()
	})

	return err
}

// pingHandler is the main goroutine responsible for enforcing the ping/pong
// protocol.
func (m *PingManager) pingHandler() {
	defer m.wg.Done()
	defer m.pingTimeout.Stop()

	// Ensure that the pingTimeout channel is empty.
	if !m.pingTimeout.Stop() {
		<-m.pingTimeout.C
	}

	for {
		select {
		case <-m.pingTicker.C:
			// If this occurs it means that the new ping cycle has
			// begun while there is still an outstanding ping
			// awaiting a pong response.  This should never occur,
			// but if it does, it implies a timeout.
			if m.outstandingPongSize >= 0 {
				e := errors.New("impossible: new ping" +
					"in unclean state",
				)
				m.cfg.OnPongFailure(e)

				return
			}

			pongSize := m.cfg.NewPongSize()
			ping := &lnwire.Ping{
				NumPongBytes: pongSize,
				PaddingBytes: m.cfg.NewPingPayload(),
			}

			// Set up our bookkeeping for the new Ping.
			if err := m.setPingState(pongSize); err != nil {
				m.cfg.OnPongFailure(err)

				return
			}

			m.cfg.SendPing(ping)

		case <-m.pingTimeout.C:
			m.resetPingState()

			e := errors.New("timeout while waiting for " +
				"pong response",
			)

			m.cfg.OnPongFailure(e)

			return

		case pong := <-m.pongChan:
			pongSize := int32(len(pong.PongBytes))

			// Save off values we are about to override when we
			// call resetPingState.
			expected := m.outstandingPongSize
			lastPing := m.pingLastSend

			m.resetPingState()

			// If the pong we receive doesn't match the ping we
			// sent out, then we fail out.
			if pongSize != expected {
				e := errors.New("pong response does " +
					"not match expected size",
				)

				m.cfg.OnPongFailure(e)

				return
			}

			// Compute RTT of ping and save that for future
			// querying.
			if lastPing != nil {
				rtt := time.Since(*lastPing)
				m.pingTime.Store(&rtt)
			}

		case <-m.quit:
			return
		}
	}
}

// Stop interrupts the goroutines that the PingManager owns.
func (m *PingManager) Stop() {
	if m.pingTicker == nil {
		return
	}

	m.stopped.Do(func() {
		close(m.quit)
		m.wg.Wait()

		m.pingTicker.Stop()
		m.pingTimeout.Stop()
	})
}

// setPingState is a private method to keep track of all of the fields we need
// to set when we send out a Ping.
func (m *PingManager) setPingState(pongSize uint16) error {
	t := time.Now()
	m.pingLastSend = &t
	m.outstandingPongSize = int32(pongSize)
	if m.pingTimeout.Reset(m.cfg.TimeoutDuration) {
		return fmt.Errorf(
			"impossible: ping timeout reset when already active",
		)
	}

	return nil
}

// resetPingState is a private method that resets all of the bookkeeping that
// is tracking a currently outstanding Ping.
func (m *PingManager) resetPingState() {
	m.pingLastSend = nil
	m.outstandingPongSize = -1
	if !m.pingTimeout.Stop() {
		select {
		case <-m.pingTimeout.C:
		default:
		}
	}
}

// GetPingTimeMicroSeconds reports back the RTT calculated by the pingManager.
func (m *PingManager) GetPingTimeMicroSeconds() int64 {
	rtt := m.pingTime.Load()

	if rtt == nil {
		return -1
	}

	return rtt.Microseconds()
}

// ReceivedPong is called to evaluate a Pong message against the expectations
// we have for it. It will cause the PingManager to invoke the supplied
// OnPongFailure function if the Pong argument supplied violates expectations.
func (m *PingManager) ReceivedPong(msg *lnwire.Pong) {
	select {
	case m.pongChan <- msg:
	case <-m.quit:
	}
}
