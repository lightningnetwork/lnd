// +build kvdb_etcd

package etcd

import (
	"testing"

	"github.com/btcsuite/btcwallet/walletdb/walletdbtest"
)

// TestWalletDBInterface performs the WalletDB interface test suite for the
// etcd database driver.
func TestWalletDBInterface(t *testing.T) {
	f := NewEtcdTestFixture(t)
	defer f.Cleanup()
	walletdbtest.TestInterface(t, dbType, f.BackendConfig())
}
