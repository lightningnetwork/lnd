package lncfg

import (
	"fmt"

	"github.com/lightningnetwork/lnd/channeldb/kvdb"
	"github.com/lightningnetwork/lnd/channeldb/kvdb/etcd"
)

const (
	dbName      = "channel.db"
	boltBackend = "bolt"
	etcdBackend = "etcd"
)

// BoltDB holds bolt configuration.
type BoltDB struct {
	NoFreeListSync bool `long:"nofreelistsync" description:"If true, prevents the database from syncing its freelist to disk"`
}

// EtcdDB hold etcd configuration.
type EtcdDB struct {
	Host string `long:"host" description:"Etcd database host."`

	User string `long:"user" description:"Etcd database user."`

	Pass string `long:"pass" description:"Password for the database user."`

	CertFile string `long:"cert_file" description:"Path to the TLS certificate for etcd RPC."`

	KeyFile string `long:"key_file" description:"Path to the TLS private key for etcd RPC."`

	InsecureSkipVerify bool `long:"insecure_skip_verify" description:"Whether we intend to skip TLS verification"`

	CollectStats bool `long:"collect_stats" description:"Wheter to collect etcd commit stats."`
}

// DB holds database configuration for LND.
type DB struct {
	Backend string `long:"backend" description:"The selected database backend."`

	Etcd *EtcdDB `group:"etcd" namespace:"etcd" description:"Etcd settings."`

	Bolt *BoltDB `group:"bolt" namespace:"bolt" description:"Bolt settings."`
}

// NewDB creates and returns a new default DB config.
func DefaultDB() *DB {
	return &DB{
		Backend: boltBackend,
		Bolt: &BoltDB{
			NoFreeListSync: true,
		},
	}
}

// Validate validates the DB config.
func (db *DB) Validate() error {
	switch db.Backend {
	case boltBackend:

	case etcdBackend:
		if db.Etcd.Host == "" {
			return fmt.Errorf("etcd host must be set")
		}

	default:
		return fmt.Errorf("unknown backend, must be either \"%v\" or \"%v\"",
			boltBackend, etcdBackend)
	}

	return nil
}

// GetBackend returns a kvdb.Backend as set in the DB config.
func (db *DB) GetBackend(path string) (kvdb.Backend, error) {
	if db.Backend == etcdBackend {
		backendConfig := etcd.BackendConfig{
			Host:               db.Etcd.Host,
			User:               db.Etcd.User,
			Pass:               db.Etcd.Pass,
			CertFile:           db.Etcd.CertFile,
			KeyFile:            db.Etcd.KeyFile,
			InsecureSkipVerify: db.Etcd.InsecureSkipVerify,
			CollectCommitStats: db.Etcd.CollectStats,
		}
		return kvdb.Open(kvdb.EtcdBackendName, backendConfig)
	}

	return kvdb.GetBoltBackend(path, dbName, db.Bolt.NoFreeListSync)
}

// Compile-time constraint to ensure Workers implements the Validator interface.
var _ Validator = (*DB)(nil)
