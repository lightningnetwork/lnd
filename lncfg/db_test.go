package lncfg_test

import (
	"testing"

	"github.com/jessevdk/go-flags"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

// TestDBDefaultConfig tests that the default DB config is created as expected.
func TestDBDefaultConfig(t *testing.T) {
	defaultConfig := lncfg.DefaultDB()

	require.Equal(t, lncfg.BoltBackend, defaultConfig.Backend)
	require.Equal(
		t, kvdb.DefaultBoltAutoCompactMinAge,
		defaultConfig.Bolt.AutoCompactMinAge,
	)
	require.Equal(t, kvdb.DefaultDBTimeout, defaultConfig.Bolt.DBTimeout)
	// Implicitly, the following fields are default to false.
	require.False(t, defaultConfig.Bolt.AutoCompact)
	require.True(t, defaultConfig.Bolt.NoFreelistSync)

	// Read-write transactions must stay at SERIALIZABLE unless the user
	// explicitly opts into the relaxed level.
	require.Equal(
		t, sqldb.TxIsolationSerializable,
		defaultConfig.Postgres.TxIsolation,
	)
	require.False(t, defaultConfig.Postgres.WriteTxRepeatableRead())
}

// TestDBPostgresTxIsolation tests that the db.postgres.tx-isolation option is
// parsed by the flag parser, is rejected when it holds a value we don't
// support, and reaches the kvdb Postgres config.
func TestDBPostgresTxIsolation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		arg        string
		parses     bool
		expected   sqldb.TxIsolation
		expectedRR bool
	}{
		{
			name:     "no value given",
			parses:   true,
			expected: sqldb.TxIsolationSerializable,
		},
		{
			name:     "serializable",
			arg:      "--db.postgres.tx-isolation=serializable",
			parses:   true,
			expected: sqldb.TxIsolationSerializable,
		},
		{
			name:       "repeatable read",
			arg:        "--db.postgres.tx-isolation=repeatable-read",
			parses:     true,
			expected:   sqldb.TxIsolationRepeatableRead,
			expectedRR: true,
		},
		{
			name: "garbage",
			arg:  "--db.postgres.tx-isolation=snapshot",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := struct {
				DB *lncfg.DB `group:"db" namespace:"db"`
			}{
				DB: lncfg.DefaultDB(),
			}

			var args []string
			if test.arg != "" {
				args = append(args, test.arg)
			}

			parser := flags.NewParser(&cfg, flags.None)
			_, err := parser.ParseArgs(args)
			if !test.parses {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			require.Equal(
				t, test.expected, cfg.DB.Postgres.TxIsolation,
			)

			// The value must also survive the trip into the kvdb
			// flavored Postgres config, which is what the kvdb SQL
			// backends are handed.
			kvCfg := lncfg.GetPostgresConfigKVDB(cfg.DB.Postgres)
			require.Equal(
				t, test.expectedRR,
				kvCfg.WriteTxRepeatableRead,
			)
		})
	}
}
