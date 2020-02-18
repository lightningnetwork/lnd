package etcd

import (
	"time"

	"github.com/coreos/etcd/clientv3"
)

const (
	// etcdConnectionTimeout is the timeout until successful connection to the
	// etcd instance.
	etcdConnectionTimeout = 10 * time.Second
)

// db holds a reference to the etcd client connection.
type db struct {
	cli *clientv3.Client
}

// BackendConfig holds and etcd backend config and connection parameters.
type BackendConfig struct {
	// Host holds the peer url of the etcd instance.
	Host string

	// User is the username for the etcd peer.
	User string

	// Pass is the password for the etcd peer.
	Pass string
}

// newEtcdBackend returns a db object initialized with the passed backend
// config. If etcd connection cannot be estabished, then returns error.
func newEtcdBackend(config BackendConfig) (*db, error) {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{config.Host},
		DialTimeout: etcdConnectionTimeout,
		Username:    config.User,
		Password:    config.Pass,
	})
	if err != nil {
		return nil, err
	}

	backend := &db{
		cli: cli,
	}

	return backend, nil
}

// Close closes the db, but closing the underlying etcd client connection.
func (db *db) Close() error {
	return db.cli.Close()
}
