package lncfg

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/btcsuite/btclog"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/kvdb/etcd"
	"github.com/lightningnetwork/lnd/kvdb/postgres"
	"github.com/lightningnetwork/lnd/kvdb/sqlbase"
	"github.com/lightningnetwork/lnd/kvdb/sqlite"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
)

const (
	ChannelDBName     = "channel.db"
	MacaroonDBName    = "macaroons.db"
	DecayedLogDbName  = "sphinxreplay.db"
	TowerClientDBName = "wtclient.db"
	TowerServerDBName = "watchtower.db"
	WalletDBName      = "wallet.db"

	SqliteChannelDBName  = "channel.sqlite"
	SqliteChainDBName    = "chain.sqlite"
	SqliteNeutrinoDBName = "neutrino.sqlite"
	SqliteTowerDBName    = "watchtower.sqlite"

	BoltBackend                = "bolt"
	EtcdBackend                = "etcd"
	PostgresBackend            = "postgres"
	SqliteBackend              = "sqlite"
	DefaultBatchCommitInterval = 500 * time.Millisecond

	defaultPostgresMaxConnections = 50
	defaultSqliteMaxConnections   = 2

	defaultSqliteBusyTimeout = 5 * time.Second

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

	// NSNeutrinoDB is the namespace name that we use for the neutrino DB.
	NSNeutrinoDB = "neutrinodb"
)

// DB holds database configuration for LND.
//
//nolint:lll
type DB struct {
	Backend string `long:"backend" description:"The selected database backend."`

	BatchCommitInterval time.Duration `long:"batch-commit-interval" description:"The maximum duration the channel graph batch schedulers will wait before attempting to commit a batch of pending updates. This can be tradeoff database contenion for commit latency."`

	Etcd *etcd.Config `group:"etcd" namespace:"etcd" description:"Etcd settings."`

	Bolt *kvdb.BoltConfig `group:"bolt" namespace:"bolt" description:"Bolt settings."`

	Postgres *postgres.Config `group:"postgres" namespace:"postgres" description:"Postgres settings."`

	Sqlite *sqlite.Config `group:"sqlite" namespace:"sqlite" description:"Sqlite settings."`

	NoGraphCache bool `long:"no-graph-cache" description:"Don't use the in-memory graph cache for path finding. Much slower but uses less RAM. Can only be used with a bolt database backend."`

	PruneRevocation bool `long:"prune-revocation" description:"Run the optional migration that prunes the revocation logs to save disk space."`

	NoRevLogAmtData bool `long:"no-rev-log-amt-data" description:"If set, the to-local and to-remote output amounts of revoked commitment transactions will not be stored in the revocation log. Note that once this data is lost, a watchtower client will not be able to back up the revoked state."`
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
		Sqlite: &sqlite.Config{
			MaxConnections: defaultSqliteMaxConnections,
			BusyTimeout:    defaultSqliteBusyTimeout,
		},
	}
}

// Validate validates the DB config.
func (db *DB) Validate() error {
	switch db.Backend {
	case BoltBackend, SqliteBackend:
	case PostgresBackend:
		if db.Postgres.Dsn == "" {
			return fmt.Errorf("postgres dsn must be set")
		}

	case EtcdBackend:
		if !db.Etcd.Embedded && db.Etcd.Host == "" {
			return fmt.Errorf("etcd host must be set")
		}

	default:
		return fmt.Errorf("unknown backend, must be either '%v', "+
			"'%v', '%v' or '%v'", BoltBackend, EtcdBackend,
			PostgresBackend, SqliteBackend)
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
		sqlbase.Init(db.Postgres.MaxConnections)

	case db.Backend == SqliteBackend:
		sqlbase.Init(db.Sqlite.MaxConnections)
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
	// replicated instances or local bbolt or sqlite backed databases.
	Remote bool

	// CloseFuncs is a map of close functions for each of the initialized
	// DB backends keyed by their namespace name.
	CloseFuncs map[string]func() error
}

// GetBackends returns a set of kvdb.Backends as set in the DB config.
func (db *DB) GetBackends(ctx context.Context, chanDBPath,
	walletDBPath, towerServerDBPath string, towerClientEnabled,
	towerServerEnabled bool, logger btclog.Logger) (*DatabaseBackends,
	error) {

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

		// Warn if the user is trying to switch over to a Postgres DB
		// while there is a wallet or channel bbolt DB still present.
		warnExistingBoltDBs(
			logger, "postgres", walletDBPath, WalletDBName,
		)
		warnExistingBoltDBs(
			logger, "postgres", chanDBPath, ChannelDBName,
		)

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

	case SqliteBackend:
		// Note that for sqlite, we put kv tables for the channel.db,
		// wtclient.db and sphinxreplay.db all in the channel.sqlite db.
		// The tables for wallet.db and macaroon.db are in the
		// chain.sqlite db and watchtower.db tables are in the
		// watchtower.sqlite db. The reason for the multiple sqlite dbs
		// is twofold. The first reason is that it maintains the file
		// structure that users are used to. The second reason is the
		// fact that sqlite only supports one writer at a time which
		// would cause deadlocks in the code due to the wallet db often
		// being accessed during a write to another db.
		sqliteBackend, err := kvdb.Open(
			kvdb.SqliteBackendName, ctx, db.Sqlite, chanDBPath,
			SqliteChannelDBName, NSChannelDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening sqlite graph "+
				"DB: %v", err)
		}
		closeFuncs[NSChannelDB] = sqliteBackend.Close

		sqliteMacaroonBackend, err := kvdb.Open(
			kvdb.SqliteBackendName, ctx, db.Sqlite, walletDBPath,
			SqliteChainDBName, NSMacaroonDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening sqlite "+
				"macaroon DB: %v", err)
		}
		closeFuncs[NSMacaroonDB] = sqliteMacaroonBackend.Close

		sqliteDecayedLogBackend, err := kvdb.Open(
			kvdb.SqliteBackendName, ctx, db.Sqlite, chanDBPath,
			SqliteChannelDBName, NSDecayedLogDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening sqlite decayed "+
				"log DB: %v", err)
		}
		closeFuncs[NSDecayedLogDB] = sqliteDecayedLogBackend.Close

		sqliteTowerClientBackend, err := kvdb.Open(
			kvdb.SqliteBackendName, ctx, db.Sqlite, chanDBPath,
			SqliteChannelDBName, NSTowerClientDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening sqlite tower "+
				"client DB: %v", err)
		}
		closeFuncs[NSTowerClientDB] = sqliteTowerClientBackend.Close

		sqliteTowerServerBackend, err := kvdb.Open(
			kvdb.SqliteBackendName, ctx, db.Sqlite,
			towerServerDBPath, SqliteTowerDBName, NSTowerServerDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening sqlite tower "+
				"server DB: %v", err)
		}
		closeFuncs[NSTowerServerDB] = sqliteTowerServerBackend.Close

		sqliteWalletBackend, err := kvdb.Open(
			kvdb.SqliteBackendName, ctx, db.Sqlite, walletDBPath,
			SqliteChainDBName, NSWalletDB,
		)
		if err != nil {
			return nil, fmt.Errorf("error opening sqlite macaroon "+
				"DB: %v", err)
		}
		closeFuncs[NSWalletDB] = sqliteWalletBackend.Close

		// Warn if the user is trying to switch over to a sqlite DB
		// while there is a wallet or channel bbolt DB still present.
		warnExistingBoltDBs(
			logger, "sqlite", walletDBPath, WalletDBName,
		)
		warnExistingBoltDBs(
			logger, "sqlite", chanDBPath, ChannelDBName,
		)

		returnEarly = false

		return &DatabaseBackends{
			GraphDB:       sqliteBackend,
			ChanStateDB:   sqliteBackend,
			HeightHintDB:  sqliteBackend,
			MacaroonDB:    sqliteMacaroonBackend,
			DecayedLogDB:  sqliteDecayedLogBackend,
			TowerClientDB: sqliteTowerClientBackend,
			TowerServerDB: sqliteTowerServerBackend,
			// The wallet loader will attempt to use/create the
			// wallet in the replicated remote DB if we're running
			// in a clustered environment. This will ensure that all
			// members of the cluster have access to the same wallet
			// state.
			WalletDB: btcwallet.LoaderWithExternalWalletDB(
				sqliteWalletBackend,
			),
			CloseFuncs: closeFuncs,
		}, nil
	}

	// We're using all bbolt based databases by default.
	boltBackend, err := kvdb.GetBoltBackend(&kvdb.BoltBackendConfig{
		DBPath:            chanDBPath,
		DBFileName:        ChannelDBName,
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
		DBFileName:        MacaroonDBName,
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
		DBFileName:        DecayedLogDbName,
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
				DBFileName:        TowerClientDBName,
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
				DBFileName:        TowerServerDBName,
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

// warnExistingBoltDBs checks if there is an existing bbolt database in the
// given location and logs a warning if so.
func warnExistingBoltDBs(log btclog.Logger, dbType, dir, fileName string) {
	if lnrpc.FileExists(filepath.Join(dir, fileName)) {
		log.Warnf("Found existing bbolt database file in %s/%s while "+
			"using database type %s. Existing data will NOT be "+
			"migrated to %s automatically!", dir, fileName, dbType,
			dbType)
	}
}

// Compile-time constraint to ensure Workers implements the Validator interface.
var _ Validator = (*DB)(nil)
