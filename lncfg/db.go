package lncfg

import (
	"context"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/kvdb/etcd"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
)

const (
	channelDBName     = "channel.db"
	macaroonDBName    = "macaroons.db"
	decayedLogDbName  = "sphinxreplay.db"
	towerClientDBName = "wtclient.db"
	towerServerDBName = "watchtower.db"

	BoltBackend                = "bolt"
	EtcdBackend                = "etcd"
	DefaultBatchCommitInterval = 500 * time.Millisecond

	// NSChannelDB is the namespace name that we use for the combined graph
	// and channel state DB.
	NSChannelDB = "channeldb"

	// NSMacaroonDB is the namespace name that we use for the macaroon DB.
	NSMacaroonDB = "macaroondb"

	// NSDecayedLogDB is the namespace name that we use for the sphinx
	// replay a.k.a. decayed log DB.
	NSDecayedLogDB = "decayedlogdb"

	// NSTowerClientDB is the namespace name that we use for the watchtower
	// client DB.
	NSTowerClientDB = "towerclientdb"

	// NSTowerServerDB is the namespace name that we use for the watchtower
	// server DB.
	NSTowerServerDB = "towerserverdb"
)

// DB holds database configuration for LND.
type DB struct {
	Backend string `long:"backend" description:"The selected database backend."`

	BatchCommitInterval time.Duration `long:"batch-commit-interval" description:"The maximum duration the channel graph batch schedulers will wait before attempting to commit a batch of pending updates. This can be tradeoff database contenion for commit latency."`

	Etcd *etcd.Config `group:"etcd" namespace:"etcd" description:"Etcd settings."`

	Bolt *kvdb.BoltConfig `group:"bolt" namespace:"bolt" description:"Bolt settings."`
}

// DefaultDB creates and returns a new default DB config.
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
// backends for the daemon. The two backends we expose are the graph database
// backend, and the channel state backend.
type DatabaseBackends struct {
	// GraphDB points to the database backend that contains the less
	// critical data that is accessed often, such as the channel graph and
	// chain height hints.
	GraphDB kvdb.Backend

	// ChanStateDB points to a possibly networked replicated backend that
	// contains the critical channel state related data.
	ChanStateDB kvdb.Backend

	// HeightHintDB points to a possibly networked replicated backend that
	// contains the chain height hint related data.
	HeightHintDB kvdb.Backend

	// MacaroonDB points to a database backend that stores the macaroon root
	// keys.
	MacaroonDB kvdb.Backend

	// DecayedLogDB points to a database backend that stores the decayed log
	// data.
	DecayedLogDB kvdb.Backend

	// TowerClientDB points to a database backend that stores the watchtower
	// client data. This might be nil if the watchtower client is disabled.
	TowerClientDB kvdb.Backend

	// TowerServerDB points to a database backend that stores the watchtower
	// server data. This might be nil if the watchtower server is disabled.
	TowerServerDB kvdb.Backend

	// WalletDB is an option that instructs the wallet loader where to load
	// the underlying wallet database from.
	WalletDB btcwallet.LoaderOption

	// Remote indicates whether the database backends are remote, possibly
	// replicated instances or local bbolt backed databases.
	Remote bool

	// CloseFuncs is a map of close functions for each of the initialized
	// DB backends keyed by their namespace name.
	CloseFuncs map[string]func() error
}

// GetBackends returns a set of kvdb.Backends as set in the DB config.
func (db *DB) GetBackends(ctx context.Context, chanDBPath,
	walletDBPath, towerServerDBPath string, towerClientEnabled,
	towerServerEnabled bool) (*DatabaseBackends, error) {

	// We keep track of all the kvdb backends we actually open and return a
	// reference to their close function so they can be cleaned up properly
	// on error or shutdown.
	closeFuncs := make(map[string]func() error)

	// If we need to return early because of an error, we invoke any close
	// function that has been initialized so far.
	returnEarly := true
	defer func() {
		if !returnEarly {
			return
		}

		for _, closeFunc := range closeFuncs {
			_ = closeFunc()
		}
	}()

	if db.Backend == EtcdBackend {
		etcdBackend, err := kvdb.Open(
			kvdb.EtcdBackendName, ctx, db.Etcd,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening etcd DB: %v", err)
		}
		closeFuncs[NSChannelDB] = etcdBackend.Close

		returnEarly = false
		return &DatabaseBackends{
			GraphDB:       etcdBackend,
			ChanStateDB:   etcdBackend,
			HeightHintDB:  etcdBackend,
			MacaroonDB:    etcdBackend,
			DecayedLogDB:  etcdBackend,
			TowerClientDB: etcdBackend,
			TowerServerDB: etcdBackend,
			// The wallet loader will attempt to use/create the
			// wallet in the replicated remote DB if we're running
			// in a clustered environment. This will ensure that all
			// members of the cluster have access to the same wallet
			// state.
			WalletDB: btcwallet.LoaderWithExternalWalletDB(
				etcdBackend,
			),
			Remote:     true,
			CloseFuncs: closeFuncs,
		}, nil
	}

	// We're using all bbolt based databases by default.
	boltBackend, err := kvdb.GetBoltBackend(&kvdb.BoltBackendConfig{
		DBPath:            chanDBPath,
		DBFileName:        channelDBName,
		DBTimeout:         db.Bolt.DBTimeout,
		NoFreelistSync:    !db.Bolt.SyncFreelist,
		AutoCompact:       db.Bolt.AutoCompact,
		AutoCompactMinAge: db.Bolt.AutoCompactMinAge,
	})
	if err != nil {
		return nil, fmt.Errorf("error opening bolt DB: %v", err)
	}
	closeFuncs[NSChannelDB] = boltBackend.Close

	macaroonBackend, err := kvdb.GetBoltBackend(&kvdb.BoltBackendConfig{
		DBPath:            walletDBPath,
		DBFileName:        macaroonDBName,
		DBTimeout:         db.Bolt.DBTimeout,
		NoFreelistSync:    !db.Bolt.SyncFreelist,
		AutoCompact:       db.Bolt.AutoCompact,
		AutoCompactMinAge: db.Bolt.AutoCompactMinAge,
	})
	if err != nil {
		return nil, fmt.Errorf("error opening macaroon DB: %v", err)
	}
	closeFuncs[NSMacaroonDB] = macaroonBackend.Close

	decayedLogBackend, err := kvdb.GetBoltBackend(&kvdb.BoltBackendConfig{
		DBPath:            chanDBPath,
		DBFileName:        decayedLogDbName,
		DBTimeout:         db.Bolt.DBTimeout,
		NoFreelistSync:    !db.Bolt.SyncFreelist,
		AutoCompact:       db.Bolt.AutoCompact,
		AutoCompactMinAge: db.Bolt.AutoCompactMinAge,
	})
	if err != nil {
		return nil, fmt.Errorf("error opening decayed log DB: %v", err)
	}
	closeFuncs[NSDecayedLogDB] = decayedLogBackend.Close

	// The tower client is optional and might not be enabled by the user. We
	// handle it being nil properly in the main server.
	var towerClientBackend kvdb.Backend
	if towerClientEnabled {
		towerClientBackend, err = kvdb.GetBoltBackend(
			&kvdb.BoltBackendConfig{
				DBPath:            chanDBPath,
				DBFileName:        towerClientDBName,
				DBTimeout:         db.Bolt.DBTimeout,
				NoFreelistSync:    !db.Bolt.SyncFreelist,
				AutoCompact:       db.Bolt.AutoCompact,
				AutoCompactMinAge: db.Bolt.AutoCompactMinAge,
			},
		)
		if err != nil {
			return nil, fmt.Errorf("error opening tower client "+
				"DB: %v", err)
		}
		closeFuncs[NSTowerClientDB] = towerClientBackend.Close
	}

	// The tower server is optional and might not be enabled by the user. We
	// handle it being nil properly in the main server.
	var towerServerBackend kvdb.Backend
	if towerServerEnabled {
		towerServerBackend, err = kvdb.GetBoltBackend(
			&kvdb.BoltBackendConfig{
				DBPath:            towerServerDBPath,
				DBFileName:        towerServerDBName,
				DBTimeout:         db.Bolt.DBTimeout,
				NoFreelistSync:    !db.Bolt.SyncFreelist,
				AutoCompact:       db.Bolt.AutoCompact,
				AutoCompactMinAge: db.Bolt.AutoCompactMinAge,
			},
		)
		if err != nil {
			return nil, fmt.Errorf("error opening tower server "+
				"DB: %v", err)
		}
		closeFuncs[NSTowerServerDB] = towerServerBackend.Close
	}

	returnEarly = false
	return &DatabaseBackends{
		GraphDB:       boltBackend,
		ChanStateDB:   boltBackend,
		HeightHintDB:  boltBackend,
		MacaroonDB:    macaroonBackend,
		DecayedLogDB:  decayedLogBackend,
		TowerClientDB: towerClientBackend,
		TowerServerDB: towerServerBackend,
		// When "running locally", LND will use the bbolt wallet.db to
		// store the wallet located in the chain data dir, parametrized
		// by the active network.
		WalletDB: btcwallet.LoaderWithLocalWalletDB(
			walletDBPath, !db.Bolt.SyncFreelist, db.Bolt.DBTimeout,
		),
		CloseFuncs: closeFuncs,
	}, nil
}

// Compile-time constraint to ensure Workers implements the Validator interface.
var _ Validator = (*DB)(nil)
