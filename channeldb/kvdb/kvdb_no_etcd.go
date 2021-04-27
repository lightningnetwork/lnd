// +build !kvdb_etcd

package kvdb

import (
	"context"
	"fmt"
)

// TestBackend is conditionally set to bdb when the kvdb_etcd build tag is
// not defined, allowing testing our database code with bolt backend.
const TestBackend = BoltBackendName

var errEtcdNotAvailable = fmt.Errorf("etcd backend not available")

// GetEtcdBackend is a stub returning nil and errEtcdNotAvailable error.
func GetEtcdBackend(ctx context.Context, prefix string,
	etcdConfig *EtcdConfig) (Backend, error) {

	return nil, errEtcdNotAvailable
}

// GetTestEtcdBackend  is a stub returning nil, an empty closure and an
// errEtcdNotAvailable error.
func GetEtcdTestBackend(path, name string) (Backend, func(), error) {
	return nil, func() {}, errEtcdNotAvailable
}
