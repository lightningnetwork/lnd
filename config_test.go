package lnd

import (
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/htlcswitch"
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

// TestValidateMaxOutgoingCltvExpiry asserts that max-cltv-expiry accepts
// values within its supported bounds and rejects values outside them.
func TestValidateMaxOutgoingCltvExpiry(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()

	require.NoError(
		t, validateMaxOutgoingCltvExpiry(
			htlcswitch.DefaultMaxOutgoingCltvExpiry,
			cfg.Bitcoin.TimeLockDelta,
		),
	)
	require.NoError(t, validateMaxOutgoingCltvExpiry(
		MaxTimeLockDelta, MaxTimeLockDelta,
	))

	err := validateMaxOutgoingCltvExpiry(
		cfg.Bitcoin.TimeLockDelta-1,
		cfg.Bitcoin.TimeLockDelta,
	)
	require.ErrorContains(t, err, "max-cltv-expiry must be at least")

	err = validateMaxOutgoingCltvExpiry(
		MaxTimeLockDelta+1, cfg.Bitcoin.TimeLockDelta,
	)
	require.ErrorContains(t, err, "max-cltv-expiry must be at most")
}

// TestValidateChannelPolicyTimeLockDelta asserts that advertised channel
// policy CLTV deltas stay within the node's supported forwarding bounds.
func TestValidateChannelPolicyTimeLockDelta(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()

	require.NoError(t, validateChannelPolicyTimeLockDelta(
		cfg.Bitcoin.TimeLockDelta, cfg.MaxOutgoingCltvExpiry,
	))
	require.NoError(t, validateChannelPolicyTimeLockDelta(
		cfg.MaxOutgoingCltvExpiry, cfg.MaxOutgoingCltvExpiry,
	))

	err := validateChannelPolicyTimeLockDelta(
		minTimeLockDelta-1, cfg.MaxOutgoingCltvExpiry,
	)
	require.ErrorContains(t, err, "time lock delta of")
	require.ErrorContains(t, err, "is too small")

	err = validateChannelPolicyTimeLockDelta(
		MaxTimeLockDelta+1, MaxTimeLockDelta,
	)
	require.ErrorContains(t, err, "is too big")

	err = validateChannelPolicyTimeLockDelta(
		cfg.MaxOutgoingCltvExpiry+1, cfg.MaxOutgoingCltvExpiry,
	)
	require.ErrorContains(t, err, "exceeds max-cltv-expiry")
}
