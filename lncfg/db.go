package lncfg

import (
	"context"
	"fmt"

	"github.com/lightningnetwork/lnd/channeldb/kvdb"
)

const (
	dbName      = "channel.db"
	boltBackend = "bolt"
	etcdBackend = "etcd"
)

// DB holds database configuration for LND.
type DB struct {
	Backend string `long:"backend" description:"The selected database backend."`

	Etcd *kvdb.EtcdConfig `group:"etcd" namespace:"etcd" description:"Etcd settings."`

	Bolt *kvdb.BoltConfig `group:"bolt" namespace:"bolt" description:"Bolt settings."`
}

// NewDB creates and returns a new default DB config.
func DefaultDB() *DB {
	return &DB{
		Backend: boltBackend,
		Bolt: &kvdb.BoltConfig{
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
func (db *DB) GetBackend(ctx context.Context, dbPath string,
	networkName string) (kvdb.Backend, error) {

	if db.Backend == etcdBackend {
		// Prefix will separate key/values in the db.
		return kvdb.GetEtcdBackend(ctx, networkName, db.Etcd)
	}

	return kvdb.GetBoltBackend(dbPath, dbName, db.Bolt.NoFreeListSync)
}

// Compile-time constraint to ensure Workers implements the Validator interface.
var _ Validator = (*DB)(nil)
