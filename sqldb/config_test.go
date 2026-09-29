package sqldb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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
