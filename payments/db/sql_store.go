package paymentsdb

import (
	"context"
	"fmt"

	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

// SQLQueries is a subset of the sqlc.Querier interface that can be used to
// execute queries against the SQL payments tables.
type SQLQueries interface {
	/*
		Payment DB read operations.
	*/
	FilterPayments(ctx context.Context, query sqlc.FilterPaymentsParams) ([]sqlc.FilterPaymentsRow, error)
	FetchPayment(ctx context.Context, paymentIdentifier []byte) (sqlc.FetchPaymentRow, error)
	FetchPaymentsByIDs(ctx context.Context, paymentIDs []int64) ([]sqlc.FetchPaymentsByIDsRow, error)

	CountPayments(ctx context.Context) (int64, error)

	FetchHtlcAttemptsForPayments(ctx context.Context, paymentIDs []int64) ([]sqlc.FetchHtlcAttemptsForPaymentsRow, error)
	FetchAllInflightAttempts(ctx context.Context) ([]sqlc.PaymentHtlcAttempt, error)
	FetchHopsForAttempts(ctx context.Context, htlcAttemptIndices []int64) ([]sqlc.FetchHopsForAttemptsRow, error)

	FetchPaymentLevelFirstHopCustomRecords(ctx context.Context, paymentIDs []int64) ([]sqlc.PaymentFirstHopCustomRecord, error)
	FetchRouteLevelFirstHopCustomRecords(ctx context.Context, htlcAttemptIndices []int64) ([]sqlc.PaymentAttemptFirstHopCustomRecord, error)
	FetchHopLevelCustomRecords(ctx context.Context, hopIDs []int64) ([]sqlc.PaymentHopCustomRecord, error)
}

// BatchedSQLQueries is a version of the SQLQueries that's capable
// of batched database operations.
type BatchedSQLQueries interface {
	SQLQueries
	sqldb.BatchedTx[SQLQueries]
}

// SQLStore represents a storage backend.
type SQLStore struct {
	// TODO(ziggie): Remove the KVStore once all the interface functions are
	// implemented.
	KVStore

	cfg *SQLStoreConfig
	db  BatchedSQLQueries

	// keepFailedPaymentAttempts is a flag that indicates whether we should
	// keep failed payment attempts in the database.
	keepFailedPaymentAttempts bool
}

// A compile-time constraint to ensure SQLStore implements DB.
var _ DB = (*SQLStore)(nil)

// SQLStoreConfig holds the configuration for the SQLStore.
type SQLStoreConfig struct {
	// QueryConfig holds configuration values for SQL queries.
	QueryCfg *sqldb.QueryConfig
}

// NewSQLStore creates a new SQLStore instance given an open
// BatchedSQLPaymentsQueries storage backend.
func NewSQLStore(cfg *SQLStoreConfig, db BatchedSQLQueries,
	options ...OptionModifier) (*SQLStore, error) {

	opts := DefaultOptions()
	for _, applyOption := range options {
		applyOption(opts)
	}

	if opts.NoMigration {
		return nil, fmt.Errorf("the NoMigration option is not yet " +
			"supported for SQL stores")
	}

	return &SQLStore{
		cfg:                       cfg,
		db:                        db,
		keepFailedPaymentAttempts: opts.KeepFailedPaymentAttempts,
	}, nil
}

// A compile-time constraint to ensure SQLStore implements DB.
var _ DB = (*SQLStore)(nil)
