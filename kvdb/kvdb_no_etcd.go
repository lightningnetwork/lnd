//go:build !kvdb_etcd
// +build !kvdb_etcd

package kvdb

import (
	"fmt"

	"github.com/lightningnetwork/lnd/kvdb/etcd"
)

// EtcdBackend is conditionally set to false when the kvdb_etcd build tag is not
// defined. This will allow testing of other database backends.
const EtcdBackend = false

var errEtcdNotAvailable = fmt.Errorf("etcd backend not available")

// StartEtcdTestBackend  is a stub returning nil, and errEtcdNotAvailable error.
func StartEtcdTestBackend(path string, clientPort, peerPort uint16,
	logFile string) (*etcd.Config, func(), error) {

	return nil, func() {}, errEtcdNotAvailable
}
