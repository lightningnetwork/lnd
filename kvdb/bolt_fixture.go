package kvdb

import (
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/require"
)

type boltFixture struct {
	t       *testing.T
	tempDir string
}

func NewBoltFixture(t *testing.T) *boltFixture {
	return &boltFixture{
		t:       t,
		tempDir: t.TempDir(),
	}
}

func (b *boltFixture) NewBackend() walletdb.DB {
	dbPath := filepath.Join(b.tempDir)

	db, err := GetBoltBackend(&BoltBackendConfig{
		DBPath:         dbPath,
		DBFileName:     "test.db",
		NoFreelistSync: true,
		DBTimeout:      DefaultDBTimeout,
		ReadOnly:       false,
	})
	require.NoError(b.t, err)

	return db
}
