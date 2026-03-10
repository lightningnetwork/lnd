package lnd

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/peer"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/require"
)

// mockNet implements tor.Net for testing. All calls return errors
// since tests that use workers mock the dialer at a higher level.
type mockNet struct{}

var _ tor.Net = (*mockNet)(nil)

func (m *mockNet) Dial(network, addr string,
	timeout time.Duration) (net.Conn, error) {

	return nil, errors.New("mock: no real dialing")
}

func (m *mockNet) LookupHost(host string) ([]string, error) {
	return nil, errors.New("mock: no DNS")
}

func (m *mockNet) LookupSRV(service, proto, name string,
	timeout time.Duration) (string, []*net.SRV, error) {

	return "", nil, errors.New("mock: no SRV")
}

func (m *mockNet) ResolveTCPAddr(network,
	address string) (*net.TCPAddr, error) {

	return nil, errors.New("mock: no resolve")
}

// newTestServer creates a minimal server instance with only the fields needed
// for persistent connection management tests.
func newTestServer(t *testing.T) *server {
	t.Helper()

	// Generate a test identity key for the server.
	identityPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	s := &server{
		cfg: &Config{
			MinBackoff: time.Second,
			MaxBackoff: time.Hour,
			Dev:        &lncfg.DevConfig{},
			net:        &mockNet{},
		},
		identityECDH: &keychain.PrivKeyECDH{
			PrivKey: identityPriv,
		},
		persistentWorkers:       make(map[string]*connWorker),
		peersByPub:              make(map[string]*peer.Brontide),
		inboundPeers:            make(map[string]*peer.Brontide),
		outboundPeers:           make(map[string]*peer.Brontide),
		ignorePeerTermination:   make(map[*peer.Brontide]struct{}),
		scheduledPeerConnection: make(map[string]func()),
		quit:                    make(chan struct{}),
	}

	return s
}

// generateTestPubKey creates a new random public key for testing.
func generateTestPubKey(t *testing.T) *btcec.PublicKey {
	t.Helper()
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	return priv.PubKey()
}

// TestNodeAnnouncementTimestampComparison tests the timestamp comparison
// logic used in setSelfNode to ensure node announcements have strictly
// increasing timestamps at second precision (as required by BOLT-07 and
// enforced by the database storage).
func TestNodeAnnouncementTimestampComparison(t *testing.T) {
	t.Parallel()

	// Use a simple base time for the tests.
	baseTime := int64(1000)

	tests := []struct {
		name              string
		srcNodeLastUpdate time.Time
		nodeLastUpdate    time.Time
		expectedResult    time.Time
		description       string
	}{
		{
			name:              "same second different nanoseconds",
			srcNodeLastUpdate: time.Unix(baseTime, 0),
			nodeLastUpdate:    time.Unix(baseTime, 500_000_000),
			expectedResult:    time.Unix(baseTime+1, 0),
			description: "Edge case: timestamps in same second " +
				"but different nanoseconds. Must increment " +
				"to avoid persisting same second-level " +
				"timestamp.",
		},
		{
			name:              "different seconds",
			srcNodeLastUpdate: time.Unix(baseTime, 0),
			nodeLastUpdate:    time.Unix(baseTime+2, 0),
			expectedResult:    time.Unix(baseTime+2, 0),
			description: "Normal case: current time is already " +
				"in a different (later) second. No increment " +
				"needed.",
		},
		{
			name:              "exactly equal",
			srcNodeLastUpdate: time.Unix(baseTime, 123456789),
			nodeLastUpdate:    time.Unix(baseTime, 123456789),
			expectedResult:    time.Unix(baseTime+1, 123456789),
			description: "Timestamps are identical. Must " +
				"increment to ensure strictly greater " +
				"timestamp.",
		},
		{
			name:              "exactly equal - zero nanoseconds",
			srcNodeLastUpdate: time.Unix(baseTime, 0),
			nodeLastUpdate:    time.Unix(baseTime, 0),
			expectedResult:    time.Unix(baseTime+1, 0),
			description: "Timestamps are identical at second " +
				"precision (0 nanoseconds), as would be read " +
				"from DB. Must increment.",
		},
		{
			name:              "clock skew - persisted is newer",
			srcNodeLastUpdate: time.Unix(baseTime+5, 0),
			nodeLastUpdate:    time.Unix(baseTime+3, 0),
			expectedResult:    time.Unix(baseTime+6, 0),
			description: "Clock went backwards: persisted " +
				"timestamp is newer than current time. Must " +
				"increment from persisted timestamp.",
		},
		{
			name:              "clock skew - same second",
			srcNodeLastUpdate: time.Unix(baseTime+5, 100_000_000),
			nodeLastUpdate:    time.Unix(baseTime+5, 900_000_000),
			expectedResult:    time.Unix(baseTime+6, 100_000_000),
			description: "Clock skew within same second. Must " +
				"increment to ensure strictly greater " +
				"second-level timestamp.",
		},
		{
			name: "same second component different " +
				"minute",
			srcNodeLastUpdate: time.Unix(baseTime, 0),
			nodeLastUpdate:    time.Unix(baseTime+60, 0),
			expectedResult:    time.Unix(baseTime+60, 0),
			description: "Same seconds component (:00) but " +
				"different minutes. Current time is later. " +
				"Verifies we use .Unix() not .Second().",
		},
		{
			name: "lower second component but " +
				"later time",
			srcNodeLastUpdate: time.Unix(baseTime+58, 0),
			nodeLastUpdate:    time.Unix(baseTime+63, 0),
			expectedResult:    time.Unix(baseTime+63, 0),
			description: "Persisted has second=58, current has " +
				"second=3 (next minute). Current is later " +
				"overall. Verifies .Unix() not .Second().",
		},
		{
			name: "higher second component but " +
				"earlier time",
			srcNodeLastUpdate: time.Unix(baseTime+63, 0),
			nodeLastUpdate:    time.Unix(baseTime+58, 0),
			expectedResult:    time.Unix(baseTime+64, 0),
			description: "Persisted has second=3 (next minute), " +
				"current has second=58. Persisted is later " +
				"overall. Verifies .Unix() not .Second().",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := calculateNodeAnnouncementTimestamp(
				tc.srcNodeLastUpdate,
				tc.nodeLastUpdate,
			)

			// Verify we got the expected result.
			require.Equal(
				t, tc.expectedResult, result,
				"Unexpected result: %s", tc.description,
			)

			// Verify result is strictly greater than persisted
			// timestamp. This is an additional check to ensure
			// the result is strictly greater than the persisted
			// timestamp.
			require.Greater(
				t, result.Unix(), tc.srcNodeLastUpdate.Unix(),
				"Result must be strictly greater than "+
					"persisted timestamp: %s",
				tc.description,
			)
		})
	}
}

// TestConnectToPeerAccumulation verifies that repeated ConnectToPeer calls
// with perm=true result in a single worker, not accumulated ConnReqs.
func TestConnectToPeerAccumulation(t *testing.T) {
	t.Parallel()

	s := newTestServer(t)

	pubKey := generateTestPubKey(t)
	addr := &lnwire.NetAddress{
		IdentityKey: pubKey,
		Address:     &net.TCPAddr{IP: net.ParseIP("1.2.3.4"), Port: 9735},
	}

	targetPub := string(pubKey.SerializeCompressed())

	// Call ConnectToPeer 10 times with perm=true. With workers,
	// each call reuses the same worker and sends cmdConnect.
	for i := 0; i < 10; i++ {
		err := s.ConnectToPeer(addr, true, time.Second)
		require.NoError(t, err)
	}

	s.mu.Lock()
	w, ok := s.persistentWorkers[targetPub]
	s.mu.Unlock()

	// Only one worker should exist, and it should be perm.
	require.True(t, ok, "worker should exist")
	require.True(t, w.perm, "worker should be perm")

	// Clean up workers.
	s.mu.Lock()
	s.stopWorker(targetPub)
	s.mu.Unlock()
}

// TestPeerBackoff tests the pure peerBackoff function with table-driven cases
// covering zero backoff, short-lived connections (doubles), stable connections
// (reduces), and capping at max.
func TestPeerBackoff(t *testing.T) {
	t.Parallel()

	const (
		minBackoff         = 1 * time.Second
		maxBackoff         = 1 * time.Hour
		stableConnDuration = 10 * time.Minute
	)

	tests := []struct {
		name           string
		currentBackoff time.Duration
		startTime      time.Time
		assertBackoff  func(t *testing.T, result time.Duration)
	}{
		{
			// Peer never started: backoff should roughly double
			// (with randomization).
			name:           "zero start time doubles backoff",
			currentBackoff: 10 * time.Second,
			startTime:      time.Time{},
			assertBackoff: func(t *testing.T, result time.Duration) {
				// computeNextBackoff doubles with ±5%
				// wiggle, so result should be roughly 20s.
				require.Greater(t, result,
					15*time.Second,
					"backoff too low for failed start")
				require.Less(t, result,
					25*time.Second,
					"backoff too high for failed start")
			},
		},
		{
			// Short-lived connection (< stableConnDuration):
			// backoff should roughly double.
			name:           "short lived connection doubles backoff",
			currentBackoff: 10 * time.Second,
			startTime:      time.Now().Add(-5 * time.Minute),
			assertBackoff: func(t *testing.T, result time.Duration) {
				require.Greater(t, result,
					15*time.Second,
					"backoff too low for short conn")
				require.Less(t, result,
					25*time.Second,
					"backoff too high for short conn")
			},
		},
		{
			// Stable connection (> stableConnDuration) with
			// large backoff: should reduce. reb(30m) ≈ 60m,
			// minus 20m conn = ~40m, which is > minBackoff.
			name:           "stable connection reduces backoff",
			currentBackoff: 30 * time.Minute,
			startTime:      time.Now().Add(-20 * time.Minute),
			assertBackoff: func(t *testing.T, result time.Duration) {
				require.Greater(t, result, minBackoff,
					"should be above min")
				require.Less(t, result, 50*time.Minute,
					"should be reduced from doubled")
			},
		},
		{
			// Stable connection that lasted much longer than
			// backoff: should return minBackoff.
			// reb(1s) ≈ 2s, minus 1h conn → negative → min.
			name:           "long stable connection resets to min",
			currentBackoff: minBackoff,
			startTime:      time.Now().Add(-1 * time.Hour),
			assertBackoff: func(t *testing.T, result time.Duration) {
				require.Equal(t, minBackoff, result)
			},
		},
		{
			// Backoff at max: doubling should cap at max.
			name:           "backoff caps at max",
			currentBackoff: maxBackoff,
			startTime:      time.Time{},
			assertBackoff: func(t *testing.T, result time.Duration) {
				// After capping + wiggle.
				require.LessOrEqual(t, result,
					maxBackoff+maxBackoff/10)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := peerBackoff(
				tc.currentBackoff, tc.startTime, minBackoff,
				maxBackoff, stableConnDuration,
			)

			tc.assertBackoff(t, result)
		})
	}
}

// TestGetOrCreateWorker verifies that getOrCreateWorker creates a new worker on
// first call and returns the existing one on subsequent calls.
func TestGetOrCreateWorker(t *testing.T) {
	t.Parallel()

	s := newTestServer(t)

	pubKey := generateTestPubKey(t)
	pubStr := string(pubKey.SerializeCompressed())

	// First call creates a new worker.
	w := s.getOrCreateWorker(pubStr, false, nil)
	require.NotNil(t, w)

	_, ok := s.persistentWorkers[pubStr]
	require.True(t, ok, "worker should be in map")

	// Second call returns the same worker.
	w2 := s.getOrCreateWorker(pubStr, false, nil)
	require.Equal(t, w, w2, "should return same worker")

	// Upgrade to perm.
	w3 := s.getOrCreateWorker(pubStr, true, nil)
	require.Equal(t, w, w3)
	require.True(t, w.perm, "should be upgraded to perm")

	// Clean up.
	s.stopWorker(pubStr)
}

// TestStopWorker verifies that stopWorker removes the worker from the map and
// sends cmdStop.
func TestStopWorker(t *testing.T) {
	t.Parallel()

	s := newTestServer(t)

	pubKey := generateTestPubKey(t)
	pubStr := string(pubKey.SerializeCompressed())

	// Create a worker.
	s.getOrCreateWorker(pubStr, false, nil)
	require.Contains(t, s.persistentWorkers, pubStr)

	// Stop it.
	s.stopWorker(pubStr)
	require.NotContains(t, s.persistentWorkers, pubStr)
}

// TestStopWorkerDropsCmdOnFullChannel demonstrates that stopWorker silently
// drops cmdStop when the worker's command channel is already full. The worker
// is removed from the map but its goroutine continues running — it becomes
// orphaned and can only be stopped by closing s.quit.
func TestStopWorkerDropsCmdOnFullChannel(t *testing.T) {
	t.Parallel()

	s := newTestServer(t)

	pubKey := generateTestPubKey(t)
	pubStr := string(pubKey.SerializeCompressed())

	// Create a worker with a slow dial so it stays in the dial loop.
	dialStarted := make(chan struct{})
	blockDial := make(chan struct{})
	s.getOrCreateWorker(pubStr, false, nil)
	w := s.persistentWorkers[pubStr]

	// Override the dial function to block until we release it.
	w.cfg.dialContext = func(ctx context.Context,
		addr *lnwire.NetAddress) (net.Conn, error) {

		close(dialStarted)
		<-blockDial
		return nil, errors.New("blocked dial released")
	}

	addr := &lnwire.NetAddress{
		IdentityKey: pubKey,
		Address:     &net.TCPAddr{IP: net.ParseIP("1.2.3.4"), Port: 9735},
	}

	// Send cmdConnect to start the worker dialing. This fills the buffer
	// momentarily but the worker drains it and enters dialLoop.
	w.cmdChan <- connWorkerMsg{
		cmd:   cmdConnect,
		addrs: []*lnwire.NetAddress{addr},
	}

	// Wait for the worker to be inside the dial goroutine.
	select {
	case <-dialStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for dial to start")
	}

	// Now fill the command channel while the worker is blocked in dial.
	// The worker is inside tryAllAddresses waiting on resultCh, so it
	// hasn't drained cmdChan yet. We send a command to fill the buffer.
	w.cmdChan <- connWorkerMsg{cmd: cmdStandDown}

	// Channel is now full (capacity 1). stopWorker should fail to deliver
	// cmdStop.
	s.stopWorker(pubStr)

	// The worker is removed from the map...
	require.NotContains(t, s.persistentWorkers, pubStr)

	// ...but the goroutine is still alive. Release the dial so the worker
	// can process commands again.
	close(blockDial)

	// Give the worker time to process the cmdStandDown (which it received
	// instead of cmdStop) and return to idle.
	time.Sleep(100 * time.Millisecond)

	// The worker goroutine is still running in its idle select — it never
	// received cmdStop. We can prove this by sending it another command.
	// If the goroutine had exited, this send would block forever (no
	// reader).
	workerAlive := make(chan bool, 1)
	go func() {
		select {
		case w.cmdChan <- connWorkerMsg{cmd: cmdStop}:
			workerAlive <- true
		case <-time.After(2 * time.Second):
			workerAlive <- false
		}
	}()

	alive := <-workerAlive
	require.True(t, alive,
		"worker goroutine is still running despite being "+
			"removed from persistentWorkers — orphaned")

	// Clean up: close quit to stop the orphaned worker.
	close(s.quit)
}

// TestSendWorkerCmdNoWorker verifies that sendWorkerCmd returns false when no
// worker exists for the peer.
func TestSendWorkerCmdNoWorker(t *testing.T) {
	t.Parallel()

	s := newTestServer(t)

	ok := s.sendWorkerCmd("nonexistent", connWorkerMsg{
		cmd: cmdStandDown,
	})
	require.False(t, ok)
}
