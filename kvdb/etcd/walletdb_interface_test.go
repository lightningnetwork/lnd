//go:build kvdb_etcd
// +build kvdb_etcd

package etcd

import (
	"context"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb/walletdbtest"
)

// TestWalletDBInterface performs the WalletDB interface test suite for the
// etcd database driver.
func TestWalletDBInterface(t *testing.T) {
	f := NewEtcdTestFixture(t)
	cfg := f.BackendConfig()
	walletdbtest.TestInterface(t, dbType, context.TODO(), &cfg)
}
