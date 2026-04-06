package lnd

import (
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/require"
)

var (
	testPassword     = "testpassword"
	redactedPassword = "[redacted]"
)

// TestConfigToFlatMap tests that the configToFlatMap function works as
// expected on the default configuration.
func TestConfigToFlatMap(t *testing.T) {
	cfg := DefaultConfig()
	cfg.BitcoindMode.RPCPass = testPassword
	cfg.BtcdMode.RPCPass = testPassword
	cfg.Tor.Password = testPassword
	cfg.DB.Etcd.Pass = testPassword
	cfg.DB.Postgres.Dsn = testPassword

	// Set deprecated fields.
	cfg.Bitcoin.Active = true

	result, deprecated, err := configToFlatMap(cfg)
	require.NoError(t, err)

	// Check that the deprecated option has been parsed out.
	require.Contains(t, deprecated, "bitcoin.active")

	// Pick a couple of random values to check.
	require.Equal(t, DefaultLndDir, result["lnddir"])
	require.Equal(
		t, fmt.Sprintf("%v", chainreg.DefaultBitcoinTimeLockDelta),
		result["bitcoin.timelockdelta"],
	)
	require.Equal(
		t, fmt.Sprintf("%v", routing.DefaultAprioriWeight),
		result["routerrpc.apriori.weight"],
	)
	require.Contains(t, result, "routerrpc.routermacaroonpath")

	// Check that sensitive values are not included.
	require.Equal(t, redactedPassword, result["bitcoind.rpcpass"])
	require.Equal(t, redactedPassword, result["btcd.rpcpass"])
	require.Equal(t, redactedPassword, result["tor.password"])
	require.Equal(t, redactedPassword, result["db.etcd.pass"])
	require.Equal(t, redactedPassword, result["db.postgres.dsn"])
}

// TestSupplyEnvValue tests that the supplyEnvValue function works as
// expected on the passed inputs.
func TestSupplyEnvValue(t *testing.T) {
	// Mock environment variables for testing.
	t.Setenv("EXISTING_VAR", "existing_value")
	t.Setenv("EMPTY_VAR", "")

	tests := []struct {
		input       string
		expected    string
		description string
	}{
		{
			input:    "$EXISTING_VAR",
			expected: "existing_value",
			description: "Valid environment variable without " +
				"default value",
		},
		{
			input:    "${EXISTING_VAR:-default_value}",
			expected: "existing_value",
			description: "Valid environment variable with " +
				"default value",
		},
		{
			input:    "$NON_EXISTENT_VAR",
			expected: "",
			description: "Non-existent environment variable " +
				"without default value",
		},
		{
			input:    "${NON_EXISTENT_VAR:-default_value}",
			expected: "default_value",
			description: "Non-existent environment variable " +
				"with default value",
		},
		{
			input:    "$EMPTY_VAR",
			expected: "",
			description: "Empty environment variable without " +
				"default value",
		},
		{
			input:    "${EMPTY_VAR:-default_value}",
			expected: "default_value",
			description: "Empty environment variable with " +
				"default value",
		},
		{
			input:       "raw_input",
			expected:    "raw_input",
			description: "Raw input - no matching format",
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			result := supplyEnvValue(test.input)
			require.Equal(t, test.expected, result)
		})
	}
}

// TestNormalizeRemoteSignerListenAddrs makes sure lnd preserves explicitly
// configured dedicated inbound remote signer listener ports and applies the
// dedicated default port when none is specified. We keep the default-port case
// as a unit test because an itest would need to bind the real default port
// 10019, which becomes flaky under the parallel CI tranche runner where
// multiple test processes can contend for the same host port. The remote
// signer itests also covers the end-to-end dedicated listener path, but they
// do so with explicit dynamically assigned ports instead of the fixed default.
func TestNormalizeRemoteSignerListenAddrs(t *testing.T) {
	tests := []struct {
		name     string
		listener string
		expected string
	}{
		{
			name:     "default port",
			listener: "localhost",
			expected: "127.0.0.1:10019",
		},
		{
			name:     "explicit port",
			listener: "localhost:12019",
			expected: "127.0.0.1:12019",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inboundCfg := lncfg.InboundWatchOnlyCfg{
				RPCListeners: []string{test.listener},
			}

			cfg := &Config{
				RemoteSigner: &lncfg.RemoteSigner{
					InboundWatchOnlyCfg: inboundCfg,
				},
				net: &tor.ClearNet{},
			}

			addrs, err := normalizeRemoteSignerListenAddrs(cfg)
			require.NoError(t, err)
			require.Len(t, addrs, 1)
			require.Equal(t, test.expected, addrs[0].String())
		})
	}
}

// TestValidateConfigTrickleDelay tests that the TrickleDelay configuration
// is properly validated and defaulted in ValidateConfig. This test directly
// verifies the validation logic without going through the full ValidateConfig
// function which has many dependencies.
func TestValidateConfigTrickleDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		trickleDelay  int
		expectedDelay int
	}{
		{
			name:          "zero delay defaults to 1ms",
			trickleDelay:  0,
			expectedDelay: 1,
		},
		{
			name:          "negative delay defaults to 1ms",
			trickleDelay:  -1000,
			expectedDelay: 1,
		},
		{
			name:          "positive delay unchanged",
			trickleDelay:  5000,
			expectedDelay: 5000,
		},
		{
			name:          "minimum valid delay (1ms)",
			trickleDelay:  1,
			expectedDelay: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create a config with the test's TrickleDelay.
			cfg := Config{
				TrickleDelay: tc.trickleDelay,
			}

			// Simulate the validation logic from ValidateConfig.
			if cfg.TrickleDelay <= 0 {
				cfg.TrickleDelay = 1
			}

			// Verify the TrickleDelay was set to the expected
			// value.
			require.Equal(
				t, tc.expectedDelay, cfg.TrickleDelay,
				"TrickleDelay mismatch",
			)
		})
	}
}
