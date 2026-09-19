//go:build kvdb_postgres

package sqlbase

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/stretchr/testify/require"
)

const (
	// numRacers is the number of transactions that concurrently attempt to
	// create the same bucket.
	numRacers = 8

	// uniqueViolationCode is the SQLSTATE of a unique constraint violation,
	// which the transaction retry loop does not retry.
	uniqueViolationCode = "SQLSTATE 23505"

	// serializationFailureCode is the SQLSTATE of a serialization failure,
	// which the transaction retry loop does retry.
	serializationFailureCode = "SQLSTATE 40001"
)

// TestPostgresConcurrentBucketCreation asserts that transactions racing to
// create the very same bucket all end up succeeding, with only a single row
// created for the bucket. The bucket creation path used to do a select followed
// by a bare insert, which leaves the loser of such a race with a unique
// constraint violation. That error is not retried by the transaction retry
// loop, so it would surface as a hard failure to the caller.
func TestPostgresConcurrentBucketCreation(t *testing.T) {
	backend := newPostgresTestBackend(t)

	// raceCreate runs the given bucket creation function in numRacers
	// concurrent transactions, none of which start creating before every
	// last one of them has its transaction open. It returns the error of
	// each of the transactions.
	raceCreate := func(create func(walletdb.ReadWriteTx) error) []error {
		var (
			ready   sync.WaitGroup
			arrived = make([]sync.Once, numRacers)
			errs    = make([]error, numRacers)
			wg      sync.WaitGroup
		)
		ready.Add(numRacers)

		for i := 0; i < numRacers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()

				errs[i] = backend.Update(
					func(tx walletdb.ReadWriteTx) error {
						// Only stall on the very first
						// attempt, a retry must not
						// wait for the others.
						arrived[i].Do(ready.Done)
						ready.Wait()

						return create(tx)
					}, func() {},
				)
			}(i)
		}

		wg.Wait()

		return errs
	}

	// countRows returns the number of rows with the given key that either
	// are or aren't top level rows.
	countRows := func(t *testing.T, key string, topLevel bool) int {
		conn, err := sql.Open(
			postgresDriverName,
			fmt.Sprintf(testPgDsnTemplate, testPgPort),
		)
		require.NoError(t, err)
		defer conn.Close()

		parent := "parent_id IS NOT NULL"
		if topLevel {
			parent = "parent_id IS NULL"
		}

		var count int
		row := conn.QueryRow(
			"SELECT count(*) FROM "+backend.table+" WHERE key=$1 "+
				"AND "+parent, key,
		)
		require.NoError(t, row.Scan(&count))

		return count
	}

	// A racing CreateBucketIfNotExists must succeed everywhere, both for
	// the top level bucket and for the nested one.
	t.Run("create bucket if not exists", func(t *testing.T) {
		top, nested := "apple", "banana"

		errs := raceCreate(func(tx walletdb.ReadWriteTx) error {
			bkt, err := tx.CreateTopLevelBucket([]byte(top))
			if err != nil {
				return err
			}

			_, err = bkt.CreateBucketIfNotExists([]byte(nested))

			return err
		})

		for _, err := range errs {
			require.NoError(t, err)
		}

		require.Equal(t, 1, countRows(t, top, true))
		require.Equal(t, 1, countRows(t, nested, false))
	})

	// A racing CreateBucket must hand the bucket to exactly one caller and
	// report ErrBucketExists to all the others. Crucially, the losers must
	// not see a unique constraint violation.
	t.Run("create bucket", func(t *testing.T) {
		top, nested := "cherry", "date"

		errs := raceCreate(func(tx walletdb.ReadWriteTx) error {
			bkt, err := tx.CreateTopLevelBucket([]byte(top))
			if err != nil {
				return err
			}

			_, err = bkt.CreateBucket([]byte(nested))

			return err
		})

		var created int
		for _, err := range errs {
			if err == nil {
				created++

				continue
			}

			require.ErrorIs(t, err, walletdb.ErrBucketExists)
		}

		require.Equal(t, 1, created)
		require.Equal(t, 1, countRows(t, top, true))
		require.Equal(t, 1, countRows(t, nested, false))
	})
}

// TestPostgresBucketCreationConflict asserts that the statement used to create
// a bucket reports a concurrent creation of that same bucket as a retryable
// serialization failure and not as a unique constraint violation, at both of
// the isolation levels that write transactions may be run at. The bare insert
// that this statement replaced is exercised alongside it to show why it can't
// be used once write transactions move to repeatable read.
func TestPostgresBucketCreationConflict(t *testing.T) {
	backend := newPostgresTestBackend(t)

	conn, err := sql.Open(
		postgresDriverName, fmt.Sprintf(testPgDsnTemplate, testPgPort),
	)
	require.NoError(t, err)
	defer conn.Close()

	ctx := context.Background()
	table := backend.table

	// Create a bucket that the nested test cases can be parented to.
	var parentID int64
	row := conn.QueryRowContext(
		ctx, "INSERT INTO "+table+" (key) VALUES('parent') "+
			"RETURNING id",
	)
	require.NoError(t, row.Scan(&parentID))

	// race opens two transactions at the given isolation level, has both of
	// them take their snapshot, and then has both of them run the given
	// statement for the same key. The error of the transaction that loses
	// the race is returned.
	race := func(t *testing.T, level sql.IsolationLevel, stmt string,
		args ...interface{}) error {

		opts := &sql.TxOptions{Isolation: level}

		tx1, err := conn.BeginTx(ctx, opts)
		require.NoError(t, err)
		defer tx1.Rollback() //nolint:errcheck

		tx2, err := conn.BeginTx(ctx, opts)
		require.NoError(t, err)
		defer tx2.Rollback() //nolint:errcheck

		// A snapshot is only taken once the first statement of a
		// transaction runs, so we make sure that both transactions have
		// one that predates the inserts below.
		var count int
		for _, tx := range []*sql.Tx{tx1, tx2} {
			row := tx.QueryRowContext(
				ctx, "SELECT count(*) FROM "+table+
					" WHERE key=$1", args[0],
			)
			require.NoError(t, row.Scan(&count))
			require.Zero(t, count)
		}

		_, err = tx1.ExecContext(ctx, stmt, args...)
		require.NoError(t, err)

		// The second transaction blocks on the first one until it
		// commits, so it has to be run separately.
		loser := make(chan error, 1)
		go func() {
			_, err := tx2.ExecContext(ctx, stmt, args...)
			loser <- err
		}()

		select {
		case err := <-loser:
			t.Fatalf("second insert did not block: %v", err)

		case <-time.After(250 * time.Millisecond):
		}

		require.NoError(t, tx1.Commit())

		select {
		case err := <-loser:
			require.Error(t, err)

			return err

		case <-time.After(time.Minute):
			t.Fatal("second insert never returned")

			return nil
		}
	}

	levels := []struct {
		name  string
		level sql.IsolationLevel
	}{
		{
			name:  "serializable",
			level: sql.LevelSerializable,
		},
		{
			name:  "repeatable_read",
			level: sql.LevelRepeatableRead,
		},
	}

	// The statements below are the top level and the nested flavour of both
	// the old and the new bucket creation statement. Each has to line up
	// with the partial unique index that covers the row it inserts:
	// <table>_unp for top level rows and <table>_up for nested ones.
	stmts := []struct {
		name   string
		legacy bool
		stmt   string
		args   []interface{}
	}{
		{
			name: "upsert_top_level",
			stmt: "INSERT INTO " + table + " (key) VALUES($1) " +
				"ON CONFLICT (key) WHERE parent_id IS NULL " +
				"DO UPDATE SET key=$1 RETURNING id, value",
		},
		{
			name: "upsert_nested",
			stmt: "INSERT INTO " + table + " (key, parent_id) " +
				"VALUES($1, $2) ON CONFLICT (key, parent_id) " +
				"WHERE parent_id IS NOT NULL " +
				"DO UPDATE SET key=$1 RETURNING id, value",
			args: []interface{}{parentID},
		},
		{
			name:   "legacy_top_level",
			legacy: true,
			stmt: "INSERT INTO " + table + " (parent_id, key) " +
				"VALUES(NULL, $1) RETURNING id",
		},
		{
			name:   "legacy_nested",
			legacy: true,
			stmt: "INSERT INTO " + table + " (parent_id, key) " +
				"VALUES($2, $1) RETURNING id",
			args: []interface{}{parentID},
		},
	}

	for _, level := range levels {
		for _, stmt := range stmts {
			name := level.name + "/" + stmt.name
			t.Run(name, func(t *testing.T) {
				args := append(
					[]interface{}{"key-" + name},
					stmt.args...,
				)
				err := race(t, level.level, stmt.stmt, args...)

				// The bare insert is only kept around to
				// demonstrate the failure mode that the upsert
				// avoids under snapshot isolation.
				if stmt.legacy &&
					level.level == sql.LevelRepeatableRead {

					require.Contains(
						t, err.Error(),
						uniqueViolationCode,
					)

					return
				}

				// Everything else must fail in a way that the
				// transaction retry loop knows how to handle.
				require.Contains(
					t, err.Error(), serializationFailureCode,
				)

				var serErr *sqldb.ErrSerializationError
				require.ErrorAs(
					t, sqldb.MapSQLError(err), &serErr,
					"want serialization failure, got %v",
					err,
				)
			})
		}
	}
}
