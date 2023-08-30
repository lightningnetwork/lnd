//go:build kvdb_postgres || (kvdb_sqlite && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64)))

package sqlbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
)

const (
	// kvTableName is the name of the table that will contain all the kv
	// pairs.
	kvTableName = "kv"

	// DefaultNumTxRetries is the default number of times we'll retry a
	// transaction if it fails with an error that permits transaction
	// repetition.
	DefaultNumTxRetries = 10

	// DefaultInitialRetryDelay is the default initial delay between
	// retries. This will be used to generate a random delay between -50%
	// and +50% of this value, so 20 to 60 milliseconds. The retry will be
	// doubled after each attempt until we reach DefaultMaxRetryDelay. We
	// start with a random value to avoid multiple goroutines that are
	// created at the same time to effectively retry at the same time.
	DefaultInitialRetryDelay = time.Millisecond * 50

	// DefaultMaxRetryDelay is the default maximum delay between retries.
	DefaultMaxRetryDelay = time.Second * 5
)

var (
	// ErrRetriesExceeded is returned when a transaction is retried more
	// than the max allowed valued without a success.
	ErrRetriesExceeded = errors.New("db tx retries exceeded")
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

	// lock is the global write lock that ensures single writer. This is
	// only used if cfg.WithTxLevelLock is set.
	lock sync.RWMutex

	// table is the name of the table that contains the data for all
	// top-level buckets that have keys that cannot be mapped to a distinct
	// sql table.
	table string
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
func (db *db) Update(f func(tx walletdb.ReadWriteTx) error,
	reset func()) error {

	return db.executeTransaction(f, reset, false)
}

// randRetryDelay returns a random retry delay between -50% and +50% of the
// configured delay that is doubled for each attempt and capped at a max value.
func randRetryDelay(initialRetryDelay, maxRetryDelay, attempt int) time.Duration {
	halfDelay := initialRetryDelay / 2
	randDelay := prand.Int63n(int64(initialRetryDelay)) //nolint:gosec

	// 50% plus 0%-100% gives us the range of 50%-150%.
	initialDelay := halfDelay + time.Duration(randDelay)

	// If this is the first attempt, we just return the initial delay.
	if attempt == 0 {
		return initialDelay
	}

	// For each subsequent delay, we double the initial delay. This still
	// gives us a somewhat random delay, but it still increases with each
	// attempt. If we double something n times, that's the same as
	// multiplying the value with 2^n. We limit the power to 32 to avoid
	// overflows.
	factor := time.Duration(math.Pow(2, math.Min(float64(attempt), 32)))
	actualDelay := initialDelay * factor

	// Cap the delay at the maximum configured value.
	if actualDelay > maxRetryDelay {
		return maxRetryDelay
	}

	return actualDelay
}

// executeTransaction creates a new read-only or read-write transaction and
// executes the given function within it.
func (db *db) executeTransaction(f func(tx walletdb.ReadWriteTx) error,
	reset func(), readOnly bool) error {

	// waitBeforeRetry is a helper function that will wait for a random
	// interval before exiting to retry the db transaction. If false is
	// returned, then this means that daemon is shutting down so we
	// should abort the retries.
	waitBeforeRetry := func(attemptNumber int) bool {
		retryDelay := randRetryDelay(
			attemptNumber, DefaultInitialRetryDelay,
			DefaultMaxRetryDelay,
		)

		log.Debugf("Retrying transaction due to tx serialization "+
			"error, attempt_number=%v, delay=%v", attemptNumber,
			retryDelay)

		select {
		// Before we try again, we'll wait with a random backoff based
		// on the retry delay.
		case time.After(retryDelay):
			return true

		// If the daemon is shutting down, then we'll exit early.
		case <-db.Context.Done():
			return false
		}
	}

	for i := 0; i < DefaultNumTxRetries; i++ {
		reset()

		tx, err := newReadWriteTx(db, readOnly)
		if err != nil {
			dbErr := MapSQLError(err)

			if IsSerializationError(dbErr) {
				// Nothing to roll back here, since we didn't
				// even get a transaction yet.
				if waitBeforeRetry(i) {
					continue
				}
			}

			return dbErr
		}

		err = catchPanic(func() error { return f(tx) })
		if err != nil {
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				log.Errorf("Error rolling back tx: %v",
					rollbackErr)
			}

			return err
		}

		dbErr := tx.Commit()
		if IsSerializationError(dbErr) {
			_ = tx.Rollback()

			if waitBeforeRetry(i) {
				continue
			}
		}

		return dbErr
	}

	// If we get to this point, then we weren't able to successfully commit
	// a tx given the max number of retries.
	return ErrRetriesExceeded
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
