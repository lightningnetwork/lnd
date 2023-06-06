package sqldb

import (
	"context"
	"database/sql"
	prand "math/rand"
	"time"

	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

var (
	// DefaultStoreTimeout is the default timeout used for any interaction
	// with the storage/database.
	DefaultStoreTimeout = time.Second * 10
)

const (
	// DefaultNumTxRetries is the default number of times we'll retry a
	// transaction if it fails with an error that permits transaction
	// repetition.
	DefaultNumTxRetries = 10

	// DefaultRetryDelay is the default delay between retries. This will be
	// used to generate a random delay between 0 and this value.
	DefaultRetryDelay = time.Millisecond * 50
)

// TxOptions represents a set of options one can use to control what type of
// database transaction is created. Transaction can wither be read or write.
type TxOptions interface {
	// ReadOnly returns true if the transaction should be read only.
	ReadOnly() bool
}

// BatchedTx is a generic interface that represents the ability to execute
// several operations to a given storage interface in a single atomic
// transaction. Typically, Q here will be some subset of the main sqlc.Querier
// interface allowing it to only depend on the routines it needs to implement
// any additional business logic.
type BatchedTx[Q any] interface {
	// ExecTx will execute the passed txBody, operating upon generic
	// parameter Q (usually a storage interface) in a single transaction.
	//
	// The set of TxOptions are passed in order to allow the caller to
	// specify if a transaction should be read-only and optionally what
	// type of concurrency control should be used.
	ExecTx(ctx context.Context, txOptions TxOptions,
		txBody func(Q) error) error
}

// Tx represents a database transaction that can be committed or rolled back.
type Tx interface {
	// Commit commits the database transaction, an error should be returned
	// if the commit isn't possible.
	Commit() error

	// Rollback rolls back an incomplete database transaction.
	// Transactions that were able to be committed can still call this as a
	// noop.
	Rollback() error
}

// QueryCreator is a generic function that's used to create a Querier, which is
// a type of interface that implements storage related methods from a database
// transaction. This will be used to instantiate an object callers can use to
// apply multiple modifications to an object interface in a single atomic
// transaction.
type QueryCreator[Q any] func(*sql.Tx) Q

// BatchedQuerier is a generic interface that allows callers to create a new
// database transaction based on an abstract type that implements the TxOptions
// interface.
type BatchedQuerier interface {
	// Querier is the underlying query source, this is in place so we can
	// pass a BatchedQuerier implementation directly into objects that
	// create a batched version of the normal methods they need.
	sqlc.Querier

	// BeginTx creates a new database transaction given the set of
	// transaction options.
	BeginTx(ctx context.Context, options TxOptions) (*sql.Tx, error)
}

// txExecutorOptions is a struct that holds the options for the transaction
// executor. This can be used to do things like retry a transaction due to an
// error a certain amount of times.
type txExecutorOptions struct {
	numRetries int
	retryDelay time.Duration
}

// defaultTxExecutorOptions returns the default options for the transaction
// executor.
func defaultTxExecutorOptions() *txExecutorOptions {
	return &txExecutorOptions{
		numRetries: DefaultNumTxRetries,
		retryDelay: DefaultRetryDelay,
	}
}

// randRetryDelay returns a random retry delay between 0 and the configured max
// delay.
func (t *txExecutorOptions) randRetryDelay() time.Duration {
	return time.Duration(prand.Int63n(int64(t.retryDelay))) //nolint:gosec
}

// TxExecutorOption is a functional option that allows us to pass in optional
// argument when creating the executor.
type TxExecutorOption func(*txExecutorOptions)

// WithTxRetries is a functional option that allows us to specify the number of
// times a transaction should be retried if it fails with a repeatable error.
func WithTxRetries(numRetries int) TxExecutorOption {
	return func(o *txExecutorOptions) {
		o.numRetries = numRetries
	}
}

// WithTxRetryDelay is a functional option that allows us to specify the delay
// to wait before a transaction is retried.
func WithTxRetryDelay(delay time.Duration) TxExecutorOption {
	return func(o *txExecutorOptions) {
		o.retryDelay = delay
	}
}

// TransactionExecutor is a generic struct that abstracts away from the type of
// query a type needs to run under a database transaction, and also the set of
// options for that transaction. The QueryCreator is used to create a query
// given a database transaction created by the BatchedQuerier.
type TransactionExecutor[Query any] struct {
	BatchedQuerier

	createQuery QueryCreator[Query]

	opts *txExecutorOptions
}

// NewTransactionExecutor creates a new instance of a TransactionExecutor given
// a Querier query object and a concrete type for the type of transactions the
// Querier understands.
func NewTransactionExecutor[Querier any](db BatchedQuerier,
	createQuery QueryCreator[Querier],
	opts ...TxExecutorOption) *TransactionExecutor[Querier] {

	txOpts := defaultTxExecutorOptions()
	for _, optFunc := range opts {
		optFunc(txOpts)
	}

	return &TransactionExecutor[Querier]{
		BatchedQuerier: db,
		createQuery:    createQuery,
		opts:           txOpts,
	}
}

// ExecTx is a wrapper for txBody to abstract the creation and commit of a db
// transaction. The db transaction is embedded in a `*Queries` that txBody
// needs to use when executing each one of the queries that need to be applied
// atomically. This can be used by other storage interfaces to parameterize the
// type of query and options run, in order to have access to batched operations
// related to a storage object.
func (t *TransactionExecutor[Q]) ExecTx(ctx context.Context,
	txOptions TxOptions, txBody func(Q) error) error {

	waitBeforeRetry := func(attemptNumber int) {
		retryDelay := t.opts.randRetryDelay()

		log.Tracef("Retrying transaction due to tx serialization "+
			"error, attempt_number=%v, delay=%v", attemptNumber,
			retryDelay)

		// Before we try again, we'll wait with a random backoff based
		// on the retry delay.
		time.Sleep(retryDelay)
	}

	for i := 0; i < t.opts.numRetries; i++ {
		// Create the db transaction.
		tx, err := t.BatchedQuerier.BeginTx(ctx, txOptions)
		if err != nil {
			dbErr := MapSQLError(err)
			if IsSerializationError(dbErr) {
				// Nothing to roll back here, since we didn't
				// even get a transaction yet.
				waitBeforeRetry(i)

				continue
			}

			return dbErr
		}

		// Rollback is safe to call even if the tx is already closed,
		// so if the tx commits successfully, this is a no-op.
		defer func() {
			_ = tx.Rollback()
		}()

		if err := txBody(t.createQuery(tx)); err != nil {
			dbErr := MapSQLError(err)
			if IsSerializationError(dbErr) {
				// Roll back the transaction, then pop back up
				// to try once again.
				_ = tx.Rollback()

				waitBeforeRetry(i)

				continue
			}

			return dbErr
		}

		// Commit transaction.
		if err = tx.Commit(); err != nil {
			dbErr := MapSQLError(err)
			if IsSerializationError(dbErr) {
				// Roll back the transaction, then pop back up
				// to try once again.
				_ = tx.Rollback()

				waitBeforeRetry(i)

				continue
			}

			return dbErr
		}

		return nil
	}

	// If we get to this point, then we weren't able to successfully commit
	// a tx given the max number of retries.
	return ErrRetriesExceeded
}

// BaseDB is the base database struct that each implementation can embed to
// gain some common functionality.
type BaseDB struct {
	*sql.DB

	*sqlc.Queries
}

// BeginTx wraps the normal sql specific BeginTx method with the TxOptions
// interface. This interface is then mapped to the concrete sql tx options
// struct.
func (s *BaseDB) BeginTx(ctx context.Context, opts TxOptions) (*sql.Tx, error) {
	sqlOptions := sql.TxOptions{
		ReadOnly: opts.ReadOnly(),
	}

	return s.DB.BeginTx(ctx, &sqlOptions)
}
