package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/jessevdk/go-flags"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/kvdb/etcd"
	"github.com/lightningnetwork/lnd/kvdb/postgres"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/signal"
)

var (
	// tombstoneKey is the key under which we add a tag in the source DB
	// after we've successfully and completely migrated it to the target/
	// destination DB.
	tombstoneKey = []byte("data-migration-tombstone")

	// alreadyMigratedKey is the key under which we add a tag in the target/
	// destination DB after we've successfully and completely migrated it
	// from a source DB.
	alreadyMigratedKey = []byte("data-migration-already-migrated")
)

const (
	MigrationMaxCallSize = 10240000000
)

type Bolt struct {
	DBTimeout time.Duration `long:"dbtimeout" description:"Specify the timeout value used when opening the database."`
	DataDir   string        `long:"data-dir" description:"Lnd data dir where bolt dbs are located."`
	TowerDir  string        `long:"tower-dir" description:"Lnd watchtower dir where bolt dbs for the watchtower server are located."`
	Network   string        `long:"network" description:"Network within data dir where bolt dbs are located."`
}

type DB struct {
	Backend  string           `long:"backend" description:"The selected database backend."`
	Etcd     *etcd.Config     `group:"etcd" namespace:"etcd" description:"Etcd settings."`
	Bolt     *Bolt            `group:"bolt" namespace:"bolt" description:"Bolt settings."`
	Postgres *postgres.Config `group:"postgres" namespace:"postgres" description:"Postgres settings."`
}

type migrateDBCommand struct {
	Source *DB `group:"source" namespace:"source" long:"source" short:"s" description:"The source database where the data is read from; will be deleted by default (after successful migration) to avoid keeping old channel state around"`
	Dest   *DB `group:"dest" namespace:"dest" long:"dest" short:"d" description:"The destination database where the data is written to"`
}

func newMigrateDBCommand() *migrateDBCommand {
	return &migrateDBCommand{
		Source: &DB{
			Backend: lncfg.BoltBackend,
			Etcd:    &etcd.Config{},
			Bolt: &Bolt{
				DBTimeout: kvdb.DefaultDBTimeout,
			},
			Postgres: &postgres.Config{},
		},
		Dest: &DB{
			Backend: lncfg.EtcdBackend,
			Etcd: &etcd.Config{
				MaxCallSizeBytes: MigrationMaxCallSize,
			},
			Bolt: &Bolt{
				DBTimeout: kvdb.DefaultDBTimeout,
			},
			Postgres: &postgres.Config{},
		},
	}
}

func (x *migrateDBCommand) Register(parser *flags.Parser) error {
	_, err := parser.AddCommand(
		"migrate-db",
		"Migrate an lnd source DB to a destination DB",
		"Migrate an lnd source database (for example the bbolt based "+
			"channel.db) to a destination database (for example "+
			"a PostgreSQL database)",
		x,
	)
	return err
}

func (x *migrateDBCommand) Execute(_ []string) error {
	// Since this will potentially run for a while, make sure we catch any
	// interrupt signals.
	_, err := signal.Intercept()
	if err != nil {
		return fmt.Errorf("error intercepting signals: %v", err)
	}

	var prefixes = []string{
		lncfg.NSChannelDB, lncfg.NSMacaroonDB, lncfg.NSDecayedLogDB,
		lncfg.NSTowerClientDB, lncfg.NSTowerServerDB, lncfg.NSWalletDB,
	}

	for _, prefix := range prefixes {
		log("Migrating DB with prefix %s", prefix)

		srcDb, err := openDb(x.Source, prefix)
		if err != nil {
			if err == walletdb.ErrDbDoesNotExist &&
				x.Source.Backend == lncfg.BoltBackend {

				log("Skipping DB with prefix %s because "+
					"source does not exist", prefix)
				continue
			}
			return err
		}

		destDb, err := openDb(x.Dest, prefix)
		if err != nil {
			return err
		}

		// Check that the source database hasn't been marked with a
		// tombstone yet. Once we set the tombstone we see the DB as not
		// viable for migration anymore to avoid old state overwriting
		// new state. We only set the tombstone at the end of a
		// successful and complete migration.
		marker, err := checkMarkerPresent(srcDb, tombstoneKey)
		if err == nil && len(marker) > 0 {
			log("Skipping DB with prefix %s because the source "+
				"DB was marked with a tombstone which "+
				"means it was already migrated successfully. "+
				"Tombstone reads: %s", marker)
			continue
		}
		if err != nil {
			return err
		}

		// Also make sure that the destination DB hasn't been marked as
		// successfully having been the target of a migration. We only
		// mark a destination DB as successfully migrated at the end of
		// a successful and complete migration.
		marker, err = checkMarkerPresent(destDb, alreadyMigratedKey)
		if err == nil && len(marker) > 0 {
			log("Skipping DB with prefix %s because the "+
				"destination DB was marked as already having "+
				"been the target of a successful migration. "+
				"Tag reads: %s", marker)
			continue
		}

		// Using ReadWrite otherwise there is no access to the sequence
		// number.
		srcTx, err := srcDb.BeginReadWriteTx()
		if err != nil {
			return err
		}

		err = srcTx.ForEachBucket(func(key []byte) error {
			log("Copying top-level bucket '%s'",
				loggableKeyName(key))

			destTx, err := destDb.BeginReadWriteTx()
			if err != nil {
				return err
			}

			destBucket, err := destTx.CreateTopLevelBucket(key)
			if err != nil {
				return fmt.Errorf("error creating top level "+
					"bucket '%s': %v", loggableKeyName(key),
					err)
			}

			srcBucket := srcTx.ReadWriteBucket(key)
			err = copyBucket(srcBucket, destBucket)
			if err != nil {
				return fmt.Errorf("error copying bucket '%s': "+
					"%v", loggableKeyName(key), err)
			}

			log("Committing bucket '%s'", loggableKeyName(key))
			if err := destTx.Commit(); err != nil {
				return fmt.Errorf("error committing bucket "+
					"'%s': %v", loggableKeyName(key), err)
			}

			return nil

		})
		if err != nil {
			return fmt.Errorf("error enumerating top level "+
				"buckets: %v", err)
		}

		// We're done now, so we can roll back the read transaction of
		// the source DB.
		if err := srcTx.Rollback(); err != nil {
			return fmt.Errorf("error rolling back source tx: %v",
				err)
		}

		// Migrate wallet created marker.
		if prefix == lncfg.NSWalletDB {
			const (
				walletMetaBucket = "lnwallet"
				walletReadyKey   = "ready"
			)

			log("Creating 'wallet created' marker")
			destTx, err := destDb.BeginReadWriteTx()
			if err != nil {
				return err
			}

			metaBucket, err := destTx.CreateTopLevelBucket(
				[]byte(walletMetaBucket),
			)
			if err != nil {
				return err
			}

			err = metaBucket.Put(
				[]byte(walletReadyKey), []byte(walletReadyKey),
			)
			if err != nil {
				return err
			}

			log("Committing 'wallet created' marker")
			if err := destTx.Commit(); err != nil {
				return fmt.Errorf("error committing 'wallet "+
					"created' marker: %v", err)
			}
		}

		// If we get here, we've successfully migrated the DB and can
		// now set the tombstone marker on the source database and the
		// already migrated marker on the target database.
		if err := addMarker(srcDb, tombstoneKey); err != nil {
			return err
		}
		if err := addMarker(destDb, alreadyMigratedKey); err != nil {
			return err
		}
	}

	return nil
}

func copyBucket(src walletdb.ReadWriteBucket,
	dest walletdb.ReadWriteBucket) error {

	if err := dest.SetSequence(src.Sequence()); err != nil {
		return fmt.Errorf("error copying sequence number")
	}

	return src.ForEach(func(k, v []byte) error {
		if v == nil {
			srcBucket := src.NestedReadWriteBucket(k)
			destBucket, err := dest.CreateBucket(k)
			if err != nil {
				return fmt.Errorf("error creating bucket "+
					"'%s': %v", loggableKeyName(k), err)
			}

			if err := copyBucket(srcBucket, destBucket); err != nil {
				return fmt.Errorf("error copying bucket "+
					"'%s': %v", loggableKeyName(k), err)
			}

			return nil
		}

		err := dest.Put(k, v)
		if err != nil {
			return fmt.Errorf("error copying key '%s': %v",
				loggableKeyName(k), err)
		}

		return nil
	})
}

func openDb(cfg *DB, prefix string) (walletdb.DB, error) {
	backend := cfg.Backend

	var args []interface{}

	graphDir := filepath.Join(cfg.Bolt.DataDir, "graph", cfg.Bolt.Network)
	walletDir := filepath.Join(
		cfg.Bolt.DataDir, "chain", "bitcoin", cfg.Bolt.Network,
	)
	towerServerDir := filepath.Join(
		cfg.Bolt.TowerDir, "bitcoin", cfg.Bolt.Network,
	)

	switch backend {
	case lncfg.BoltBackend:
		var path string
		switch prefix {
		case lncfg.NSChannelDB:
			path = filepath.Join(graphDir, lncfg.ChannelDBName)

		case lncfg.NSMacaroonDB:
			path = filepath.Join(walletDir, lncfg.MacaroonDBName)

		case lncfg.NSDecayedLogDB:
			path = filepath.Join(graphDir, lncfg.DecayedLogDbName)

		case lncfg.NSTowerClientDB:
			path = filepath.Join(graphDir, lncfg.TowerClientDBName)

		case lncfg.NSTowerServerDB:
			path = filepath.Join(
				towerServerDir, lncfg.TowerServerDBName,
			)

		case lncfg.NSWalletDB:
			path = filepath.Join(walletDir, lncfg.WalletDBName)
		}

		const (
			noFreelistSync = true
			timeout        = time.Minute
		)

		args = []interface{}{
			path, noFreelistSync, timeout,
		}
		backend = kvdb.BoltBackendName
		log("Opening bbolt backend at %s for prefix '%s'", path, prefix)

	case kvdb.EtcdBackendName:
		args = []interface{}{
			context.Background(),
			cfg.Etcd.CloneWithSubNamespace(prefix),
		}
		log("Opening etcd backend at %s with namespace '%s'",
			cfg.Etcd.Host, prefix)

	case kvdb.PostgresBackendName:
		args = []interface{}{
			context.Background(),
			&postgres.Config{
				Dsn: cfg.Postgres.Dsn,
			},
			prefix,
		}
		log("Opening postgres backend at %s with prefix '%s'",
			cfg.Postgres.Dsn, prefix)

	default:
		return nil, fmt.Errorf("unknown backend: %v", backend)
	}

	return kvdb.Open(backend, args...)
}

func checkMarkerPresent(db walletdb.DB, markerKey []byte) ([]byte, error) {
	rtx, err := db.BeginReadTx()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rtx.Rollback() }()

	markerBucket := rtx.ReadBucket(markerKey)
	if markerBucket == nil {
		return nil, nil
	}

	return markerBucket.Get(markerKey), nil
}

func addMarker(db walletdb.DB, markerKey []byte) error {
	rwtx, err := db.BeginReadWriteTx()
	if err != nil {
		return err
	}

	markerBucket, err := rwtx.CreateTopLevelBucket(markerKey)
	if err != nil {
		return err
	}

	err = markerBucket.Put(markerKey, []byte(fmt.Sprintf(
		"lndinit migrate-db %s", time.Now(),
	)))
	if err != nil {
		return err
	}

	return rwtx.Commit()
}

// loggableKeyName returns a printable name of the given key.
func loggableKeyName(key []byte) string {
	strKey := string(key)
	if hasSpecialChars(strKey) {
		return hex.EncodeToString(key)
	}

	return strKey
}

// hasSpecialChars returns true if any of the characters in the given string
// cannot be printed.
func hasSpecialChars(s string) bool {
	for _, b := range s {
		if !(b >= 'a' && b <= 'z') && !(b >= 'A' && b <= 'Z') &&
			!(b >= '0' && b <= '9') && b != '-' && b != '_' {

			return true
		}
	}

	return false
}
