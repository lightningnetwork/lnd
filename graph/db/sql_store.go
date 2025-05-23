package graphdb

import (
	"fmt"
	"sync"

	"github.com/lightningnetwork/lnd/batch"
	"github.com/lightningnetwork/lnd/sqldb"
)

// SQLQueries is a subset of the sqlc.Querier interface that can be used to
// execute queries against the SQL graph tables.
type SQLQueries interface {
}

// BatchedSQLQueries is a version of SQLQueries that's capable of batched
// database operations.
type BatchedSQLQueries interface {
	SQLQueries
	sqldb.BatchedTx[SQLQueries]
}

// SQLStore is an implementation of the V1Store interface that uses a SQL
// database as the backend.
//
// NOTE: currently, this temporarily embeds the KVStore struct so that we can
// implement the V1Store interface incrementally. For any method not
// implemented,  things will fall back to the KVStore. This is ONLY the case
// for the time being while this struct is purely used in unit tests only.
type SQLStore struct {
	db BatchedSQLQueries

	// cacheMu guards all caches (rejectCache and chanCache). If
	// this mutex will be acquired at the same time as the DB mutex then
	// the cacheMu MUST be acquired first to prevent deadlock.
	cacheMu     sync.RWMutex
	rejectCache *rejectCache
	chanCache   *channelCache

	chanScheduler batch.Scheduler[SQLQueries]
	nodeScheduler batch.Scheduler[SQLQueries]

	// Temporary fall-back to the KVStore so that we can implement the
	// interface incrementally.
	*KVStore
}

// A compile-time assertion to ensure that SQLStore implements the V1Store
// interface.
var _ V1Store = (*SQLStore)(nil)

// NewSQLStore creates a new SQLStore instance given an open BatchedSQLQueries
// storage backend.
func NewSQLStore(db BatchedSQLQueries, kvStore *KVStore,
	options ...StoreOptionModifier) (*SQLStore, error) {

	opts := DefaultOptions()
	for _, o := range options {
		o(opts)
	}

	if opts.NoMigration {
		return nil, fmt.Errorf("the NoMigration option is not yet " +
			"supported for SQL stores")
	}

	s := &SQLStore{
		db:          db,
		KVStore:     kvStore,
		rejectCache: newRejectCache(opts.RejectCacheSize),
		chanCache:   newChannelCache(opts.ChannelCacheSize),
	}

	s.chanScheduler = batch.NewTimeScheduler(
		db, &s.cacheMu, opts.BatchCommitInterval,
	)
	s.nodeScheduler = batch.NewTimeScheduler(
		db, nil, opts.BatchCommitInterval,
	)

	return s, nil
}
