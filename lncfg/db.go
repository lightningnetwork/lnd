package lncfg

import (
	"context"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/kvdb/etcd"
)

const (
	dbName                     = "channel.db"
	BoltBackend                = "bolt"
	EtcdBackend                = "etcd"
	DefaultBatchCommitInterval = 500 * time.Millisecond
)

// DB holds database configuration for LND.
type DB struct {
	Backend string `long:"backend" description:"The selected database backend."`

	BatchCommitInterval time.Duration `long:"batch-commit-interval" description:"The maximum duration the channel graph batch schedulers will wait before attempting to commit a batch of pending updates. This can be tradeoff database contenion for commit latency."`

	Etcd *etcd.Config `group:"etcd" namespace:"etcd" description:"Etcd settings."`

	Bolt *kvdb.BoltConfig `group:"bolt" namespace:"bolt" description:"Bolt settings."`
}

// NewDB creates and returns a new default DB config.
func DefaultDB() *DB {
	return &DB{
		Backend:             BoltBackend,
		BatchCommitInterval: DefaultBatchCommitInterval,
		Bolt: &kvdb.BoltConfig{
			AutoCompactMinAge: kvdb.DefaultBoltAutoCompactMinAge,
			DBTimeout:         kvdb.DefaultDBTimeout,
		},
	}
}

// Validate validates the DB config.
func (db *DB) Validate() error {
	switch db.Backend {
	case BoltBackend:

	case EtcdBackend:
		if !db.Etcd.Embedded && db.Etcd.Host == "" {
			return fmt.Errorf("etcd host must be set")
		}

	default:
		return fmt.Errorf("unknown backend, must be either \"%v\" or \"%v\"",
			BoltBackend, EtcdBackend)
	}

	return nil
}

// Init should be called upon start to pre-initialize database access dependent
// on configuration.
func (db *DB) Init(ctx context.Context, dbPath string) error {
	// Start embedded etcd server if requested.
	if db.Backend == EtcdBackend && db.Etcd.Embedded {
		cfg, _, err := kvdb.StartEtcdTestBackend(
			dbPath, db.Etcd.EmbeddedClientPort,
			db.Etcd.EmbeddedPeerPort,
		)
		if err != nil {
			return err
		}

		// Override the original config with the config for
		// the embedded instance.
		db.Etcd = cfg
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
func (db *DB) GetBackends(ctx context.Context, dbPath string) (
	*DatabaseBackends, error) {

	var (
		localDB, remoteDB kvdb.Backend
		err               error
	)

	if db.Backend == EtcdBackend {
		remoteDB, err = kvdb.Open(
			kvdb.EtcdBackendName, ctx, db.Etcd,
		)
		if err != nil {
			return nil, err
		}
	}

	localDB, err = kvdb.GetBoltBackend(&kvdb.BoltBackendConfig{
		DBPath:            dbPath,
		DBFileName:        dbName,
		DBTimeout:         db.Bolt.DBTimeout,
		NoFreelistSync:    !db.Bolt.SyncFreelist,
		AutoCompact:       db.Bolt.AutoCompact,
		AutoCompactMinAge: db.Bolt.AutoCompactMinAge,
	})
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
