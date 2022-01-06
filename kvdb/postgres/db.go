//go:build kvdb_postgres
// +build kvdb_postgres

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
)

const (
	// kvTableName is the name of the table that will contain all the kv
	// pairs.
	kvTableName = "kv"
)

// KV stores a key/value pair.
type KV struct {
	key string
	val string
}

// db holds a reference to the postgres connection connection.
type db struct {
	// cfg is the postgres connection config.
	cfg *Config

	// prefix is the table name prefix that is used to simulate namespaces.
	// We don't use schemas because at least sqlite does not support that.
	prefix string

	// ctx is the overall context for the database driver.
	//
	// TODO: This is an anti-pattern that is in place until the kvdb
	// interface supports a context.
	ctx context.Context

	// db is the underlying database connection instance.
	db *sql.DB

	// lock is the global write lock that ensures single writer.
	lock sync.RWMutex

	// table is the name of the table that contains the data for all
	// top-level buckets that have keys that cannot be mapped to a distinct
	// sql table.
	table string
}

// Enforce db implements the walletdb.DB interface.
var _ walletdb.DB = (*db)(nil)

// Global set of database connections.
var dbConns *dbConnSet

// Init initializes the global set of database connections.
func Init(maxConnections int) {
	dbConns = newDbConnSet(maxConnections)
}

// newPostgresBackend returns a db object initialized with the passed backend
// config. If postgres connection cannot be estabished, then returns error.
func newPostgresBackend(ctx context.Context, config *Config, prefix string) (
	*db, error) {

	if prefix == "" {
		return nil, errors.New("empty postgres prefix")
	}

	if dbConns == nil {
		return nil, errors.New("db connection set not initialized")
	}

	dbConn, err := dbConns.Open(config.Dsn)
	if err != nil {
		return nil, err
	}

	// Compose system table names.
	table := fmt.Sprintf(
		"%s_%s", prefix, kvTableName,
	)

	// Execute the create statements to set up a kv table in postgres. Every
	// row points to the bucket that it is one via its parent_id field. A
	// NULL parent_id means that the key belongs to the upper-most bucket in
	// this table. A constraint on parent_id is enforcing referential
	// integrity.
	//
	// Furthermore there is a <table>_p index on parent_id that is required
	// for the foreign key constraint.
	//
	// Finally there are unique indices on (parent_id, key) to prevent the
	// same key being present in a bucket more than once (<table>_up and
	// <table>_unp). In postgres, a single index wouldn't enforce the unique
	// constraint on rows with a NULL parent_id. Therefore two indices are
	// defined.
	_, err = dbConn.ExecContext(ctx, `
CREATE SCHEMA IF NOT EXISTS public;
CREATE TABLE IF NOT EXISTS public.`+table+`
(
    key bytea NOT NULL,
    value bytea,
    parent_id bigint,
    id bigserial PRIMARY KEY,
    sequence bigint,
    CONSTRAINT `+table+`_parent FOREIGN KEY (parent_id)
        REFERENCES public.`+table+` (id)
        ON UPDATE NO ACTION
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS `+table+`_p
    ON public.`+table+` (parent_id);

CREATE UNIQUE INDEX IF NOT EXISTS `+table+`_up
    ON public.`+table+`
    (parent_id, key) WHERE parent_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS `+table+`_unp 
    ON public.`+table+` (key) WHERE parent_id IS NULL;
`)
	if err != nil {
		_ = dbConn.Close()

		return nil, err
	}

	backend := &db{
		cfg:    config,
		prefix: prefix,
		ctx:    ctx,
		db:     dbConn,
		table:  table,
	}

	return backend, nil
}

// getTimeoutCtx gets a timeout context for database requests.
func (db *db) getTimeoutCtx() (context.Context, func()) {
	if db.cfg.Timeout == time.Duration(0) {
		return db.ctx, func() {}
	}

	return context.WithTimeout(db.ctx, db.cfg.Timeout)
}

// getPrefixedTableName returns a table name for this prefix (namespace).
func (db *db) getPrefixedTableName(table string) string {
	return fmt.Sprintf("%s_%s", db.prefix, table)
}

// catchPanic executes the specified function. If a panic occurs, it is returned
// as an error value.
func catchPanic(f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Criticalf("Caught unhandled error: %v", r)

			switch data := r.(type) {
			case error:
				err = data

			default:
				err = errors.New(fmt.Sprintf("%v", data))
			}
		}
	}()

	err = f()

	return
}

// View opens a database read transaction and executes the function f with the
// transaction passed as a parameter. After f exits, the transaction is rolled
// back. If f errors, its error is returned, not a rollback error (if any
// occur). The passed reset function is called before the start of the
// transaction and can be used to reset intermediate state. As callers may
// expect retries of the f closure (depending on the database backend used), the
// reset function will be called before each retry respectively.
func (db *db) View(f func(tx walletdb.ReadTx) error, reset func()) error {
	return db.executeTransaction(
		func(tx walletdb.ReadWriteTx) error {
			return f(tx.(walletdb.ReadTx))
		},
		reset, true,
	)
}

// Update opens a database read/write transaction and executes the function f
// with the transaction passed as a parameter. After f exits, if f did not
// error, the transaction is committed. Otherwise, if f did error, the
// transaction is rolled back. If the rollback fails, the original error
// returned by f is still returned. If the commit fails, the commit error is
// returned. As callers may expect retries of the f closure, the reset function
// will be called before each retry respectively.
func (db *db) Update(f func(tx walletdb.ReadWriteTx) error, reset func()) (err error) {
	return db.executeTransaction(f, reset, false)
}

// executeTransaction creates a new read-only or read-write transaction and
// executes the given function within it.
func (db *db) executeTransaction(f func(tx walletdb.ReadWriteTx) error,
	reset func(), readOnly bool) error {

	reset()

	tx, err := newReadWriteTx(db, readOnly)
	if err != nil {
		return err
	}

	err = catchPanic(func() error { return f(tx) })
	if err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			log.Errorf("Error rolling back tx: %v", rollbackErr)
		}

		return err
	}

	return tx.Commit()
}

// PrintStats returns all collected stats pretty printed into a string.
func (db *db) PrintStats() string {
	return "stats not supported by Postgres driver"
}

// BeginReadWriteTx opens a database read+write transaction.
func (db *db) BeginReadWriteTx() (walletdb.ReadWriteTx, error) {
	return newReadWriteTx(db, false)
}

// BeginReadTx opens a database read transaction.
func (db *db) BeginReadTx() (walletdb.ReadTx, error) {
	return newReadWriteTx(db, true)
}

// Copy writes a copy of the database to the provided writer. This call will
// start a read-only transaction to perform all operations.
// This function is part of the walletdb.Db interface implementation.
func (db *db) Copy(w io.Writer) error {
	return errors.New("not implemented")
}

// Close cleanly shuts down the database and syncs all data.
// This function is part of the walletdb.Db interface implementation.
func (db *db) Close() error {
	log.Infof("Closing database %v", db.prefix)

	return dbConns.Close(db.cfg.Dsn)
}
