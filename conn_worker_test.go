package lnd

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// testAddr creates a test lnwire.NetAddress with the given IP string.
func testAddr(t *testing.T, ip string,
	pub *btcec.PublicKey) *lnwire.NetAddress {

	t.Helper()

	return &lnwire.NetAddress{
		IdentityKey: pub,
		Address: &net.TCPAddr{
			IP:   net.ParseIP(ip),
			Port: 9735,
		},
	}
}

// dialCall records the address and timestamp of a mock dial attempt.
type dialCall struct {
	addr *lnwire.NetAddress
	time time.Time
}

// mockDialer is a controllable dialer for testing connWorker. Each dial blocks
// until the test sends a result on resultCh, allowing precise control over
// timing.
type mockDialer struct {
	t        *testing.T
	mu       sync.Mutex
	calls    []dialCall
	resultCh chan error
}

func newMockDialer(t *testing.T) *mockDialer {
	return &mockDialer{
		t:        t,
		resultCh: make(chan error, 1),
	}
}

func (d *mockDialer) dial(ctx context.Context, addr *lnwire.NetAddress) (
	net.Conn, error) {

	d.mu.Lock()
	d.calls = append(d.calls, dialCall{addr: addr, time: time.Now()})
	d.mu.Unlock()

	select {
	case err := <-d.resultCh:
		if err != nil {
			return nil, err
		}

		// Return a pipe conn as a stand-in for a real connection.
		client, server := net.Pipe()
		d.t.Cleanup(func() { server.Close() })

		return client, nil

	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (d *mockDialer) getCallCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return len(d.calls)
}

func (d *mockDialer) getCalls() []dialCall {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]dialCall, len(d.calls))
	copy(out, d.calls)

	return out
}

// testWorkerHarness bundles a connWorker and its test dependencies.
type testWorkerHarness struct {
	worker  *connWorker
	dialer  *mockDialer
	connCh  chan net.Conn
	quit    chan struct{}
	pub     *btcec.PublicKey
	stopped chan struct{}
}

func newTestWorkerHarness(t *testing.T) *testWorkerHarness {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pub := priv.PubKey()

	d := newMockDialer(t)
	connCh := make(chan net.Conn, 1)
	quit := make(chan struct{})

	cfg := connWorkerCfg{
		dialContext: d.dial,
		onConnection: func(conn net.Conn, addr *lnwire.NetAddress) {
			connCh <- conn
		},
		minBackoff:         100 * time.Millisecond,
		maxBackoff:         2 * time.Second,
		stableConnDuration: 10 * time.Minute,
		staggerDelay:       50 * time.Millisecond,
		quit:               quit,
	}

	pubStr := string(pub.SerializeCompressed())
	w := newConnWorker(pubStr, false, cfg)
	stopped := make(chan struct{})

	return &testWorkerHarness{
		worker:  w,
		dialer:  d,
		connCh:  connCh,
		quit:    quit,
		pub:     pub,
		stopped: stopped,
	}
}

// start launches the worker goroutine and returns a channel that closes when
// Run exits.
func (h *testWorkerHarness) start() {
	go func() {
		h.worker.Run()
		close(h.stopped)
	}()
}

// sendCmd sends a command to the worker, failing if it blocks.
func (h *testWorkerHarness) sendCmd(t *testing.T, msg connWorkerMsg) {
	t.Helper()

	select {
	case h.worker.cmdChan <- msg:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out sending command to worker")
	}
}

// waitStopped waits for the worker goroutine to exit.
func (h *testWorkerHarness) waitStopped(t *testing.T) {
	t.Helper()

	select {
	case <-h.stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for worker to stop")
	}
}

// TestConnWorkerDialsAllAddresses verifies that the worker dials each address
// in order with the stagger delay between them.
func TestConnWorkerDialsAllAddresses(t *testing.T) {
	t.Parallel()

	h := newTestWorkerHarness(t)
	h.start()

	addrs := []*lnwire.NetAddress{
		testAddr(t, "1.1.1.1", h.pub),
		testAddr(t, "2.2.2.2", h.pub),
		testAddr(t, "3.3.3.3", h.pub),
	}

	now := time.Now()
	h.sendCmd(t, connWorkerMsg{
		cmd:   cmdConnect,
		addrs: addrs,
	})

	// First address: fail it immediately.
	h.dialer.resultCh <- errors.New("refused")

	// Second address: fail it too.
	h.dialer.resultCh <- errors.New("refused")

	// Third address: succeed.
	h.dialer.resultCh <- nil

	// Expect the connection to be delivered.
	select {
	case conn := <-h.connCh:
		require.NotNil(t, conn)
		conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for connection")
	}

	// TODO: Assert that total time should be at least 2x the stagger delay
	// due to the two failures.
	t.Logf("total time to dial all last address: %v", time.Since(now))

	// Verify all 3 addresses were dialed in order.
	calls := h.dialer.getCalls()
	require.Len(t, calls, 3)
	require.Equal(t, "1.1.1.1:9735", calls[0].addr.Address.String())
	require.Equal(t, "2.2.2.2:9735", calls[1].addr.Address.String())
	require.Equal(t, "3.3.3.3:9735", calls[2].addr.Address.String())

	// Stop the worker.
	h.sendCmd(t, connWorkerMsg{cmd: cmdStop})
	h.waitStopped(t)
}

// TestConnWorkerStandDownCancelsDial verifies that cmdStandDown cancels an
// in-progress dial and returns the worker to idle.
func TestConnWorkerStandDownCancelsDial(t *testing.T) {
	t.Parallel()

	h := newTestWorkerHarness(t)
	h.start()

	addrs := []*lnwire.NetAddress{
		testAddr(t, "1.1.1.1", h.pub),
	}

	h.sendCmd(t, connWorkerMsg{
		cmd:   cmdConnect,
		addrs: addrs,
	})

	// Wait for dial to start.
	require.Eventually(
		t,
		func() bool {
			return h.dialer.getCallCount() >= 1
		},
		2*time.Second,
		10*time.Millisecond,
	)

	// Send stand down — the dial should be canceled via context.
	h.sendCmd(t, connWorkerMsg{cmd: cmdStandDown})

	// The worker should be idle, not stopped. Send another connect to
	// verify it's alive.
	h.sendCmd(t, connWorkerMsg{
		cmd:   cmdConnect,
		addrs: addrs,
	})

	// Wait for the second dial to start before sending a result.
	require.Eventually(
		t,
		func() bool {
			return h.dialer.getCallCount() >= 2
		},
		2*time.Second,
		10*time.Millisecond,
	)

	// Let the second dial succeed.
	h.dialer.resultCh <- nil
	select {
	case conn := <-h.connCh:
		require.NotNil(t, conn)
		conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for connection after stand down")
	}

	h.sendCmd(t, connWorkerMsg{cmd: cmdStop})
	h.waitStopped(t)
}

// TestConnWorkerCmdStopTerminates verifies that cmdStop exits the worker
// goroutine from any state.
func TestConnWorkerCmdStopTerminates(t *testing.T) {
	t.Parallel()

	h := newTestWorkerHarness(t)
	h.start()

	// Stop immediately without any connect command.
	h.sendCmd(t, connWorkerMsg{cmd: cmdStop})
	h.waitStopped(t)
}

// TestConnWorkerCmdStopDuringDial verifies that cmdStop terminates the worker
// even when a dial is in progress.
func TestConnWorkerCmdStopDuringDial(t *testing.T) {
	t.Parallel()

	h := newTestWorkerHarness(t)
	h.start()

	h.sendCmd(
		t, connWorkerMsg{
			cmd: cmdConnect,
			addrs: []*lnwire.NetAddress{
				testAddr(t, "1.1.1.1", h.pub),
				testAddr(t, "2.2.2.2", h.pub),
			},
		},
	)

	// Fail first dial so the worker moves to stagger wait.
	h.dialer.resultCh <- errors.New("refused")

	// During stagger wait, send stop.
	h.sendCmd(t, connWorkerMsg{cmd: cmdStop})
	h.waitStopped(t)
}

// TestConnWorkerUpdateAddrsDuringDial verifies that cmdUpdateAddrs during a
// stagger wait restarts the round with new addresses.
func TestConnWorkerUpdateAddrsDuringDial(t *testing.T) {
	t.Parallel()

	h := newTestWorkerHarness(t)

	// Use a longer stagger so we can intercept it.
	h.worker.cfg.staggerDelay = 500 * time.Millisecond
	h.start()

	addrs1 := []*lnwire.NetAddress{
		testAddr(t, "1.1.1.1", h.pub),
		testAddr(t, "2.2.2.2", h.pub),
	}

	h.sendCmd(t, connWorkerMsg{
		cmd:   cmdConnect,
		addrs: addrs1,
	})

	// Fail first dial so worker enters stagger wait.
	h.dialer.resultCh <- errors.New("refused")

	// During stagger wait, update addresses. The following sleep is needed
	// to enter stagget wait, otherwise we will update the addrs immediately
	// and then attemt a new round of dials. It's not strictly needed, but
	// makes sure to test another path.
	time.Sleep(50 * time.Millisecond)
	newAddrs := []*lnwire.NetAddress{
		testAddr(t, "9.9.9.9", h.pub),
	}
	h.sendCmd(t, connWorkerMsg{
		cmd:   cmdUpdateAddrs,
		addrs: newAddrs,
	})

	// The worker should restart and dial the new address.
	h.dialer.resultCh <- nil
	select {
	case conn := <-h.connCh:
		require.NotNil(t, conn)
		conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for connection")
	}

	// Verify the new address was dialed.
	calls := h.dialer.getCalls()
	lastCall := calls[len(calls)-1]
	require.Equal(t, "9.9.9.9:9735", lastCall.addr.Address.String())

	h.sendCmd(t, connWorkerMsg{cmd: cmdStop})
	h.waitStopped(t)
}

// TestConnWorkerBackoffDoublesOnFailure verifies that the worker retries with
// increasing delays after failed dials. With minBackoff=100ms the expected
// gaps are ~100ms, ~200ms, ~400ms (each doubled, ±5% jitter). We assert each
// gap is at least half the nominal value and that gaps grow monotonically.
func TestConnWorkerBackoffDoublesOnFailure(t *testing.T) {
	t.Parallel()

	h := newTestWorkerHarness(t)
	h.worker.cfg.staggerDelay = 0
	h.start()

	addrs := []*lnwire.NetAddress{
		testAddr(t, "1.1.1.1", h.pub),
	}

	// Connect with zero backoff — first dial is immediate.
	h.sendCmd(t, connWorkerMsg{
		cmd:   cmdConnect,
		addrs: addrs,
	})

	// Fail 3 rounds so backoff increases: 0 → 100ms → ~200ms → ~400ms.
	for i := range 3 {
		require.Eventually(
			t,
			func() bool {
				return h.dialer.getCallCount() > i
			},
			5*time.Second,
			10*time.Millisecond,
		)
		h.dialer.resultCh <- errors.New("refused")
	}

	// Wait for the 4th dial to start, then succeed.
	require.Eventually(
		t,
		func() bool {
			return h.dialer.getCallCount() >= 4
		},
		5*time.Second,
		10*time.Millisecond,
	)
	h.dialer.resultCh <- nil

	select {
	case conn := <-h.connCh:
		conn.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for connection")
	}

	// Verify the backoff gaps between consecutive dials. The sequence
	// of waits before each dial is: 0, 100ms, ~200ms, ~400ms.
	calls := h.dialer.getCalls()
	require.Len(t, calls, 4)

	for i := 1; i < len(calls); i++ {
		gap := calls[i].time.Sub(calls[i-1].time)
		t.Logf("gap[%d→%d] = %v", i-1, i, gap)
	}

	// Gap 1→2 should be ~100ms (minBackoff). Allow ≥50ms.
	gap1 := calls[1].time.Sub(calls[0].time)
	require.GreaterOrEqual(t, gap1, 50*time.Millisecond,
		"first backoff too short")

	// Gap 2→3 should be ~200ms. Assert ≥100ms and > gap1.
	gap2 := calls[2].time.Sub(calls[1].time)
	require.GreaterOrEqual(t, gap2, 100*time.Millisecond,
		"second backoff too short")
	require.Greater(t, gap2, gap1,
		"backoff should increase")

	// Gap 3→4 should be ~400ms. Assert ≥200ms and > gap2.
	gap3 := calls[3].time.Sub(calls[2].time)
	require.GreaterOrEqual(t, gap3, 200*time.Millisecond,
		"third backoff too short")
	require.Greater(t, gap3, gap2,
		"backoff should keep increasing")

	h.sendCmd(t, connWorkerMsg{cmd: cmdStop})
	h.waitStopped(t)
}

// TestConnWorkerIdleAfterSuccess verifies that after a successful dial, the
// worker returns to idle and waits for the next command.
func TestConnWorkerIdleAfterSuccess(t *testing.T) {
	t.Parallel()

	h := newTestWorkerHarness(t)
	h.start()

	addrs := []*lnwire.NetAddress{
		testAddr(t, "1.1.1.1", h.pub),
	}

	// First connect: succeed immediately.
	h.sendCmd(t, connWorkerMsg{
		cmd:   cmdConnect,
		addrs: addrs,
	})
	h.dialer.resultCh <- nil

	select {
	case conn := <-h.connCh:
		conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}

	// Worker should be idle. No more dials should happen for a while.
	startCount := h.dialer.getCallCount()
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, startCount, h.dialer.getCallCount())

	// A second connect should work.
	h.sendCmd(t, connWorkerMsg{
		cmd:   cmdConnect,
		addrs: addrs,
	})
	h.dialer.resultCh <- nil
	select {
	case conn := <-h.connCh:
		conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("timed out on second connect")
	}
	require.Greater(t, h.dialer.getCallCount(), startCount)

	h.sendCmd(t, connWorkerMsg{cmd: cmdStop})
	h.waitStopped(t)
}

// TestConnWorkerQuitTerminates verifies that closing the quit channel
// terminates the worker from some state.
func TestConnWorkerQuitTerminates(t *testing.T) {
	t.Parallel()

	h := newTestWorkerHarness(t)
	h.start()

	// Start a dial.
	h.sendCmd(
		t, connWorkerMsg{
			cmd: cmdConnect,
			addrs: []*lnwire.NetAddress{
				testAddr(t, "1.1.1.1", h.pub),
			},
		},
	)

	// Wait for dial to begin.
	require.Eventually(
		t,
		func() bool {
			return h.dialer.getCallCount() >= 1
		},
		2*time.Second,
		10*time.Millisecond,
	)

	// Close quit channel.
	close(h.quit)
	h.waitStopped(t)
}

// TestConnWorkerConnectWithBackoff verifies that when cmdConnect includes a
// non-zero backoff, the worker waits before dialing.
func TestConnWorkerConnectWithBackoff(t *testing.T) {
	t.Parallel()

	h := newTestWorkerHarness(t)
	h.start()
	startTime := time.Now()

	addrs := []*lnwire.NetAddress{
		testAddr(t, "1.1.1.1", h.pub),
	}

	// Send connect with a 200ms backoff.
	h.sendCmd(
		t, connWorkerMsg{
			cmd:     cmdConnect,
			addrs:   addrs,
			backoff: 200 * time.Millisecond,
		},
	)

	// No dial should happen during the backoff window.
	time.Sleep(100 * time.Millisecond)
	require.Equal(
		t, 0, h.dialer.getCallCount(), "should not dial during backoff",
	)

	// After the backoff elapses, the dial should start.
	require.Eventually(
		t,
		func() bool {
			return h.dialer.getCallCount() >= 1
		},
		2*time.Second,
		10*time.Millisecond,
	)

	// Log the elapsed time to verify it's at least the backoff duration.
	timeDial := h.dialer.getCalls()[0].time.Sub(startTime)
	require.GreaterOrEqual(
		t, timeDial, 200*time.Millisecond,
		"dial should happen after backoff",
	)

	// Succeed on the dial.
	h.dialer.resultCh <- nil
	select {
	case conn := <-h.connCh:
		conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}

	h.sendCmd(t, connWorkerMsg{cmd: cmdStop})
	h.waitStopped(t)
}

// TestConnWorkerRapidCycling bombards the worker with a random stream of
// commands to verify it never deadlocks or gets stuck regardless of ordering.
func TestConnWorkerRapidCycling(t *testing.T) {
	t.Parallel()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pub := priv.PubKey()

	quit := make(chan struct{})
	cfg := connWorkerCfg{
		// Dialer that always fails immediately — no channel
		// coordination needed. This lets the worker churn through
		// dial attempts at full speed.
		dialContext: func(ctx context.Context,
			addr *lnwire.NetAddress) (net.Conn, error) {

			return nil, errors.New("refused")
		},
		onConnection:       func(net.Conn, *lnwire.NetAddress) {},
		minBackoff:         time.Millisecond,
		maxBackoff:         5 * time.Millisecond,
		stableConnDuration: 10 * time.Minute,
		staggerDelay:       time.Millisecond,
		quit:               quit,
	}

	pubStr := string(pub.SerializeCompressed())
	w := newConnWorker(pubStr, false, cfg)
	stopped := make(chan struct{})
	go func() {
		w.Run()
		close(stopped)
	}()

	addrs := []*lnwire.NetAddress{
		testAddr(t, "1.1.1.1", pub),
		testAddr(t, "2.2.2.2", pub),
		testAddr(t, "3.3.3.3", pub),
	}

	send := func(msg connWorkerMsg) {
		t.Helper()
		select {
		case w.cmdChan <- msg:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out sending command — worker stuck")
		}
	}

	// Bombard with random commands. The worker must handle any sequence
	// without deadlocking — redundant stand-downs while idle, back-to-back
	// connects, addr updates with no active dial, etc.
	for range 200 {
		switch rand.IntN(5) {
		case 0:
			send(connWorkerMsg{
				cmd:   cmdConnect,
				addrs: addrs[:1+rand.IntN(len(addrs))],
			})

		case 1:
			send(connWorkerMsg{
				cmd:   cmdConnect,
				addrs: addrs[:1+rand.IntN(len(addrs))],
				backoff: time.Duration(
					rand.IntN(3),
				) * time.Millisecond,
			})

		case 2:
			send(connWorkerMsg{
				cmd:   cmdUpdateAddrs,
				addrs: addrs[:1+rand.IntN(len(addrs))],
			})

		case 3:
			send(connWorkerMsg{cmd: cmdStandDown})

		case 4:
			// Double stand-down — tests the idle no-op path.
			send(connWorkerMsg{cmd: cmdStandDown})
			send(connWorkerMsg{cmd: cmdStandDown})
		}
	}

	// The worker must always terminate on cmdStop.
	send(connWorkerMsg{cmd: cmdStop})
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not terminate — likely stuck")
	}
}
