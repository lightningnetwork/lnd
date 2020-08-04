package lncfg

import (
	"context"
	"fmt"

	"github.com/lightningnetwork/lnd/channeldb/kvdb"
)

const (
	dbName      = "channel.db"
	BoltBackend = "bolt"
	EtcdBackend = "etcd"
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
		Backend: BoltBackend,
		Bolt:    &kvdb.BoltConfig{},
	}
}

// Validate validates the DB config.
func (db *DB) Validate() error {
	switch db.Backend {
	case BoltBackend:

	case EtcdBackend:
		if db.Etcd.Host == "" {
			return fmt.Errorf("etcd host must be set")
		}

	default:
		return fmt.Errorf("unknown backend, must be either \"%v\" or \"%v\"",
			BoltBackend, EtcdBackend)
	}

	return nil
}

// GetBackend returns a kvdb.Backend as set in the DB config.
func (db *DB) GetBackend(ctx context.Context, dbPath string,
	networkName string) (kvdb.Backend, error) {

	if db.Backend == EtcdBackend {
		// Prefix will separate key/values in the db.
		return kvdb.GetEtcdBackend(ctx, networkName, db.Etcd)
	}

	// The implementation by walletdb accepts "noFreelistSync" as the
	// second parameter, so we negate here.
	return kvdb.GetBoltBackend(dbPath, dbName, !db.Bolt.SyncFreelist)
}

// Compile-time constraint to ensure Workers implements the Validator interface.
var _ Validator = (*DB)(nil)
