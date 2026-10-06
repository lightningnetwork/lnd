package lnd

import (
	"net"
	"testing"
	"time"

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
