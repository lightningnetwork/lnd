// +build kvdb_etcd

package etcd

import (
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/assert"
)

func TestOpenCreateFailure(t *testing.T) {
	t.Parallel()

	db, err := walletdb.Open(dbType)
	assert.Error(t, err)
	assert.Nil(t, db)

	db, err = walletdb.Open(dbType, "wrong")
	assert.Error(t, err)
	assert.Nil(t, db)

	db, err = walletdb.Create(dbType)
	assert.Error(t, err)
	assert.Nil(t, db)

	db, err = walletdb.Create(dbType, "wrong")
	assert.Error(t, err)
	assert.Nil(t, db)
}
