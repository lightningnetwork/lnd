package sqldb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSqliteConfigMaxConns verifies that SQLite keeps the low default
// connection limit unless the caller overrides it explicitly.
func TestSqliteConfigMaxConns(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		maxConns     int
		expectedConn int
	}{
		{
			name:         "default limit",
			expectedConn: DefaultSqliteMaxConns,
		},
		{
			name:         "explicit limit",
			maxConns:     7,
			expectedConn: 7,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			cfg := &SqliteConfig{
				MaxConnections: testCase.maxConns,
			}

			require.Equal(t, testCase.expectedConn, cfg.MaxConns())
		})
	}
}

// TestSqliteConfigMaxIdleConns verifies that SQLite defaults its idle
// connections to the open connection limit unless the caller overrides it.
func TestSqliteConfigMaxIdleConns(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name             string
		maxConns         int
		maxIdleConns     int
		expectedIdleConn int
	}{
		{
			name:             "default idle limit",
			expectedIdleConn: DefaultSqliteMaxConns,
		},
		{
			name:             "inherits explicit open limit",
			maxConns:         4,
			expectedIdleConn: 4,
		},
		{
			name:             "explicit idle limit",
			maxConns:         4,
			maxIdleConns:     3,
			expectedIdleConn: 3,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			cfg := &SqliteConfig{
				MaxConnections:     testCase.maxConns,
				MaxIdleConnections: testCase.maxIdleConns,
			}

			require.Equal(t, testCase.expectedIdleConn,
				cfg.MaxIdleConns())
		})
	}
}

// TestPostgresConfigTxIsolation asserts that the tx isolation option only
// accepts the two levels we support, and that only the relaxed one asks for
// read-write transactions at repeatable read.
func TestPostgresConfigTxIsolation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		txIsolation TxIsolation
		valid       bool
		rrWrites    bool
	}{
		{
			name:        "unset",
			txIsolation: "",
			valid:       true,
		},
		{
			name:        "serializable",
			txIsolation: TxIsolationSerializable,
			valid:       true,
		},
		{
			name:        "repeatable read",
			txIsolation: TxIsolationRepeatableRead,
			valid:       true,
			rrWrites:    true,
		},
		{
			name:        "garbage",
			txIsolation: "not-an-isolation-level",
		},
		{
			// The Postgres spelling of the level is not the one we
			// accept, since our own options are dash separated.
			name:        "postgres spelling",
			txIsolation: "repeatable read",
		},
		{
			name:        "wrong case",
			txIsolation: "SERIALIZABLE",
		},
		{
			// We deliberately don't expose the weaker levels that
			// Postgres itself supports.
			name:        "read committed",
			txIsolation: "read-committed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := &PostgresConfig{
				Dsn:         "postgres://lnd@localhost/lnd",
				TxIsolation: test.txIsolation,
				QueryConfig: *DefaultPostgresConfig(),
			}

			err := cfg.Validate()
			if !test.valid {
				require.ErrorContains(
					t, err, "invalid tx isolation level",
				)

				return
			}

			require.NoError(t, err)
			require.Equal(
				t, test.rrWrites, cfg.WriteTxRepeatableRead(),
			)
		})
	}
}
