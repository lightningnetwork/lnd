//go:build kvdb_postgres || (kvdb_sqlite && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64)))

package sqlbase

import (
	"context"

	"github.com/btcsuite/btcwallet/walletdb"
)

// MigrationBackend combines the regular walletdb database operations with the
// migration-only bulk capabilities guaranteed by a migration backend.
type MigrationBackend interface {
	walletdb.DB
	MigrationBulkKVStore
}

// MigrationBulkKVStore exposes migration-only helpers for loading and verifying
// the SQL KV schema directly. Normal application code should continue to use
// the walletdb/kvdb bucket APIs.
type MigrationBulkKVStore interface {
	// CheckEmpty returns whether the underlying KV table has no rows.
	CheckEmpty(ctx context.Context) (bool, error)

	// TruncateTargetTable unconditionally and irreversibly removes every
	// row from the underlying KV table. It is only intended for fresh-only
	// migration recovery when the caller owns the whole target table.
	TruncateTargetTable(ctx context.Context) error

	// BeginBulk opens a destination write transaction for bulk loading.
	// Callers must defer Rollback immediately after a successful open; the
	// rollback is a no-op after Commit and releases locks/connections on
	// all other exits.
	BeginBulk(ctx context.Context) (MigrationBulkKVTx, error)

	// BeginBulkVerify opens a read transaction for batched verification.
	// Callers must defer Rollback immediately after a successful open so
	// the read transaction lock is always released.
	BeginBulkVerify(ctx context.Context) (MigrationBulkKVVerifier, error)
}

// MigrationBulkLeaf is a leaf key/value row to be inserted under ParentID.
// ParentID must be a positive row ID returned by InsertBucket. Top-level leaves
// are not supported by the migration bulk API.
type MigrationBulkLeaf struct {
	ParentID int64

	// Key must be non-empty.
	Key   []byte
	Value []byte
}

// MigrationBulkKVTx is a migration-only transaction for bulk-loading SQL KV
// data.
type MigrationBulkKVTx interface {
	// InsertBucket inserts a bucket row with a non-empty key. It returns
	// the generated id. A nil parentID creates a top-level bucket.
	InsertBucket(ctx context.Context, parentID *int64, key []byte,
		seq uint64) (int64, error)

	// InsertLeaves inserts leaf rows with non-empty keys. Implementations
	// may choose COPY, multi-row INSERT, or another backend-specific
	// strategy.
	InsertLeaves(ctx context.Context, leaves []MigrationBulkLeaf) error

	// Commit atomically commits the bulk load transaction.
	Commit() error

	// Rollback aborts the bulk load transaction.
	Rollback() error
}

// MigrationBulkChild is a single SQL KV row returned by the verifier helpers.
type MigrationBulkChild struct {
	// ID is the SQL row id.
	ID int64

	// ParentID is nil for top-level rows.
	ParentID *int64

	// Key is the bucket key or leaf key.
	Key []byte

	// Value is the leaf value. It is nil for buckets.
	Value []byte

	// IsBucket identifies bucket rows explicitly. This avoids ambiguity
	// between SQL NULL bucket markers and empty leaf values decoded as nil.
	IsBucket bool

	// Sequence is the bucket sequence number. It is zero for unset
	// sequences and for leaf rows.
	Sequence uint64
}

// MigrationBulkKVVerifier reads SQL KV rows in batches for migration
// verification.
type MigrationBulkKVVerifier interface {
	// FetchTopLevel returns all top-level rows ordered by key.
	FetchTopLevel(ctx context.Context) ([]MigrationBulkChild, error)

	// FetchChildren returns direct children for the given bucket ids,
	// ordered by parent id and key.
	FetchChildren(ctx context.Context,
		parentIDs []int64) ([]MigrationBulkChild, error)

	// Rollback closes the verifier transaction.
	Rollback() error
}
