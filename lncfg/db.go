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

// DatabaseBackends is a two-tuple that holds the set of active database
// backends for the daemon. The two backends we expose are the local database
// backend, and the remote backend. The LocalDB attribute will always be
// populated. However, the remote DB will only be set if a replicated database
// is active.
type DatabaseBackends struct {
	// LocalDB points to the local non-replicated backend.
	LocalDB kvdb.Backend

	// RemoteDB points to a possibly networked replicated backend. If no
	// replicated backend is active, then this pointer will be nil.
	RemoteDB kvdb.Backend
}

// GetBackends returns a set of kvdb.Backends as set in the DB config.  The
// local database will ALWAYS be non-nil, while the remote database will only
// be populated if etcd is specified.
func (db *DB) GetBackends(ctx context.Context, dbPath string,
	networkName string) (*DatabaseBackends, error) {

	var (
		localDB, remoteDB kvdb.Backend
		err               error
	)

	if db.Backend == EtcdBackend {
		// Prefix will separate key/values in the db.
		remoteDB, err = kvdb.GetEtcdBackend(ctx, networkName, db.Etcd)
		if err != nil {
			return nil, err
		}
	}

	localDB, err = kvdb.GetBoltBackend(
		dbPath, dbName, !db.Bolt.SyncFreelist,
	)
	if err != nil {
		return nil, err
	}

	return &DatabaseBackends{
		LocalDB:  localDB,
		RemoteDB: remoteDB,
	}, nil
}

// Compile-time constraint to ensure Workers implements the Validator interface.
var _ Validator = (*DB)(nil)
