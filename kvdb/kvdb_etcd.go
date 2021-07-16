// +build kvdb_etcd

package kvdb

import (
	"context"

	"github.com/lightningnetwork/lnd/kvdb/etcd"
)

// GetTestBackend opens (or creates if doesn't exist) an etcd backed database
// (for testing), and returns a kvdb.Backend and a cleanup func. The passed path
// is used to hold all db files, while the name is only used for bbolt.
func GetTestBackend(path, name string) (Backend, func(), error) {
	etcdConfig, cancel, err := etcd.NewEmbeddedEtcdInstance(
		path, 0, 0,
	)
	if err != nil {
		return nil, nil, err
	}
	backend, err := Open(
		EtcdBackendName, context.TODO(), etcdConfig,
	)
	return backend, cancel, err
}
