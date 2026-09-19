//go:build kvdb_postgres

package sqlbase

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/stretchr/testify/require"
)

const (
	// testPgPort is the port that the embedded Postgres instance used by
	// this package listens on. A dedicated port is used here so that this
	// test binary can run alongside the one of the kvdb/postgres package,
	// which brings up its own instance.
	testPgPort = 9877

	// testPgDsnTemplate is the connection string template for the embedded
	// Postgres instance above.
	testPgDsnTemplate = "postgres://postgres:postgres@localhost:%d/" +
		"postgres?sslmode=disable"

	// testPgMaxConnections is the maximum number of connections that the
	// embedded Postgres instance accepts.
	testPgMaxConnections = 20
)

// newPostgresTestBackend spins up an embedded Postgres instance and returns a
// SQL backend that is connected to it.
func newPostgresTestBackend(t *testing.T) *db {
	t.Helper()

	Init(testPgMaxConnections)

	// Keep all of the state of the embedded instance contained in a
	// temporary directory that is removed once the test completes.
	runtimePath := t.TempDir()

	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Port(testPgPort).
			RuntimePath(runtimePath).
			DataPath(filepath.Join(runtimePath, "data")).
			Logger(io.Discard).
			StartParameters(map[string]string{
				"max_connections": fmt.Sprintf(
					"%d", testPgMaxConnections,
				),
			}),
	)
	require.NoError(t, pg.Start())
	t.Cleanup(func() {
		require.NoError(t, pg.Stop())
	})

	backend, err := NewSqlBackend(context.Background(), &Config{
		DriverName:      postgresDriverName,
		Dsn:             fmt.Sprintf(testPgDsnTemplate, testPgPort),
		Timeout:         time.Minute,
		Schema:          "public",
		TableNamePrefix: "test",
		SQLiteCmdReplacements: SQLiteCmdReplacements{
			"BLOB":                "BYTEA",
			"INTEGER PRIMARY KEY": "BIGSERIAL PRIMARY KEY",
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, backend.Close())
	})

	return backend
}

// TestPostgresTxIsolationLevel asserts that the isolation level that Postgres
// itself reports for a transaction matches what we expect: read-only
// transactions run at repeatable read while read-write transactions remain
// serializable unless the opt-in knob moves them to repeatable read as well.
// We also assert the read-only flag that Postgres reports, so that dropping it
// from the tx options would be caught here as well.
func TestPostgresTxIsolationLevel(t *testing.T) {
	backend := newPostgresTestBackend(t)

	tests := []struct {
		name         string
		readOnly     bool
		rrWrites     bool
		expected     string
		expectedFlag string
	}{
		{
			name:         "read-only",
			readOnly:     true,
			expected:     "repeatable read",
			expectedFlag: "on",
		},
		{
			name:         "read-write",
			readOnly:     false,
			expected:     "serializable",
			expectedFlag: "off",
		},
		{
			name:         "read-only, rr writes",
			readOnly:     true,
			rrWrites:     true,
			expected:     "repeatable read",
			expectedFlag: "on",
		},
		{
			name:         "read-write, rr writes",
			readOnly:     false,
			rrWrites:     true,
			expected:     "repeatable read",
			expectedFlag: "off",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// The config isn't consulted anywhere else while a
			// transaction is being opened, and these sub tests are
			// not run in parallel, so it's safe to flip the knob in
			// place rather than to bring up a second backend.
			backend.cfg.WriteTxRepeatableRead = test.rrWrites

			tx, err := newReadWriteTx(backend, test.readOnly)
			require.NoError(t, err)
			defer func() {
				require.NoError(t, tx.Rollback())
			}()

			var level string
			row := tx.tx.QueryRow("SHOW transaction_isolation")
			require.NoError(t, row.Scan(&level))
			require.Equal(t, test.expected, level)

			var readOnly string
			row = tx.tx.QueryRow("SHOW transaction_read_only")
			require.NoError(t, row.Scan(&readOnly))
			require.Equal(t, test.expectedFlag, readOnly)
		})
	}
}

// TestPostgresReadTxSnapshotStability asserts the property that the relaxed
// isolation level is chosen for, rather than just the knob itself: a read-only
// transaction keeps reading from the snapshot it started with, even after a
// concurrent writer on a different connection has committed over the same key.
func TestPostgresReadTxSnapshotStability(t *testing.T) {
	backend := newPostgresTestBackend(t)

	var (
		bucketKey = []byte("snapshot")
		key       = []byte("key")
		before    = []byte("before")
		after     = []byte("after")
	)

	// Seed the key with its original value.
	err := backend.Update(func(tx walletdb.ReadWriteTx) error {
		bucket, err := tx.CreateTopLevelBucket(bucketKey)
		if err != nil {
			return err
		}

		return bucket.Put(key, before)
	}, func() {})
	require.NoError(t, err)

	readTx, err := newReadWriteTx(backend, true)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, readTx.Rollback())
	}()

	// Take the first read. Note that this is what pins the snapshot, since
	// Postgres only acquires it once the first statement of a repeatable
	// read transaction runs, not at BEGIN.
	readBucket := readTx.ReadBucket(bucketKey)
	require.NotNil(t, readBucket)
	require.Equal(t, before, readBucket.Get(key))

	// Now overwrite the key and commit, using a separate connection from
	// the pool.
	err = backend.Update(func(tx walletdb.ReadWriteTx) error {
		return tx.ReadWriteBucket(bucketKey).Put(key, after)
	}, func() {})
	require.NoError(t, err)

	// The write is visible to anyone starting fresh.
	err = backend.View(func(tx walletdb.ReadTx) error {
		require.Equal(t, after, tx.ReadBucket(bucketKey).Get(key))

		return nil
	}, func() {})
	require.NoError(t, err)

	// The long lived read transaction, however, must still observe the
	// value that its snapshot was taken at.
	require.Equal(t, before, readTx.ReadBucket(bucketKey).Get(key))
}
