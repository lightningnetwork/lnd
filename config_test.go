package lnd

import (
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/routing"
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
	cfg.Tor.V2 = true

	result, deprecated, err := configToFlatMap(cfg)
	require.NoError(t, err)

	// Check that the deprecated option has been parsed out.
	require.Contains(t, deprecated, "bitcoin.active")
	require.Contains(t, deprecated, "tor.v2")

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
