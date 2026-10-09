package lnd

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/require"
)

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

// TestParseAddrRejectsTorV2 ensures that parseAddr rejects v2 .onion hosts at
// the operator-input boundary. This is the path used by lncli connect (via
// rpcserver.ConnectPeer) and the --addpeer config option, mirroring the
// equivalent gate in lncfg.ParseAddressString.
func TestParseAddrRejectsTorV2(t *testing.T) {
	t.Parallel()

	const (
		v2Host = "3g2upl4pq6kufc4m.onion"
		v3Host = "4acth47i6kxnvkewtm6q7ib2s3ufpo5sqbsnzjpb" +
			"i7utijcltosqemad.onion"
	)

	netCfg := &tor.ClearNet{}

	tests := []struct {
		name      string
		address   string
		expectErr bool
	}{
		{
			name:      "v2 without port is rejected",
			address:   v2Host,
			expectErr: true,
		},
		{
			name:      "v2 with port is rejected",
			address:   v2Host + ":9735",
			expectErr: true,
		},
		{
			name:      "v3 without port is accepted",
			address:   v3Host,
			expectErr: false,
		},
		{
			name:      "v3 with port is accepted",
			address:   v3Host + ":9735",
			expectErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addr, err := parseAddr(tc.address, netCfg)
			if tc.expectErr {
				require.Error(t, err)
				require.Contains(
					t, err.Error(), "tor v2 onion",
				)
				require.Nil(t, addr)

				return
			}

			require.NoError(t, err)
			onionAddr, ok := addr.(*tor.OnionAddr)
			require.True(t, ok)
			require.Equal(t, v3Host, onionAddr.OnionService)
		})
	}
}

// TestWithoutV2Onion ensures that Tor v2 onion addresses are dropped from
// reconnect/dial consumption paths (startup persistent reconnect, live
// topology updates, fetchNodeAdvertisedAddrs) while non-onion and v3 onion
// addresses pass through unchanged. Storage and gossip re-broadcast remain
// byte-faithful elsewhere.
func TestWithoutV2Onion(t *testing.T) {
	t.Parallel()

	v2 := &tor.OnionAddr{
		OnionService: "3g2upl4pq6kufc4m.onion",
		Port:         9735,
	}
	v3 := &tor.OnionAddr{
		OnionService: "4acth47i6kxnvkewtm6q7ib2s3ufpo5sqbsnz" +
			"jpbi7utijcltosqemad.onion",
		Port: 9735,
	}
	tcp := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 9735,
	}

	require.True(t, isV2OnionAddr(v2))
	require.False(t, isV2OnionAddr(v3))
	require.False(t, isV2OnionAddr(tcp))

	filtered := withoutV2Onion([]net.Addr{v2, v3, tcp, v2})
	require.Equal(t, []net.Addr{v3, tcp}, filtered)

	// An all-v2 input filters to an empty slice; callers such as
	// fetchNodeAdvertisedAddrs treat this as "no advertised address".
	require.Empty(t, withoutV2Onion([]net.Addr{v2, v2}))
}

// TestExitCodeOnFailure asserts that the failure callback attached to a
// health check records that check's exit code, and that a later generic code
// does not overwrite it.
func TestExitCodeOnFailure(t *testing.T) {
	interceptor, err := signal.Intercept()
	require.NoError(t, err)
	t.Cleanup(func() {
		interceptor.RequestShutdown()
		<-interceptor.ShutdownChannel()
	})

	exitCodeOnFailure(interceptor, signal.ExitCodeChainBackend)()
	interceptor.SetExitCode(signal.ExitCodeCriticalError)

	require.Equal(t, signal.ExitCodeChainBackend, interceptor.ExitCode())
}

// failingPinger is a wallet controller whose remote signer ping always fails.
type failingPinger struct {
	*mock.WalletController
}

// Ping fails so that the remote signer health check gives up.
func (p *failingPinger) Ping(context.Context, time.Duration) error {
	return errors.New("remote signer unreachable")
}

// notLeader is a leader elector that reports it is never the leader.
type notLeader struct{}

func (notLeader) Campaign(context.Context) error         { return nil }
func (notLeader) Resign(context.Context) error           { return nil }
func (notLeader) Leader(context.Context) (string, error) { return "", nil }
func (notLeader) IsLeader(context.Context) (bool, error) { return false, nil }

// TestLivenessMonitorExitCodes drives the server's liveness monitor with one
// failing health check at a time and asserts that each check records its own
// exit code. The tor check is not covered as it needs a live tor control
// connection.
func TestLivenessMonitorExitCodes(t *testing.T) {
	// disabled keeps a check out of the monitor, enabled fails it on the
	// first attempt shortly after the monitor starts.
	disabled := func() *lncfg.CheckConfig {
		return &lncfg.CheckConfig{Attempts: 0}
	}
	enabled := func() *lncfg.CheckConfig {
		return &lncfg.CheckConfig{
			Interval: 10 * time.Millisecond,
			Attempts: 1,
			Timeout:  time.Second,
			Backoff:  0,
		}
	}

	tests := []struct {
		name string

		// setup enables the check under test and makes it fail.
		setup func(t *testing.T, s *server, cfg *Config,
			cc *chainreg.ChainControl)

		leader   bool
		exitCode signal.ExitCode
	}{{
		name: "chain backend",
		setup: func(t *testing.T, s *server, cfg *Config,
			cc *chainreg.ChainControl) {

			cfg.HealthChecks.ChainCheck = enabled()
			cc.HealthCheck = func() error {
				return errors.New("chain backend down")
			}
		},
		exitCode: signal.ExitCodeChainBackend,
	}, {
		name: "disk space",
		setup: func(t *testing.T, s *server, cfg *Config,
			cc *chainreg.ChainControl) {

			cfg.HealthChecks.DiskCheck = &lncfg.DiskCheckConfig{
				RequiredRemaining: 1,
				CheckConfig:       enabled(),
			}
		},
		exitCode: signal.ExitCodeDiskSpace,
	}, {
		name: "tls certificate",
		setup: func(t *testing.T, s *server, cfg *Config,
			cc *chainreg.ChainControl) {

			cfg.HealthChecks.TLSCheck = enabled()
			s.tlsManager = NewTLSManager(&TLSManagerCfg{
				TLSCertPath: filepath.Join(
					cfg.LndDir, "missing.cert",
				),
				TLSKeyPath: filepath.Join(
					cfg.LndDir, "missing.key",
				),
			})
		},
		exitCode: signal.ExitCodeTLSCert,
	}, {
		name: "remote signer",
		setup: func(t *testing.T, s *server, cfg *Config,
			cc *chainreg.ChainControl) {

			cfg.HealthChecks.RemoteSigner = enabled()
			cfg.RemoteSigner.Enable = true
			cc.Wc = &failingPinger{
				WalletController: &mock.WalletController{},
			}
		},
		exitCode: signal.ExitCodeRemoteSigner,
	}, {
		name: "leader status",
		setup: func(t *testing.T, s *server, cfg *Config,
			cc *chainreg.ChainControl) {

			cfg.HealthChecks.LeaderCheck = enabled()
		},
		leader:   true,
		exitCode: signal.ExitCodeLeaderStatus,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			interceptor, err := signal.Intercept()
			require.NoError(t, err)
			t.Cleanup(func() {
				interceptor.RequestShutdown()
				<-interceptor.ShutdownChannel()
			})

			cfg := &Config{
				LndDir:       t.TempDir(),
				Bitcoin:      &lncfg.Chain{Node: "bitcoind"},
				RemoteSigner: &lncfg.RemoteSigner{},
				HealthChecks: &lncfg.HealthCheckConfig{
					ChainCheck: disabled(),
					DiskCheck: &lncfg.DiskCheckConfig{
						CheckConfig: disabled(),
					},
					TLSCheck:      disabled(),
					TorConnection: disabled(),
					RemoteSigner:  disabled(),
					LeaderCheck:   disabled(),
				},
			}
			partial := &chainreg.PartialChainControl{
				HealthCheck: func() error { return nil },
			}
			cc := &chainreg.ChainControl{
				PartialChainControl: partial,
			}
			s := &server{
				cfg:         cfg,
				interceptor: interceptor,
				cc:          cc,
			}

			test.setup(t, s, cfg, cc)

			var elector notLeader
			if test.leader {
				err = s.createLivenessMonitor(
					t.Context(), cfg, cc, elector,
				)
			} else {
				err = s.createLivenessMonitor(
					t.Context(), cfg, cc, nil,
				)
			}
			require.NoError(t, err)

			require.NoError(t, s.livenessMonitor.Start())
			t.Cleanup(func() {
				require.NoError(t, s.livenessMonitor.Stop())
			})

			require.Eventually(t, func() bool {
				return interceptor.ExitCode() == test.exitCode
			}, 5*time.Second, 10*time.Millisecond)
		})
	}
}
