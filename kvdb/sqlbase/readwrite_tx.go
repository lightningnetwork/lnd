//go:build kvdb_postgres || (kvdb_sqlite && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64)))

package sqlbase

import (
	"context"
	"database/sql"
	"sync"

	"github.com/btcsuite/btcwallet/walletdb"
)

// readWriteTx holds a reference to an open postgres transaction.
type readWriteTx struct {
	db *db
	tx *sql.Tx

	// onCommit gets called upon commit.
	onCommit func()

	// active is true if the transaction hasn't been committed yet.
	active bool

	// locker is a pointer to the global db lock.
	locker sync.Locker
}

// newReadWriteTx creates an rw transaction using a connection from the
// specified pool.
func newReadWriteTx(db *db, readOnly bool) (*readWriteTx, error) {
	locker := newNoopLocker()
	if db.cfg.WithTxLevelLock {
		// Obtain the global lock instance. An alternative here is to
		// obtain a database lock from Postgres. Unfortunately there is
		// no database-level lock in Postgres, meaning that each table
		// would need to be locked individually. Perhaps an advisory
		// lock could perform this function too.
		locker = &db.lock
		if readOnly {
			locker = db.lock.RLocker()
		}
	}
	locker.Lock()

	// Start the transaction. Don't use the timeout context because it would
	// be applied to the transaction as a whole. If possible, mark the
	// transaction as read-only to make sure that potential programming
	// errors cannot cause changes to the database.
	tx, err := db.db.BeginTx(
		context.Background(),
		&sql.TxOptions{
			ReadOnly:  readOnly,
			Isolation: sql.LevelSerializable,
		},
	)
	if err != nil {
		locker.Unlock()
		return nil, err
	}

	return &readWriteTx{
		db:     db,
		tx:     tx,
		active: true,
		locker: locker,
	}, nil
}

// ReadBucket opens the root bucket for read only access.  If the bucket
// described by the key does not exist, nil is returned.
func (tx *readWriteTx) ReadBucket(key []byte) walletdb.ReadBucket {
	return tx.ReadWriteBucket(key)
}

// ForEachBucket iterates through all top level buckets.
func (tx *readWriteTx) ForEachBucket(fn func(key []byte) error) error {
	// Fetch binary top level buckets.
	bucket := newReadWriteBucket(tx, nil)
	err := bucket.ForEach(func(k, _ []byte) error {
		return fn(k)
	})
	return err
}

// Rollback closes the transaction, discarding changes (if any) if the
// database was modified by a write transaction.
func (tx *readWriteTx) Rollback() error {
	// If the transaction has been closed roolback will fail.
	if !tx.active {
		return walletdb.ErrTxClosed
	}

	err := tx.tx.Rollback()

	// Unlock the transaction regardless of the error result.
	tx.active = false
	tx.locker.Unlock()
	return err
}

// ReadWriteBucket opens the root bucket for read/write access.  If the
// bucket described by the key does not exist, nil is returned.
func (tx *readWriteTx) ReadWriteBucket(key []byte) walletdb.ReadWriteBucket {
	if len(key) == 0 {
		return nil
	}

	bucket := newReadWriteBucket(tx, nil)
	return bucket.NestedReadWriteBucket(key)
}

// CreateTopLevelBucket creates the top level bucket for a key if it
// does not exist.  The newly-created bucket it returned.
func (tx *readWriteTx) CreateTopLevelBucket(key []byte) (
	walletdb.ReadWriteBucket, error) {

	if len(key) == 0 {
		return nil, walletdb.ErrBucketNameRequired
	}

	bucket := newReadWriteBucket(tx, nil)
	return bucket.CreateBucketIfNotExists(key)
}

// DeleteTopLevelBucket deletes the top level bucket for a key.  This
// errors if the bucket can not be found or the key keys a single value
// instead of a bucket.
func (tx *readWriteTx) DeleteTopLevelBucket(key []byte) error {
	// Execute a cascading delete on the key.
	result, err := tx.Exec(
		"DELETE FROM "+tx.db.table+" WHERE key=$1 "+
			"AND parent_id IS NULL",
		key,
	)
	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return walletdb.ErrBucketNotFound
	}

	return nil
}

// Commit commits the transaction if not already committed.
func (tx *readWriteTx) Commit() error {
	// Commit will fail if the transaction is already committed.
	if !tx.active {
		return walletdb.ErrTxClosed
	}

	// Try committing the transaction.
	err := tx.tx.Commit()
	if err == nil && tx.onCommit != nil {
		tx.onCommit()
	}

	// Unlock the transaction regardless of the error result.
	tx.active = false
	tx.locker.Unlock()

	return err
}

// OnCommit sets the commit callback (overriding if already set).
func (tx *readWriteTx) OnCommit(cb func()) {
	tx.onCommit = cb
}

// QueryRow executes a QueryRow call with a timeout context.
func (tx *readWriteTx) QueryRow(query string, args ...interface{}) (*sql.Row,
	func()) {

	ctx, cancel := tx.db.getTimeoutCtx()
	return tx.tx.QueryRowContext(ctx, query, args...), cancel
}

// Query executes a multi-row query call with a timeout context.
func (tx *readWriteTx) Query(query string, args ...interface{}) (*sql.Rows,
	func(), error) {

	ctx, cancel := tx.db.getTimeoutCtx()
	rows, err := tx.tx.QueryContext(ctx, query, args...)
	if err != nil {
		cancel()

		return nil, func() {}, err
	}

	return rows, cancel, nil
}

// Exec executes a Exec call with a timeout context.
func (tx *readWriteTx) Exec(query string, args ...interface{}) (sql.Result,
	error) {

	ctx, cancel := tx.db.getTimeoutCtx()
	defer cancel()

	return tx.tx.ExecContext(ctx, query, args...)
}

// noopLocker is an implementation of a no-op sync.Locker.
type noopLocker struct{}

// newNoopLocker creates a new noopLocker.
func newNoopLocker() sync.Locker {
	return &noopLocker{}
}

// Lock is a noop.
//
// NOTE: this is part of the sync.Locker interface.
func (n *noopLocker) Lock() {
}

// Unlock is a noop.
//
// NOTE: this is part of the sync.Locker interface.
func (n *noopLocker) Unlock() {
}

var _ sync.Locker = (*noopLocker)(nil)
