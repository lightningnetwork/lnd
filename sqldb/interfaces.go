package sqldb

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/rand"
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
	DefaultNumTxRetries = 20

	// DefaultRetryDelay is the default delay between retries. This will be
	// used to generate a random delay between 0 and this value.
	DefaultRetryDelay = time.Millisecond * 50

	// DefaultMaxRetryDelay is the default maximum delay between retries.
	DefaultMaxRetryDelay = time.Second
)

// TxOptions represents a set of options one can use to control what type of
// database transaction is created. Transaction can be either read or write.
type TxOptions interface {
	// ReadOnly returns true if the transaction should be read only.
	ReadOnly() bool
}

// txOptions is a concrete implementation of the TxOptions interface.
type txOptions struct {
	// readOnly indicates if the transaction should be read-only.
	readOnly bool
}

// ReadOnly returns true if the transaction should be read only.
//
// NOTE: This is part of the TxOptions interface.
func (t *txOptions) ReadOnly() bool {
	return t.readOnly
}

// WriteTxOpt returns a TxOptions that indicates that the transaction
// should be a write transaction.
func WriteTxOpt() TxOptions {
	return &txOptions{
		readOnly: false,
	}
}

// ReadTxOpt returns a TxOptions that indicates that the transaction
// should be a read-only transaction.
func ReadTxOpt() TxOptions {
	return &txOptions{
		readOnly: true,
	}
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
		txBody func(Q) error, reset func()) error
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
	numRetries  int
	retryDelay  time.Duration
	retryBudget time.Duration
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

// WithTxRetryBudget is a functional option that allows us to bound the total
// amount of wall clock time that may be spent retrying a single transaction. A
// value of zero, which is the default, means that only the retry count bounds
// the retry loop.
func WithTxRetryBudget(budget time.Duration) TxExecutorOption {
	return func(o *txExecutorOptions) {
		o.retryBudget = budget
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

// randRetryDelay returns a random retry delay between -50% and +50% of the
// configured delay that is doubled for each attempt and capped at a max value.
func randRetryDelay(initialRetryDelay, maxRetryDelay time.Duration,
	attempt int) time.Duration {

	halfDelay := initialRetryDelay / 2
	randDelay := rand.Int63n(int64(initialRetryDelay)) //nolint:gosec

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
	factor := time.Duration(math.Pow(2, min(float64(attempt), 32)))
	actualDelay := initialDelay * factor

	// Cap the delay at the maximum configured value.
	if actualDelay > maxRetryDelay {
		return maxRetryDelay
	}

	return actualDelay
}

// MakeTx is a function that creates a new transaction. It returns a Tx and an
// error if the transaction cannot be created. This is used to abstract the
// creation of a transaction from the actual transaction logic in order to be
// able to reuse the transaction retry logic in other packages.
type MakeTx func() (Tx, error)

// TxBody represents the function type for transactions. It returns an
// error to indicate success or failure.
type TxBody func(tx Tx) error

// RollbackTx is a function that is called when a transaction needs to be rolled
// back due to a serialization error. By using this intermediate function, we
// can avoid having to return rollback errors that are not actionable by the
// caller.
type RollbackTx func(tx Tx) error

// OnBackoff is a function that is called when a transaction is retried due to a
// serialization error. The function is called with the retry attempt number and
// the delay before the next retry.
type OnBackoff func(retry int, delay time.Duration)

// RetryConfig bounds how long the transaction retry loop is willing to keep
// retrying a transaction that repeatedly fails with a serialization error. Two
// independent budgets are available, and the loop stops as soon as either of
// them is exhausted, as soon as the context of the caller is canceled, or as
// soon as the Quit channel below is closed.
type RetryConfig struct {
	// MaxRetries is the maximum number of times a transaction will be
	// attempted before we give up. A value of zero means that the number of
	// attempts is unbounded, in which case only MaxElapsed, the context and
	// Quit bound the loop.
	MaxRetries int

	// MaxElapsed is the maximum amount of wall clock time that may be spent
	// across all attempts of a single transaction. A value of zero means
	// that the elapsed time is unbounded, in which case only MaxRetries, the
	// context and Quit bound the loop.
	MaxElapsed time.Duration

	// Quit is an optional channel that aborts the retry loop as soon as it
	// is closed. Callers that allow a generous time budget should wire this
	// to their shutdown signal, so that a transaction that keeps conflicting
	// can never hold up shutdown for the length of the budget.
	//
	// NOTE: A nil channel blocks forever in a select, so leaving this unset
	// simply means that only the budgets and the context bound the loop.
	Quit <-chan struct{}
}

// NumRetriesConfig returns a retry budget that is bounded by the given number
// of attempts only.
func NumRetriesConfig(numRetries int) RetryConfig {
	return RetryConfig{
		MaxRetries: numRetries,
	}
}

// exhausted returns true if either of the configured budgets has been used up
// after the given number of attempts and elapsed wall clock time.
func (r RetryConfig) exhausted(attempts int, elapsed time.Duration) bool {
	// If neither budget was set, then we fall back to the default attempt
	// count. We do this so that a zero value config can never put us into a
	// truly unbounded loop by accident.
	if r.MaxRetries <= 0 && r.MaxElapsed <= 0 {
		return attempts >= DefaultNumTxRetries
	}

	if r.MaxRetries > 0 && attempts >= r.MaxRetries {
		return true
	}

	if r.MaxElapsed > 0 && elapsed >= r.MaxElapsed {
		return true
	}

	return false
}

// ExecuteSQLTransactionWithRetry is a helper function that executes a
// transaction with retry logic. It will retry the transaction if it fails with
// a serialization error. The function will return an error if the transaction
// fails with a non-retryable error, the context is cancelled or the number of
// retries is exceeded.
//
// NOTE: This is retained for backwards compatibility with the callers that
// predate RetryConfig. New code should call
// ExecuteSQLTransactionWithRetryConfig directly, which also allows bounding the
// retries by elapsed time and aborting them on shutdown.
func ExecuteSQLTransactionWithRetry(ctx context.Context, makeTx MakeTx,
	rollbackTx RollbackTx, txBody TxBody, onBackoff OnBackoff,
	numRetries int) error {

	return ExecuteSQLTransactionWithRetryConfig(
		ctx, makeTx, rollbackTx, txBody, onBackoff,
		NumRetriesConfig(numRetries),
	)
}

// ExecuteSQLTransactionWithRetryConfig is a helper function that executes a
// transaction with retry logic. It will retry the transaction if it fails with
// a serialization error. The function will return an error if the transaction
// fails with a non-retryable error, the context is cancelled, the quit channel
// is closed or the retry budget is exhausted.
func ExecuteSQLTransactionWithRetryConfig(ctx context.Context, makeTx MakeTx,
	rollbackTx RollbackTx, txBody TxBody, onBackoff OnBackoff,
	retryCfg RetryConfig) error {

	// waitBeforeRetry blocks for a randomized, exponentially increasing
	// backoff before the next attempt is made. It returns a non-nil error
	// if we should stop retrying, either because the context of the caller
	// was canceled or because we were asked to quit while we were waiting.
	waitBeforeRetry := func(attemptNumber int, dbErr error) error {
		retryDelay := randRetryDelay(
			DefaultRetryDelay, DefaultMaxRetryDelay, attemptNumber,
		)

		onBackoff(attemptNumber, retryDelay)

		select {
		// Before we try again, we'll wait with a random backoff based
		// on the retry delay.
		case <-time.After(retryDelay):
			return nil

		// If the daemon is shutting down, then we'll exit early. We
		// label the error we return here, so that a caller can tell an
		// interrupted retry loop apart from a transaction that truly
		// failed. The original database error is still wrapped, so that
		// both errors.Is and errors.As keep working on the result.
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", ErrRetryCanceled, dbErr)

		// Same, but for callers whose shutdown signal is a plain
		// channel rather than a context.
		case <-retryCfg.Quit:
			return fmt.Errorf("%w: %w", ErrRetryCanceled, dbErr)
		}
	}

	// attemptTx runs a single attempt of the transaction. The first return
	// value is true if the attempt failed with a serialization error and is
	// therefore worth retrying, in which case the returned error is the
	// serialization error that caused the retry.
	attemptTx := func() (bool, error) {
		tx, err := makeTx()
		if err != nil {
			dbErr := MapSQLError(err)
			log.Tracef("Failed to makeTx: err=%v, dbErr=%v", err,
				dbErr)

			// Nothing to roll back here, since we haven't even got
			// a transaction yet.
			return IsSerializationError(dbErr), dbErr
		}

		// Rollback is safe to call even if the tx is already closed,
		// so if the tx commits successfully, this is a no-op.
		defer func() {
			_ = tx.Rollback()
		}()

		if bodyErr := txBody(tx); bodyErr != nil {
			log.Tracef("Error in txBody: %v", bodyErr)

			// Roll back the transaction, then signal a retry if the
			// error was a serialization error.
			if err := rollbackTx(tx); err != nil {
				return false, MapSQLError(err)
			}

			dbErr := MapSQLError(bodyErr)

			return IsSerializationError(dbErr), dbErr
		}

		// Commit transaction.
		if commitErr := tx.Commit(); commitErr != nil {
			log.Tracef("Failed to commit tx: %v", commitErr)

			// Roll back the transaction, then signal a retry if the
			// error was a serialization error.
			if err := rollbackTx(tx); err != nil {
				return false, MapSQLError(err)
			}

			dbErr := MapSQLError(commitErr)

			return IsSerializationError(dbErr), dbErr
		}

		return false, nil
	}

	var (
		startTime = time.Now()
		attempts  int
		lastErr   error
	)

	for {
		retry, err := attemptTx()
		if !retry {
			return err
		}

		lastErr = err
		attempts++

		// We check the budget before we back off, so that we never
		// sleep for an attempt that we're not going to make anyway.
		if retryCfg.exhausted(attempts, time.Since(startTime)) {
			break
		}

		if waitErr := waitBeforeRetry(attempts-1, err); waitErr != nil {
			return waitErr
		}
	}

	// If we get to this point, then we weren't able to successfully commit
	// a tx within the retry budget. We attach the last error we saw, so
	// that the actual reason for the failure isn't lost.
	//
	// NOTE: lastErr is always non-nil here, since we only ever leave the
	// loop above after an attempt that asked to be retried.
	return fmt.Errorf("%w (attempts=%v, elapsed=%v): %w",
		ErrRetriesExceeded, attempts, time.Since(startTime), lastErr)
}

// ExecTx is a wrapper for txBody to abstract the creation and commit of a db
// transaction. The db transaction is embedded in a `*Queries` that txBody
// needs to use when executing each one of the queries that need to be applied
// atomically. This can be used by other storage interfaces to parameterize the
// type of query and options run, in order to have access to batched operations
// related to a storage object.
func (t *TransactionExecutor[Q]) ExecTx(ctx context.Context,
	txOptions TxOptions, txBody func(Q) error, reset func()) error {

	makeTx := func() (Tx, error) {
		return t.BatchedQuerier.BeginTx(ctx, txOptions)
	}

	execTxBody := func(tx Tx) error {
		sqlTx, ok := tx.(*sql.Tx)
		if !ok {
			return fmt.Errorf("expected *sql.Tx, got %T", tx)
		}

		reset()
		return txBody(t.createQuery(sqlTx))
	}

	onBackoff := func(retry int, delay time.Duration) {
		log.Tracef("Retrying transaction due to tx serialization "+
			"error, attempt_number=%v, delay=%v", retry, delay)
	}

	rollbackTx := func(tx Tx) error {
		sqlTx, ok := tx.(*sql.Tx)
		if !ok {
			return fmt.Errorf("expected *sql.Tx, got %T", tx)
		}

		_ = sqlTx.Rollback()

		return nil
	}

	return ExecuteSQLTransactionWithRetryConfig(
		ctx, makeTx, rollbackTx, execTxBody, onBackoff, RetryConfig{
			MaxRetries: t.opts.numRetries,
			MaxElapsed: t.opts.retryBudget,
		},
	)
}

// DB is an interface that represents a generic SQL database. It provides
// methods to apply migrations and access the underlying database connection.
type DB interface {
	// GetBaseDB returns the underlying BaseDB instance.
	GetBaseDB() *BaseDB

	// ApplyAllMigrations applies all migrations to the database including
	// both sqlc and custom in-code migrations.
	ApplyAllMigrations(ctx context.Context,
		customMigrations []MigrationConfig) error
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
		Isolation: sql.LevelSerializable,
		ReadOnly:  opts.ReadOnly(),
	}

	return s.DB.BeginTx(ctx, &sqlOptions)
}
