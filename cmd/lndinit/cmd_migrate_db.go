package main

import (
	"context"
	"errors"
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

type migrateDBCommand struct {
	Source     *DB  `group:"source" namespace:"source" long:"source" short:"s" description:"The source database where the data is read from; will be deleted by default (after successful migration) to avoid keeping old channel state around"`
	Dest       *DB  `group:"dest" namespace:"dest" long:"dest" short:"d" description:"The destination database where the data is written to"`
	KeepSource bool `long:"keep-source" description:"Don't delete the data in the source database after successful migration'"`
}

func newMigrateDBCommand() *migrateDBCommand {
	return &migrateDBCommand{
		Source: &DB{
			Backend: lncfg.BoltBackend,
			Etcd:    &etcd.Config{},
			Bolt: &kvdb.BoltConfig{
				DBTimeout: kvdb.DefaultDBTimeout,
			},
			Postgres: &postgres.Config{},
		},
		Dest: &DB{
			Backend: lncfg.EtcdBackend,
			Etcd:    &etcd.Config{},
			Bolt: &kvdb.BoltConfig{
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
		"lnd", "macaroon", "replay", "wallet",
	}

	for _, prefix := range prefixes {
		fmt.Printf("Migrating prefix %s\n", prefix)

		srcDb, err := openDb(x.Source, prefix)
		if err != nil {
			return err
		}

		destDb, err := openDb(x.Dest, prefix)
		if err != nil {
			return err
		}

		// Using ReadWrite otherwise there is no access to the sequence
		// number.
		srcTx, err := srcDb.BeginReadWriteTx()
		if err != nil {
			return err
		}

		destTx, err := destDb.BeginReadWriteTx()
		if err != nil {
			return err
		}

		err = srcTx.ForEachBucket(func(key []byte) error {
			fmt.Printf("Copying top-level bucket %x\n", key)

			destBucket, err := destTx.CreateTopLevelBucket(key)
			if err != nil {
				return fmt.Errorf("error creating top level "+
					"bucket %x: %v", key, err)
			}

			srcBucket := srcTx.ReadWriteBucket(key)

			if err := copyBucket(srcBucket, destBucket); err != nil {
				return fmt.Errorf("error copying bucket %x: %v",
					key, err)
			}

			return nil

		})
		if err != nil {
			return fmt.Errorf("error enumerating top level "+
				"buckets: %v", err)
		}

		// Migrate wallet created marker.
		if prefix == "wallet" {
			const (
				walletMetaBucket = "lnwallet"
				walletReadyKey   = "ready"
			)

			metaBucket, err := destTx.CreateTopLevelBucket([]byte(walletMetaBucket))
			if err != nil {
				return err
			}

			err = metaBucket.Put([]byte(walletReadyKey), []byte(walletReadyKey))
			if err != nil {
				return err
			}
		}

		if err := destTx.Commit(); err != nil {
			return fmt.Errorf("error commiting migration: %v", err)
		}

	}

	return nil
}

func copyBucket(src walletdb.ReadWriteBucket, dest walletdb.ReadWriteBucket) error {
	if err := dest.SetSequence(src.Sequence()); err != nil {
		return fmt.Errorf("error copying sequence number")
	}

	return src.ForEach(func(k, v []byte) error {
		if v == nil {
			fmt.Printf("Copying bucket %x\n", k)

			srcBucket := src.NestedReadWriteBucket(k)
			destBucket, err := dest.CreateBucket(k)
			if err != nil {
				return fmt.Errorf("error creating bucket %x: %v",
					k, err)
			}

			if err := copyBucket(srcBucket, destBucket); err != nil {
				return fmt.Errorf("error copying bucket %x: %v",
					k, err)
			}

			return nil
		}

		fmt.Printf("Copying key %x\n", k)

		err := dest.Put(k, v)
		if err != nil {
			return fmt.Errorf("error copying key %x: %v", k, err)
		}

		return nil
	})
}

func openDb(cfg *DB, prefix string) (walletdb.DB, error) {
	backend := cfg.Backend

	var args []interface{}

	switch backend {
	case kvdb.BoltBackendName:
		var path string
		switch prefix {
		case "lnd":
			path = filepath.Join(cfg.BoltDataDir, "graph", cfg.BoltNetwork, "channel.db")

		case "replay":
			path = filepath.Join(cfg.BoltDataDir, "graph", cfg.BoltNetwork, "sphinxreplay.db")

		case "macaroon":
			path = filepath.Join(cfg.BoltDataDir, "chain", "bitcoin", cfg.BoltNetwork, "macaroons.db")

		case "wallet":
			path = filepath.Join(cfg.BoltDataDir, "chain", "bitcoin", cfg.BoltNetwork, "wallet.db")
		}

		const (
			noFreelistSync = true
			timeout        = time.Minute
		)

		args = []interface{}{
			path, noFreelistSync, timeout,
		}

	case kvdb.PostgresBackendName:
		args = []interface{}{
			context.Background(),
			&postgres.Config{
				Dsn: cfg.Postgres.Dsn,
			},
			prefix,
		}

	default:
		return nil, errors.New("unknown backend")
	}

	return kvdb.Open(backend, args...)
}

type DB struct {
	Backend string `long:"backend" description:"The selected database backend."`

	Etcd *etcd.Config `group:"etcd" namespace:"etcd" description:"Etcd settings."`

	Bolt        *kvdb.BoltConfig `group:"bolt" namespace:"bolt" description:"Bolt settings."`
	BoltDataDir string           `long:"bbolt-data-dir" description:"Lnd data dir where bolt dbs are located."`
	BoltNetwork string           `long:"bbolt-network" description:"Network within data dir where bolt dbs are located."`

	Postgres *postgres.Config `group:"postgres" namespace:"postgres" description:"Postgres settings."`
}
