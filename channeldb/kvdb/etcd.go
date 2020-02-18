package kvdb

import (
	_ "github.com/lightningnetwork/lnd/channeldb/kvdb/etcd" // Import to register backend.
)

// EtcdBackendName is the name of the backend that should be passed into
// kvdb.Create to initialize a new instance of kvdb.Backend backed by a live
// instance of etcd.
const EtcdBackendName = "etcd"
