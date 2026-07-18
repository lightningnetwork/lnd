//go:build kvdb_postgres

package sqlbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// postgresDB adds Postgres-only capabilities to the shared SQL backend.
type postgresDB struct {
	*db
}

var (
	_ walletdb.DB          = (*postgresDB)(nil)
	_ MigrationBulkKVStore = (*postgresDB)(nil)
)

// bulkLeafCols is the leaf-row projection of the shared KV table schema
// defined in schema.go. The id column is database-generated. Sequence is
// walletdb bucket metadata copied separately by InsertBucket, so leaf rows do
// not include either column in the COPY operation.
var bulkLeafCols = []string{"parent_id", "key", "value"}

// NewPostgresBackend returns a shared SQL backend with Postgres-only
// capabilities, including migration bulk loading.
func NewPostgresBackend(ctx context.Context, cfg *Config) (
	MigrationBackend, error) {

	db, err := NewSqlBackend(ctx, cfg)
	if err != nil {
		return nil, err
	}

	return &postgresDB{db: db}, nil
}

// CheckEmpty returns whether the underlying KV table has no rows.
func (p *postgresDB) CheckEmpty(ctx context.Context) (bool, error) {
	locker := p.bulkLocker(true)
	locker.Lock()
	defer locker.Unlock()

	var count int64
	err := p.db.db.QueryRowContext(
		ctx, "SELECT COUNT(*) FROM "+p.table,
	).Scan(&count)
	if err != nil {
		return false, err
	}

	return count == 0, nil
}

// TruncateTargetTable unconditionally and irreversibly removes every row from
// the underlying KV table. It is only intended for fresh migration recovery
// where the caller owns the whole target table.
func (p *postgresDB) TruncateTargetTable(ctx context.Context) error {
	locker := p.bulkLocker(false)
	locker.Lock()
	defer locker.Unlock()

	_, err := p.db.db.ExecContext(ctx, "TRUNCATE TABLE "+p.table)

	return err
}

// BeginBulk opens a write transaction for bulk loading. It uses a dedicated
// *sql.Conn so InsertLeaves can reach the underlying pgx connection and COPY
// into the same transaction. Callers must defer Rollback immediately after a
// successful open so the lock and connection are released on all exits.
func (p *postgresDB) BeginBulk(ctx context.Context) (MigrationBulkKVTx, error) {
	locker := p.bulkLocker(false)
	locker.Lock()

	conn, err := p.db.db.Conn(ctx)
	if err != nil {
		locker.Unlock()
		return nil, err
	}

	// A bulk migration can touch millions of rows in a single transaction.
	// PostgreSQL retains predicate locks until a serializable transaction
	// ends, which can make its predicate lock table consume excessive memory.
	// Read committed is sufficient because the migration owns the empty
	// destination database while loading it.
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelReadCommitted,
	})
	if err != nil {
		locker.Unlock()
		_ = conn.Close()
		return nil, err
	}

	return &postgresBulkKVTx{
		db:     p.db,
		conn:   conn,
		tx:     tx,
		locker: locker,
		active: true,
	}, nil
}

// BeginBulkVerify opens a read-only transaction for batched verification.
// Callers must defer Rollback immediately after a successful open so the read
// transaction lock is always released.
func (p *postgresDB) BeginBulkVerify(
	ctx context.Context) (MigrationBulkKVVerifier, error) {

	locker := p.bulkLocker(true)
	locker.Lock()

	tx, err := p.db.db.BeginTx(ctx, &sql.TxOptions{
		ReadOnly:  true,
		Isolation: sql.LevelRepeatableRead,
	})
	if err != nil {
		locker.Unlock()
		return nil, err
	}

	return &postgresBulkKVVerifier{
		db:     p.db,
		tx:     tx,
		locker: locker,
		active: true,
	}, nil
}

// bulkLocker returns the same optional global lock used by regular sqlbase
// transactions so migration-only transactions respect WithTxLevelLock.
func (p *postgresDB) bulkLocker(readOnly bool) sync.Locker {
	if !p.cfg.WithTxLevelLock {
		return newNoopLocker()
	}
	if readOnly {
		return p.lock.RLocker()
	}

	return &p.lock
}

// postgresBulkKVTx is a migration-only Postgres transaction for loading the SQL
// KV table directly.
type postgresBulkKVTx struct {
	db     *db
	conn   *sql.Conn
	tx     *sql.Tx
	locker sync.Locker
	active bool
}

// InsertBucket inserts a bucket row and returns its generated id.
func (p *postgresBulkKVTx) InsertBucket(ctx context.Context,
	parentID *int64, key []byte, seq uint64) (int64, error) {

	if !p.active {
		return 0, walletdb.ErrTxClosed
	}
	if len(key) == 0 {
		return 0, walletdb.ErrBucketNameRequired
	}

	keyCopy := cloneBulkBytes(key)

	var id int64
	if seq != 0 {
		err := p.tx.QueryRowContext(
			ctx, "INSERT INTO "+p.db.table+
				" (parent_id, key, sequence) "+
				"VALUES ($1,$2,$3) RETURNING id",
			parentID, keyCopy, int64(seq),
		).Scan(&id)

		return id, err
	}

	err := p.tx.QueryRowContext(
		ctx, "INSERT INTO "+p.db.table+" (parent_id, key) "+
			"VALUES ($1,$2) RETURNING id",
		parentID, keyCopy,
	).Scan(&id)

	return id, err
}

// InsertLeaves inserts leaf rows with Postgres COPY.
func (p *postgresBulkKVTx) InsertLeaves(ctx context.Context,
	leaves []MigrationBulkLeaf) error {

	if !p.active {
		return walletdb.ErrTxClosed
	}
	if len(leaves) == 0 {
		return nil
	}

	rows := make([][]any, len(leaves))
	for i := range leaves {
		if leaves[i].ParentID <= 0 {
			return fmt.Errorf(
				"bulk leaf %d has invalid parent id %d", i,
				leaves[i].ParentID,
			)
		}
		if len(leaves[i].Key) == 0 {
			return fmt.Errorf(
				"bulk leaf %d: %w", i, walletdb.ErrKeyRequired,
			)
		}

		value := cloneBulkBytes(leaves[i].Value)
		if value == nil {
			value = []byte{}
		}

		rows[i] = []any{
			leaves[i].ParentID,
			cloneBulkBytes(leaves[i].Key),
			value,
		}
	}

	var copied int64
	err := p.conn.Raw(func(driverConn any) error {
		pgxConn, ok := driverConn.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("driver conn is %T, not "+
				"pgx/v5/stdlib.Conn", driverConn)
		}

		var copyErr error
		// The shared schema and normal SQL paths use unquoted
		// identifiers, which Postgres folds to lowercase. CopyFrom
		// quotes its identifier, so fold it explicitly to resolve the
		// same physical table.
		copied, copyErr = pgxConn.Conn().CopyFrom(
			ctx, pgx.Identifier{strings.ToLower(p.db.table)},
			bulkLeafCols,
			pgx.CopyFromRows(rows),
		)

		return copyErr
	})
	if err != nil {
		return err
	}
	if copied != int64(len(leaves)) {
		return fmt.Errorf("bulk leaf copy count mismatch: got=%d "+
			"want=%d", copied, len(leaves))
	}

	return nil
}

// Commit commits the bulk transaction and releases its dedicated connection.
func (p *postgresBulkKVTx) Commit() error {
	if !p.active {
		return walletdb.ErrTxClosed
	}

	err := p.tx.Commit()
	p.active = false
	p.locker.Unlock()
	closeErr := p.conn.Close()
	if err != nil {
		if closeErr != nil {
			log.Warnf(
				"Could not close bulk migration connection: %v",
				closeErr,
			)
		}

		return err
	}
	if closeErr != nil {
		log.Warnf("Could not close bulk migration connection after "+
			"commit: %v", closeErr)
	}

	return nil
}

// Rollback rolls back the bulk transaction and releases its dedicated
// connection. It is idempotent for already-closed transactions.
func (p *postgresBulkKVTx) Rollback() error {
	if !p.active {
		return nil
	}

	err := p.tx.Rollback()
	p.active = false
	p.locker.Unlock()
	closeErr := p.conn.Close()
	if err != nil && !errors.Is(err, sql.ErrTxDone) {
		return err
	}

	return closeErr
}

// postgresBulkKVVerifier is a read-only Postgres transaction for batched SQL KV
// verification.
type postgresBulkKVVerifier struct {
	db     *db
	tx     *sql.Tx
	locker sync.Locker
	active bool
}

// FetchTopLevel returns all top-level rows ordered by key.
func (p *postgresBulkKVVerifier) FetchTopLevel(
	ctx context.Context) ([]MigrationBulkChild, error) {

	if !p.active {
		return nil, walletdb.ErrTxClosed
	}

	// The table name is constructed internally from the configured prefix.
	//nolint:gosec
	rows, err := p.tx.QueryContext(ctx, "SELECT id, parent_id, key, "+
		"value, sequence, CASE WHEN value IS NULL THEN 1 ELSE 0 END "+
		"FROM "+p.db.table+" WHERE parent_id IS NULL ORDER BY key")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanBulkChildren(rows)
}

// FetchChildren returns direct children for parentIDs ordered by parent id and
// key.
func (p *postgresBulkKVVerifier) FetchChildren(ctx context.Context,
	parentIDs []int64) ([]MigrationBulkChild, error) {

	if !p.active {
		return nil, walletdb.ErrTxClosed
	}
	if len(parentIDs) == 0 {
		return nil, nil
	}

	// parentIDs is passed as a native []int64; the pgx stdlib driver
	// encodes it as a Postgres bigint array for the ANY($1) match.
	//
	// The table name is constructed internally from the configured prefix.
	//nolint:gosec
	rows, err := p.tx.QueryContext(ctx, "SELECT id, parent_id, key, "+
		"value, sequence, CASE WHEN value IS NULL THEN 1 ELSE 0 END "+
		"FROM "+p.db.table+" WHERE parent_id = ANY($1) "+
		"ORDER BY parent_id, key", parentIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanBulkChildren(rows)
}

// Rollback closes the verifier read transaction.
func (p *postgresBulkKVVerifier) Rollback() error {
	if !p.active {
		return nil
	}

	err := p.tx.Rollback()
	p.active = false
	p.locker.Unlock()
	if err != nil && !errors.Is(err, sql.ErrTxDone) {
		return err
	}

	return nil
}

// scanBulkChildren scans verifier rows and preserves an explicit IsBucket flag
// so empty leaf values are not confused with SQL NULL bucket markers.
func scanBulkChildren(rows *sql.Rows) ([]MigrationBulkChild, error) {
	var children []MigrationBulkChild
	for rows.Next() {
		var (
			child      MigrationBulkChild
			parentID   sql.NullInt64
			sequence   sql.NullInt64
			bucketFlag int
		)
		if err := rows.Scan(
			&child.ID, &parentID, &child.Key, &child.Value,
			&sequence, &bucketFlag,
		); err != nil {
			return nil, err
		}
		if parentID.Valid {
			id := parentID.Int64
			child.ParentID = &id
		}
		if sequence.Valid {
			child.Sequence = uint64(sequence.Int64)
		}
		child.IsBucket = bucketFlag == 1
		if !child.IsBucket && child.Value == nil {
			child.Value = []byte{}
		}

		children = append(children, child)
	}

	return children, rows.Err()
}

// cloneBulkBytes copies driver-owned byte slices before they are buffered or
// returned to callers.
func cloneBulkBytes(b []byte) []byte {
	if b == nil {
		return nil
	}

	out := make([]byte, len(b))
	copy(out, b)

	return out
}
