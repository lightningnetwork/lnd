// +build kvdb_postgres

package kvdb

import (
	"github.com/lightningnetwork/lnd/kvdb/postgres"
)

// GetTestBackend opens (or creates if doesn't exist) a postgres backed database
// (for testing), and returns a kvdb.Backend and a cleanup func.
func GetTestBackend(path, name string) (Backend, func(), error) {
	f, err := postgres.NewFixture()
	if err != nil {
		return nil, func() {}, err
	}
	return f.Db, func() {
		_ = f.Db.Close()
	}, nil
}

func SetupTestBackend() error {
	return postgres.StartEmbeddedPostgres()
}

func TearDownTestBackend() {
	postgres.StopEmbeddedPostgres()
}
