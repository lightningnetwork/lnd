package lncfg_test

import (
	"testing"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lncfg"
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
}

// TestDBValidateEtcdDisabled checks storage rejection while preserving etcd
// connection settings for leader election with supported database backends.
func TestDBValidateEtcdDisabled(t *testing.T) {
	// Arrange: Cover both former etcd modes and supported storage that
	// still needs independent etcd connection settings for elections.
	testCases := []struct {
		name     string
		backend  string
		host     string
		embedded bool
		wantErr  bool
	}{
		{
			name:    "RemoteEtcdRejected",
			backend: lncfg.EtcdBackend,
			host:    "localhost:2379",
			wantErr: true,
		},
		{
			name:     "EmbeddedEtcdRejected",
			backend:  lncfg.EtcdBackend,
			embedded: true,
			wantErr:  true,
		},
		{
			name:    "BoltWithEtcdSettingsAccepted",
			backend: lncfg.BoltBackend,
			host:    "localhost:2379",
		},
		{
			name:    "PostgresWithEtcdSettingsAccepted",
			backend: lncfg.PostgresBackend,
			host:    "localhost:2379",
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			// Arrange: Use valid defaults and a PostgreSQL DSN
			// so storage selection alone decides rejection.
			cfg := lncfg.DefaultDB()
			cfg.Backend = test.backend
			cfg.Etcd.Host = test.host
			cfg.Etcd.Embedded = test.embedded
			cfg.Postgres.Dsn = "postgres://localhost/lnd"

			// Act: Validate before any backend can connect
			// to a server or create database files.
			err := cfg.Validate()

			// Assert: Reject both etcd storage modes with a clear
			// error; independent election settings stay valid.
			if test.wantErr {
				require.ErrorContains(
					t, err,
					"etcd database backend is disabled",
				)

				return
			}
			require.NoError(t, err)
		})
	}
}
