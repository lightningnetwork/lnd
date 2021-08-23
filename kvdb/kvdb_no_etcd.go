//go:build !kvdb_etcd
// +build !kvdb_etcd

package kvdb

import (
	"fmt"

	"github.com/lightningnetwork/lnd/kvdb/etcd"
)

// TestBackend is conditionally set to bdb when the kvdb_etcd build tag is
// not defined, allowing testing our database code with bolt backend.
const TestBackend = BoltBackendName

var errEtcdNotAvailable = fmt.Errorf("etcd backend not available")

// StartEtcdTestBackend  is a stub returning nil, and errEtcdNotAvailable error.
func StartEtcdTestBackend(path string, clientPort, peerPort uint16,
	logFile string) (*etcd.Config, func(), error) {

	return nil, func() {}, errEtcdNotAvailable
}
