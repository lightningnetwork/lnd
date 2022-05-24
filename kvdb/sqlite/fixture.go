//go:build kvdb_sqlite
// +build kvdb_sqlite

package sqlite

import "github.com/btcsuite/btcwallet/walletdb"

func NewFixture() (*fixture, error) {
	return nil, nil
}

type fixture struct {
	Db walletdb.DB
}

func (b *fixture) DB() walletdb.DB {
	return b.Db
}

// Dump returns the raw contents of the database.
func (b *fixture) Dump() (map[string]interface{}, error) {
	return nil, nil
}
