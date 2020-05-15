// +build kvdb_etcd

package kvdb

import (
	"github.com/lightningnetwork/lnd/channeldb/kvdb/etcd"
)

// TestBackend is conditionally set to etcd when the kvdb_etcd build tag is
// defined, allowing testing our database code with etcd backend.
const TestBackend = EtcdBackendName

// GetEtcdBackend returns an etcd backend configured according to the
// passed etcdConfig.
func GetEtcdBackend(etcdConfig *EtcdConfig) (Backend, error) {
	// Config translation is needed here in order to keep the
	// etcd package fully independent from the rest of the source tree.
	backendConfig := etcd.BackendConfig{
		Host:               etcdConfig.Host,
		User:               etcdConfig.User,
		Pass:               etcdConfig.Pass,
		CertFile:           etcdConfig.CertFile,
		KeyFile:            etcdConfig.KeyFile,
		InsecureSkipVerify: etcdConfig.InsecureSkipVerify,
		CollectCommitStats: etcdConfig.CollectStats,
	}

	return Open(EtcdBackendName, backendConfig)
}

// GetEtcdTestBackend creates an embedded etcd backend for testing
// storig the database at the passed path.
func GetEtcdTestBackend(path, name string) (Backend, func(), error) {
	empty := func() {}

	config, cleanup, err := etcd.NewEmbeddedEtcdInstance(path)
	if err != nil {
		return nil, empty, err
	}

	backend, err := Open(EtcdBackendName, *config)
	if err != nil {
		cleanup()
		return nil, empty, err
	}

	return backend, cleanup, nil
}
