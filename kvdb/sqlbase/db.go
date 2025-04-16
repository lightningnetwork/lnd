//go:build kvdb_postgres || (kvdb_sqlite && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64)))

package sqlbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/sqldb"
)

const (
	// kvTableName is the name of the table that will contain all the kv
	// pairs.
	kvTableName = "kv"

	// DefaultNumTxRetries is the default number of times we'll retry a
	// transaction if it fails with an error that permits transaction
	// repetition.
	DefaultNumTxRetries = 50
)

// Config holds a set of configuration options of a sql database connection.
type Config struct {
	// DriverName is the string that defines the registered sql driver that
	// is to be used.
	DriverName string

	// Dsn is the database connection string that will be used to connect
	// to the db.
	Dsn string

	// Timeout is the time after which a query to the db will be canceled if
	// it has not yet completed.
	Timeout time.Duration

	// Schema is the name of the schema under which the sql tables should be
	// created. It should be left empty for backends like sqlite that do not
	// support having more than one schema.
	Schema string

	// TableNamePrefix is the name that should be used as a table name
	// prefix when constructing the KV style table.
	TableNamePrefix string

	// SQLiteCmdReplacements define a one-to-one string mapping of sql
	// keywords to the strings that should replace those keywords in any
	// commands. Note that the sqlite keywords to be replaced are
	// case-sensitive.
	SQLiteCmdReplacements SQLiteCmdReplacements

	// WithTxLevelLock when set will ensure that there is a transaction
	// level lock.
	//
	// NOTE: Temporary, should be removed when all parts of the LND code
	// are more resilient against concurrent db access..
	WithTxLevelLock bool
}

// db holds a reference to the sql db connection.
type db struct {
	// cfg is the sql db connection config.
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

	// table is the name of the table that contains the data for all
	// top-level buckets that have keys that cannot be mapped to a distinct
	// sql table.
	table string

	// lock is the global write lock that ensures single writer. This is
	// only used if cfg.WithTxLevelLock is set.
	lock sync.RWMutex
}

// Enforce db implements the walletdb.DB interface.
var _ walletdb.DB = (*db)(nil)

var (
	// dbConns is a global set of database connections.
	dbConns   *dbConnSet
	dbConnsMu sync.Mutex
)

// Init initializes the global set of database connections.
func Init(maxConnections int) {
	dbConnsMu.Lock()
	defer dbConnsMu.Unlock()

	if dbConns != nil {
		return
	}

	dbConns = newDbConnSet(maxConnections)
}

// NewSqlBackend returns a db object initialized with the passed backend
// config. If database connection cannot be established, then returns error.
func NewSqlBackend(ctx context.Context, cfg *Config) (*db, error) {
	dbConnsMu.Lock()
	defer dbConnsMu.Unlock()

	if dbConns == nil {
		return nil, errors.New("db connection set not initialized")
	}

	if cfg.TableNamePrefix == "" {
		return nil, errors.New("empty table name prefix")
	}

	table := fmt.Sprintf("%s_%s", cfg.TableNamePrefix, kvTableName)

	query := newKVSchemaCreationCmd(
		table, cfg.Schema, cfg.SQLiteCmdReplacements,
	)

	dbConn, err := dbConns.Open(cfg.DriverName, cfg.Dsn)
	if err != nil {
		return nil, err
	}

	_, err = dbConn.ExecContext(ctx, query)
	if err != nil {
		_ = dbConn.Close()

		return nil, err
	}

	return &db{
		cfg:    cfg,
		ctx:    ctx,
		db:     dbConn,
		table:  table,
		prefix: cfg.TableNamePrefix,
	}, nil
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
			switch data := r.(type) {
			case error:
				err = data

			default:
				err = errors.New(fmt.Sprintf("%v", data))
			}

			// Before we issue a critical log which'll cause the
			// daemon to shut down, we'll first check if this is a
			// DB serialization error. If so, then we don't need to
			// log as we can retry safely and avoid tearing
			// everything down.
			if sqldb.IsSerializationError(sqldb.MapSQLError(err)) {
				log.Tracef("Detected db serialization error "+
					"via panic: %v", err)
			} else {
				log.Criticalf("Caught unhandled error: %v", r)
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
func (db *db) Update(f func(tx walletdb.ReadWriteTx) error,
	reset func()) error {

	return db.executeTransaction(f, reset, false)
}

// executeTransaction creates a new read-only or read-write transaction and
// executes the given function within it.
func (db *db) executeTransaction(f func(tx walletdb.ReadWriteTx) error,
	reset func(), readOnly bool) error {

	makeTx := func() (sqldb.Tx, error) {
		return newReadWriteTx(db, readOnly)
	}

	execTxBody := func(tx sqldb.Tx) error {
		kvTx, ok := tx.(*readWriteTx)
		if !ok {
			return fmt.Errorf("expected *readWriteTx, got %T", tx)
		}

		reset()
		return catchPanic(func() error { return f(kvTx) })
	}

	onBackoff := func(retry int, delay time.Duration) {
		log.Tracef("Retrying transaction due to tx serialization "+
			"error, attempt_number=%v, delay=%v", retry, delay)
	}

	rollbackTx := func(tx sqldb.Tx) error {
		kvTx, ok := tx.(*readWriteTx)
		if !ok {
			return fmt.Errorf("expected *readWriteTx, got %T", tx)
		}

		return attemptRollback(kvTx)
	}

	return sqldb.ExecuteSQLTransactionWithRetry(
		db.ctx, makeTx, rollbackTx, execTxBody, onBackoff,
		DefaultNumTxRetries,
	)
}

// PrintStats returns all collected stats pretty printed into a string.
func (db *db) PrintStats() string {
	return "stats not supported by SQL driver"
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
	dbConnsMu.Lock()
	defer dbConnsMu.Unlock()

	log.Infof("Closing database %v", db.prefix)

	return dbConns.Close(db.cfg.Dsn)
}

// attemptRollback attempts to roll back the transaction, and if it fails, it
// will return the error. If the transaction was already closed, it will return
// nil.
func attemptRollback(tx *readWriteTx) error {
	rollbackErr := tx.Rollback()
	if rollbackErr != nil &&
		!errors.Is(rollbackErr, walletdb.ErrTxClosed) &&
		!strings.Contains(rollbackErr.Error(), "conn closed") {

		return fmt.Errorf("error rolling back tx: %w", rollbackErr)
	}

	return nil
}
