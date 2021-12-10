package lncfg

import (
	"context"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/kvdb/etcd"
	"github.com/lightningnetwork/lnd/kvdb/postgres"
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
	PostgresBackend            = "postgres"
	DefaultBatchCommitInterval = 500 * time.Millisecond

	defaultPostgresMaxConnections = 50

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

	// NSWalletDB is the namespace name that we use for the wallet DB.
	NSWalletDB = "walletdb"
)

// DB holds database configuration for LND.
type DB struct {
	Backend string `long:"backend" description:"The selected database backend."`

	BatchCommitInterval time.Duration `long:"batch-commit-interval" description:"The maximum duration the channel graph batch schedulers will wait before attempting to commit a batch of pending updates. This can be tradeoff database contenion for commit latency."`

	Etcd *etcd.Config `group:"etcd" namespace:"etcd" description:"Etcd settings."`

	Bolt *kvdb.BoltConfig `group:"bolt" namespace:"bolt" description:"Bolt settings."`

	Postgres *postgres.Config `group:"postgres" namespace:"postgres" description:"Postgres settings."`

	NoGraphCache bool `long:"no-graph-cache" description:"Don't use the in-memory graph cache for path finding. Much slower but uses less RAM. Can only be used with a bolt database backend."`
}

// DefaultDB creates and returns a new default DB config.
func DefaultDB() *DB {
	return &DB{
		Backend:             BoltBackend,
		BatchCommitInterval: DefaultBatchCommitInterval,
		Bolt: &kvdb.BoltConfig{
			NoFreelistSync:    true,
			AutoCompactMinAge: kvdb.DefaultBoltAutoCompactMinAge,
			DBTimeout:         kvdb.DefaultDBTimeout,
		},
		Etcd: &etcd.Config{
			// Allow at most 32 MiB messages by default.
			MaxMsgSize: 32768 * 1024,
		},
		Postgres: &postgres.Config{
			MaxConnections: defaultPostgresMaxConnections,
		},
	}
}

// Validate validates the DB config.
func (db *DB) Validate() error {
	switch db.Backend {
	case BoltBackend:
	case PostgresBackend:
		if db.Postgres.Dsn == "" {
			return fmt.Errorf("postgres dsn must be set")
		}

	case EtcdBackend:
		if !db.Etcd.Embedded && db.Etcd.Host == "" {
			return fmt.Errorf("etcd host must be set")
		}

	default:
		return fmt.Errorf("unknown backend, must be either '%v' or "+
			"'%v'", BoltBackend, EtcdBackend)
	}

	// The path finding uses a manual read transaction that's open for a
	// potentially long time. That works fine with the locking model of
	// bbolt but can lead to locks or rolled back transactions with etcd or
	// postgres. And since we already have a smaller memory footprint for
	// remote database setups (due to not needing to memory-map the bbolt DB
	// files), we can keep the graph in memory instead. But for mobile
	// devices the tradeoff between a smaller memory footprint and the
	// longer time needed for path finding might be a desirable one.
	if db.NoGraphCache && db.Backend != BoltBackend {
		return fmt.Errorf("cannot use no-graph-cache with database "+
			"backend '%v'", db.Backend)
	}

	return nil
}

// Init should be called upon start to pre-initialize database access dependent
// on configuration.
func (db *DB) Init(ctx context.Context, dbPath string) error {
	// Start embedded etcd server if requested.
	switch {
	case db.Backend == EtcdBackend && db.Etcd.Embedded:
		cfg, _, err := kvdb.StartEtcdTestBackend(
			dbPath, db.Etcd.EmbeddedClientPort,
			db.Etcd.EmbeddedPeerPort, db.Etcd.EmbeddedLogFile,
		)
		if err != nil {
			return err
		}

		// Override the original config with the config for
		// the embedded instance.
		db.Etcd = cfg

	case db.Backend == PostgresBackend:
		postgres.Init(db.Postgres.MaxConnections)
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

	switch db.Backend {
	case EtcdBackend:
		// As long as the graph data, channel state and height hint
		// cache are all still in the channel.db file in bolt, we
		// replicate the same behavior here and use the same etcd
		// backend for those three sub DBs. But we namespace it properly
		// to make such a split even easier in the future. This will
		// break lnd for users that ran on etcd with 0.13.x since that
		// code used the root namespace. We assume that nobody used etcd
		// for mainnet just yet since that feature was clearly marked as
		// experimental in 0.13.x.
		etcdBackend, err := kvdb.Open(
			kvdb.EtcdBackendName, ctx,
			db.Etcd.CloneWithSubNamespace(NSChannelDB),
		)
		if err != nil {
			return nil, fmt.Errorf("error opening etcd DB: %v", err)
		}
		closeFuncs[NSChannelDB] = etcdBackend.Close

		etcdMacaroonBackend, err := kvdb.Open(
			kvdb.EtcdBackendName, ctx,
			db.Etcd.CloneWithSubNamespace(NSMacaroonDB),
		)
		if err != nil {
			return nil, fmt.Errorf("error opening etcd macaroon "+
				"DB: %v", err)
		}
		closeFuncs[NSMacaroonDB] = etcdMacaroonBackend.Close

		etcdDecayedLogBackend, err := kvdb.Open(
			kvdb.EtcdBackendName, ctx,
			db.Etcd.CloneWithSubNamespace(NSDecayedLogDB),
		)
		if err != nil {
			return nil, fmt.Errorf("error opening etcd decayed "+
				"log DB: %v", err)
		}
		closeFuncs[NSDecayedLogDB] = etcdDecayedLogBackend.Close

		etcdTowerClientBackend, err := kvdb.Open(
			kvdb.EtcdBackendName, ctx,
			db.Etcd.CloneWithSubNamespace(NSTowerClientDB),
		)
		if err != nil {
			return nil, fmt.Errorf("error opening etcd tower "+
				"client DB: %v", err)
		}
		closeFuncs[NSTowerClientDB] = etcdTowerClientBackend.Close

		etcdTowerServerBackend, err := kvdb.Open(
			kvdb.EtcdBackendName, ctx,
			db.Etcd.CloneWithSubNamespace(NSTowerServerDB),
		)
		if err != nil {
			return nil, fmt.Errorf("error opening etcd tower "+
				"server DB: %v", err)
		}
		closeFuncs[NSTowerServerDB] = etcdTowerServerBackend.Close

		etcdWalletBackend, err := kvdb.Open(
			kvdb.EtcdBackendName, ctx,
			db.Etcd.
				CloneWithSubNamespace(NSWalletDB).
				CloneWithSingleWriter(),
		)
		if err != nil {
			return nil, fmt.Errorf("error opening etcd macaroon "+
				"DB: %v", err)
		}
		closeFuncs[NSWalletDB] = etcdWalletBackend.Close

		returnEarly = false
		return &DatabaseBackends{
			GraphDB:       etcdBackend,
			ChanStateDB:   etcdBackend,
			HeightHintDB:  etcdBackend,
			MacaroonDB:    etcdMacaroonBackend,
			DecayedLogDB:  etcdDecayedLogBackend,
			TowerClientDB: etcdTowerClientBackend,
			TowerServerDB: etcdTowerServerBackend,
			// The wallet loader will attempt to use/create the
			// wallet in the replicated remote DB if we're running
			// in a clustered environment. This will ensure that all
			// members of the cluster have access to the same wallet
			// state.
			WalletDB: btcwallet.LoaderWithExternalWalletDB(
				etcdWalletBackend,
			),
			Remote:     true,
			CloseFuncs: closeFuncs,
		}, nil

	case PostgresBackend:
		postgresBackend, err := kvdb.Open(
			kvdb.PostgresBackendName, ctx,
			db.Postgres, NSChannelDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening postgres graph "+
				"DB: %v", err)
		}
		closeFuncs[NSChannelDB] = postgresBackend.Close

		postgresMacaroonBackend, err := kvdb.Open(
			kvdb.PostgresBackendName, ctx,
			db.Postgres, NSMacaroonDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening postgres "+
				"macaroon DB: %v", err)
		}
		closeFuncs[NSMacaroonDB] = postgresMacaroonBackend.Close

		postgresDecayedLogBackend, err := kvdb.Open(
			kvdb.PostgresBackendName, ctx,
			db.Postgres, NSDecayedLogDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening postgres "+
				"decayed log DB: %v", err)
		}
		closeFuncs[NSDecayedLogDB] = postgresDecayedLogBackend.Close

		postgresTowerClientBackend, err := kvdb.Open(
			kvdb.PostgresBackendName, ctx,
			db.Postgres, NSTowerClientDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening postgres tower "+
				"client DB: %v", err)
		}
		closeFuncs[NSTowerClientDB] = postgresTowerClientBackend.Close

		postgresTowerServerBackend, err := kvdb.Open(
			kvdb.PostgresBackendName, ctx,
			db.Postgres, NSTowerServerDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening postgres tower "+
				"server DB: %v", err)
		}
		closeFuncs[NSTowerServerDB] = postgresTowerServerBackend.Close

		postgresWalletBackend, err := kvdb.Open(
			kvdb.PostgresBackendName, ctx,
			db.Postgres, NSWalletDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening postgres macaroon "+
				"DB: %v", err)
		}
		closeFuncs[NSWalletDB] = postgresWalletBackend.Close

		returnEarly = false
		return &DatabaseBackends{
			GraphDB:       postgresBackend,
			ChanStateDB:   postgresBackend,
			HeightHintDB:  postgresBackend,
			MacaroonDB:    postgresMacaroonBackend,
			DecayedLogDB:  postgresDecayedLogBackend,
			TowerClientDB: postgresTowerClientBackend,
			TowerServerDB: postgresTowerServerBackend,
			// The wallet loader will attempt to use/create the
			// wallet in the replicated remote DB if we're running
			// in a clustered environment. This will ensure that all
			// members of the cluster have access to the same wallet
			// state.
			WalletDB: btcwallet.LoaderWithExternalWalletDB(
				postgresWalletBackend,
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
		NoFreelistSync:    db.Bolt.NoFreelistSync,
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
		NoFreelistSync:    db.Bolt.NoFreelistSync,
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
		NoFreelistSync:    db.Bolt.NoFreelistSync,
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
				NoFreelistSync:    db.Bolt.NoFreelistSync,
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
				NoFreelistSync:    db.Bolt.NoFreelistSync,
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
		// by the active network. The wallet loader has its own cleanup
		// method so we don't need to add anything to our map (in fact
		// nothing is opened just yet).
		WalletDB: btcwallet.LoaderWithLocalWalletDB(
			walletDBPath, db.Bolt.NoFreelistSync, db.Bolt.DBTimeout,
		),
		CloseFuncs: closeFuncs,
	}, nil
}

// Compile-time constraint to ensure Workers implements the Validator interface.
var _ Validator = (*DB)(nil)
