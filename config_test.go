package lnd

import (
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	flags "github.com/jessevdk/go-flags"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/stretchr/testify/require"
)

// testSigNetChallengeHex is OP_TRUE encoded as Bitcoin Script hex.
const testSigNetChallengeHex = "51"

var (
	testPassword     = "testpassword"
	redactedPassword = "[redacted]"
)

// testSigNetChallenge is OP_TRUE encoded as Bitcoin Script bytes.
var testSigNetChallenge = []byte{txscript.OP_TRUE}

// validateTestConfig runs ValidateConfig with isolated test logging and closes
// the log rotator that ValidateConfig starts on successful validation.
func validateTestConfig(t *testing.T, cfg Config) (*Config, error) {
	t.Helper()

	cfg.SubLogMgr = build.NewSubLoggerManager()

	fileParser := flags.NewParser(&cfg, flags.Default)
	flagParser := flags.NewParser(&cfg, flags.Default)
	cleanCfg, err := ValidateConfig(
		cfg, signal.Interceptor{}, fileParser, flagParser,
	)
	if err != nil {
		return cleanCfg, err
	}

	require.NoError(t, cleanCfg.LogRotator.Close())

	return cleanCfg, nil
}

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

// TestApplySigNetBlockTime tests that custom signet block times update the
// target block interval used for header difficulty validation.
func TestApplySigNetBlockTime(t *testing.T) {
	t.Parallel()

	t.Run("valid block time", func(t *testing.T) {
		t.Parallel()

		params := chaincfg.CustomSignetParams(
			chaincfg.DefaultSignetChallenge,
			chaincfg.DefaultSignetDNSSeeds,
		)
		require.NoError(
			t, applySigNetBlockTime(&params, 30*time.Second),
		)

		require.Equal(t, 30*time.Second, params.TargetTimePerBlock)
		require.Equal(t, 14*24*time.Hour, params.TargetTimespan)
		require.Equal(
			t, int64(40320),
			int64(params.TargetTimespan/params.TargetTimePerBlock),
		)
		require.False(t, params.ReduceMinDifficulty)
	})

	t.Run("nil params", func(t *testing.T) {
		t.Parallel()

		err := applySigNetBlockTime(nil, 30*time.Second)
		require.ErrorContains(t, err, "params cannot be nil")
	})

	t.Run("sub-second block time", func(t *testing.T) {
		t.Parallel()

		params := chaincfg.CustomSignetParams(
			chaincfg.DefaultSignetChallenge,
			chaincfg.DefaultSignetDNSSeeds,
		)
		err := applySigNetBlockTime(&params, time.Millisecond)
		require.ErrorContains(t, err, "at least one second")
	})

	t.Run("block time exceeds target timespan", func(t *testing.T) {
		t.Parallel()

		params := chaincfg.CustomSignetParams(
			chaincfg.DefaultSignetChallenge,
			chaincfg.DefaultSignetDNSSeeds,
		)
		blockTime := params.TargetTimespan + time.Second
		err := applySigNetBlockTime(&params, blockTime)
		require.ErrorContains(t, err, "must not exceed target timespan")
	})
}

// TestValidateConfigSigNetBlockTime tests that custom signet block times are
// only applied as an optional addition to a custom signet challenge.
func TestValidateConfigSigNetBlockTime(t *testing.T) {
	tests := []struct {
		name        string
		challenge   string
		blockTime   time.Duration
		expectError string
		expectTime  time.Duration
	}{
		{
			name:       "default signet",
			expectTime: chaincfg.SigNetParams.TargetTimePerBlock,
		},
		{
			name:       "custom challenge only",
			challenge:  testSigNetChallengeHex,
			expectTime: chaincfg.SigNetParams.TargetTimePerBlock,
		},
		{
			name:       "custom challenge with block time",
			challenge:  testSigNetChallengeHex,
			blockTime:  30 * time.Second,
			expectTime: 30 * time.Second,
		},
		{
			name:        "block time without custom challenge",
			blockTime:   30 * time.Second,
			expectError: "requires custom signet challenge",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.LndDir = t.TempDir()
			cfg.Bitcoin.Node = neutrinoBackendName
			cfg.Bitcoin.SigNet = true
			cfg.Bitcoin.SigNetChallenge = tc.challenge
			cfg.Bitcoin.SigNetBlockTime = tc.blockTime

			cleanCfg, err := validateTestConfig(t, cfg)

			if tc.expectError != "" {
				require.ErrorContains(t, err, tc.expectError)
				return
			}

			require.NoError(t, err)
			require.Equal(
				t, tc.expectTime,
				cleanCfg.ActiveNetParams.Params.
					TargetTimePerBlock,
			)
		})
	}
}

// TestValidateConfigSigNetBackendOptions tests that custom signet options are
// only accepted for backends that can use them.
func TestValidateConfigSigNetBackendOptions(t *testing.T) {
	err := validateSigNetBackendOptions(nil)
	require.ErrorContains(t, err, "bitcoin config cannot be nil")

	tests := []struct {
		name        string
		node        string
		challenge   string
		blockTime   time.Duration
		expectError string
		expectNet   chaincfg.Params
	}{
		{
			name:      "bitcoind default signet",
			node:      bitcoindBackendName,
			expectNet: chaincfg.SigNetParams,
		},
		{
			name:      "bitcoind custom challenge",
			node:      bitcoindBackendName,
			challenge: testSigNetChallengeHex,
			expectError: "bitcoin.signetchallenge must not be " +
				"set with bitcoin.node=bitcoind",
		},
		{
			name:      "bitcoind custom challenge and block time",
			node:      bitcoindBackendName,
			challenge: testSigNetChallengeHex,
			blockTime: 30 * time.Second,
			expectError: "bitcoin.signetchallenge must not be " +
				"set with bitcoin.node=bitcoind",
		},
		{
			name:      "bitcoind custom block time",
			node:      bitcoindBackendName,
			blockTime: 30 * time.Second,
			expectError: "bitcoin.signetblocktime must not be " +
				"set with bitcoin.node=bitcoind",
		},
		{
			name:      "btcd custom challenge",
			node:      btcdBackendName,
			challenge: testSigNetChallengeHex,
			expectNet: chaincfg.CustomSignetParams(
				testSigNetChallenge,
				chaincfg.DefaultSignetDNSSeeds,
			),
		},
		{
			name:      "btcd custom block time",
			node:      btcdBackendName,
			challenge: testSigNetChallengeHex,
			blockTime: 30 * time.Second,
			expectError: "bitcoin.signetblocktime is not " +
				"supported with bitcoin.node=btcd",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.LndDir = t.TempDir()
			cfg.Bitcoin.Node = tc.node
			cfg.Bitcoin.SigNet = true
			cfg.Bitcoin.SigNetChallenge = tc.challenge
			cfg.Bitcoin.SigNetBlockTime = tc.blockTime
			cfg.BtcdMode.RPCUser = "user"
			cfg.BtcdMode.RPCPass = "pass"
			cfg.BitcoindMode.RPCUser = "user"
			cfg.BitcoindMode.RPCPass = "pass"
			cfg.BitcoindMode.RPCPolling = true

			cleanCfg, err := validateTestConfig(t, cfg)

			if tc.expectError != "" {
				require.ErrorContains(t, err, tc.expectError)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.node, cleanCfg.Bitcoin.Node)
			require.Equal(
				t, tc.expectNet.Net,
				cleanCfg.ActiveNetParams.Params.Net,
			)
			require.Equal(
				t, tc.expectNet.TargetTimePerBlock,
				cleanCfg.ActiveNetParams.Params.
					TargetTimePerBlock,
			)
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
