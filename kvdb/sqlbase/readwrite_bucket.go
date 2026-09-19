//go:build kvdb_postgres || (kvdb_sqlite && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64)))

package sqlbase

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/btcsuite/btcwallet/walletdb"
)

// readWriteBucket stores the bucket id and the buckets transaction.
type readWriteBucket struct {
	// id is used to identify the bucket. If id is null, it refers to the
	// root bucket.
	id *int64

	// tx holds the parent transaction.
	tx *readWriteTx

	table string
}

// newReadWriteBucket creates a new rw bucket with the passed transaction
// and bucket id.
func newReadWriteBucket(tx *readWriteTx, id *int64) *readWriteBucket {
	return &readWriteBucket{
		id:    id,
		tx:    tx,
		table: tx.db.table,
	}
}

// NestedReadBucket retrieves a nested read bucket with the given key.
// Returns nil if the bucket does not exist.
func (b *readWriteBucket) NestedReadBucket(key []byte) walletdb.ReadBucket {
	return b.NestedReadWriteBucket(key)
}

func parentSelector(id *int64) string {
	if id == nil {
		return "parent_id IS NULL"
	}
	return fmt.Sprintf("parent_id=%v", *id)
}

// ForEach invokes the passed function with every key/value pair in
// the bucket. This includes nested buckets, in which case the value
// is nil, but it does not include the key/value pairs within those
// nested buckets.
func (b *readWriteBucket) ForEach(cb func(k, v []byte) error) error {
	cursor := b.ReadWriteCursor()

	k, v := cursor.First()
	for k != nil {
		err := cb(k, v)
		if err != nil {
			return err
		}

		k, v = cursor.Next()
	}

	return nil
}

// Get returns the value for the given key. Returns nil if the key does
// not exist in this bucket.
func (b *readWriteBucket) Get(key []byte) []byte {
	// Return nil if the key is empty.
	if len(key) == 0 {
		return nil
	}

	var value *[]byte
	row, cancel := b.tx.QueryRow(
		"SELECT value FROM "+b.table+" WHERE "+parentSelector(b.id)+
			" AND key=$1", key,
	)
	defer cancel()
	err := row.Scan(&value)

	switch {
	case err == sql.ErrNoRows:
		return nil

	case err != nil:
		panic(err)
	}

	// When an empty byte array is stored as the value, Sqlite will decode
	// that into nil whereas postgres will decode that as an empty byte
	// array. Since returning nil is taken to mean that no value has ever
	// been written, we ensure here that we at least return an empty array
	// so that nil checks will fail.
	if len(*value) == 0 {
		return []byte{}
	}

	return *value
}

// ReadCursor returns a new read-only cursor for this bucket.
func (b *readWriteBucket) ReadCursor() walletdb.ReadCursor {
	return newReadWriteCursor(b)
}

// NestedReadWriteBucket retrieves a nested bucket with the given key.
// Returns nil if the bucket does not exist.
func (b *readWriteBucket) NestedReadWriteBucket(
	key []byte) walletdb.ReadWriteBucket {

	if len(key) == 0 {
		return nil
	}

	var id int64
	row, cancel := b.tx.QueryRow(
		"SELECT id FROM "+b.table+" WHERE "+parentSelector(b.id)+
			" AND key=$1 AND value IS NULL", key,
	)
	defer cancel()
	err := row.Scan(&id)

	switch {
	case err == sql.ErrNoRows:
		return nil

	case err != nil:
		panic(err)
	}

	return newReadWriteBucket(b.tx, &id)
}

// createBucket returns the id of the bucket with the given key, creating the
// bucket first if it doesn't exist yet. The first returned boolean signals
// whether the row was already there before this call, and the second signals
// whether that row holds a value instead of a bucket.
func (b *readWriteBucket) createBucket(key []byte) (int64, bool, bool, error) {
	// Check to see if the key is already taken.
	var (
		value *[]byte
		id    int64
	)
	row, cancel := b.tx.QueryRow(
		"SELECT id,value FROM "+b.table+" WHERE "+parentSelector(b.id)+
			" AND key=$1", key,
	)
	defer cancel()

	err := row.Scan(&id, &value)
	switch {
	case err == nil:
		return id, true, value != nil, nil

	case !errors.Is(err, sql.ErrNoRows):
		return 0, false, false, err
	}

	// The key isn't taken as far as this transaction can see, so we go
	// ahead and create the bucket. The database generates the id of the new
	// bucket for us.
	//
	// Note that we deliberately don't use a bare insert here. Another
	// transaction may be creating the very same bucket concurrently, and if
	// it commits first then a bare insert leaves us with a unique
	// constraint violation. That error maps to
	// ErrSQLUniqueConstraintViolation, which the transaction retry loop
	// does not consider retryable, so it would surface as a hard failure to
	// the caller. Phrasing the insert as an upsert instead means the
	// database reports the conflict as a serialization failure, which is
	// retryable: on the retry the select above finds the winner's row.
	//
	// The conflict target has to match the partial unique index that
	// applies to the row being inserted. There is one index for top level
	// rows (<table>_unp, on key where parent_id IS NULL) and one for nested
	// rows (<table>_up, on (parent_id, key) where parent_id IS NOT NULL).
	if b.id == nil {
		row, cancel = b.tx.QueryRow(
			"INSERT INTO "+b.table+" (key) VALUES($1) "+
				"ON CONFLICT (key) WHERE parent_id IS NULL "+
				"DO UPDATE SET key=$1 "+
				"RETURNING id, value", key,
		)
	} else {
		row, cancel = b.tx.QueryRow(
			"INSERT INTO "+b.table+" (key, parent_id) "+
				"VALUES($1, $2) "+
				"ON CONFLICT (key, parent_id) "+
				"WHERE parent_id IS NOT NULL "+
				"DO UPDATE SET key=$1 "+
				"RETURNING id, value", key, b.id,
		)
	}
	defer cancel()

	err = row.Scan(&id, &value)
	if err != nil {
		return 0, false, false, err
	}

	// If the row we got back holds a value, then we collided with a value
	// that was written concurrently, and the key can't be used for a
	// bucket.
	if value != nil {
		return id, true, true, nil
	}

	// At this point the row is ours: the select above proved that no such
	// row was visible to this transaction, and any row inserted
	// concurrently would have made the upsert fail with a serialization
	// error under both of the isolation levels we run write transactions at
	// (serializable and repeatable read).
	return id, false, false, nil
}

// CreateBucket creates and returns a new nested bucket with the given key.
// Returns ErrBucketExists if the bucket already exists, ErrBucketNameRequired
// if the key is empty, or ErrIncompatibleValue if the key value is otherwise
// invalid for the particular database implementation.  Other errors are
// possible depending on the implementation.
func (b *readWriteBucket) CreateBucket(key []byte) (
	walletdb.ReadWriteBucket, error) {

	if len(key) == 0 {
		return nil, walletdb.ErrBucketNameRequired
	}

	id, existed, isValue, err := b.createBucket(key)
	switch {
	case err != nil:
		return nil, err

	case isValue:
		return nil, walletdb.ErrIncompatibleValue

	case existed:
		return nil, walletdb.ErrBucketExists
	}

	return newReadWriteBucket(b.tx, &id), nil
}

// CreateBucketIfNotExists creates and returns a new nested bucket with
// the given key if it does not already exist.  Returns
// ErrBucketNameRequired if the key is empty or ErrIncompatibleValue
// if the key value is otherwise invalid for the particular database
// backend.  Other errors are possible depending on the implementation.
func (b *readWriteBucket) CreateBucketIfNotExists(key []byte) (
	walletdb.ReadWriteBucket, error) {

	if len(key) == 0 {
		return nil, walletdb.ErrBucketNameRequired
	}

	id, _, isValue, err := b.createBucket(key)
	switch {
	case err != nil:
		return nil, err

	case isValue:
		return nil, walletdb.ErrIncompatibleValue
	}

	return newReadWriteBucket(b.tx, &id), nil
}

// DeleteNestedBucket deletes the nested bucket and its sub-buckets
// pointed to by the passed key. All values in the bucket and sub-buckets
// will be deleted as well.
func (b *readWriteBucket) DeleteNestedBucket(key []byte) error {
	if len(key) == 0 {
		return walletdb.ErrIncompatibleValue
	}

	result, err := b.tx.Exec(
		"DELETE FROM "+b.table+" WHERE "+parentSelector(b.id)+
			" AND key=$1 AND value IS NULL",
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

// Put updates the value for the passed key.
// Returns ErrKeyRequired if te passed key is empty.
func (b *readWriteBucket) Put(key, value []byte) error {
	if len(key) == 0 {
		return walletdb.ErrKeyRequired
	}

	// Prevent NULL being written for an empty value slice.
	if value == nil {
		value = []byte{}
	}

	var (
		result sql.Result
		err    error
	)

	// We are putting a value in a bucket in this table. Try to insert the
	// key first. If the key already exists (ON CONFLICT), update the key.
	// Do not update a NULL value, because this indicates that the key
	// contains a sub-bucket. This case will be caught via RowsAffected
	// below.
	if b.id == nil {
		// ON CONFLICT requires the WHERE parent_id IS NULL hint to let
		// Postgres find the NULL-parent_id unique index (<table>_unp).
		result, err = b.tx.Exec(
			"INSERT INTO "+b.table+" (key, value) VALUES($1, $2) "+
				"ON CONFLICT (key) WHERE parent_id IS NULL "+
				"DO UPDATE SET value=$2 "+
				"WHERE "+b.table+".value IS NOT NULL",
			key, value,
		)
	} else {
		// ON CONFLICT requires the WHERE parent_id NOT IS NULL hint to
		// let Postgres find the non-NULL-parent_id unique index
		// (<table>_up).
		result, err = b.tx.Exec(
			"INSERT INTO "+b.table+" (key, value, parent_id) "+
				"VALUES($1, $2, $3) "+
				"ON CONFLICT (key, parent_id) "+
				"WHERE parent_id IS NOT NULL "+
				"DO UPDATE SET value=$2 "+
				"WHERE "+b.table+".value IS NOT NULL",
			key, value, b.id,
		)
	}
	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return walletdb.ErrIncompatibleValue
	}

	return nil
}

// Delete deletes the key/value pointed to by the passed key.
// Returns ErrKeyRequired if the passed key is empty.
func (b *readWriteBucket) Delete(key []byte) error {
	if key == nil {
		return nil
	}
	if len(key) == 0 {
		return walletdb.ErrKeyRequired
	}

	// Check to see if a bucket with this key exists.
	var dummy int
	row, cancel := b.tx.QueryRow(
		"SELECT 1 FROM "+b.table+" WHERE "+parentSelector(b.id)+
			" AND key=$1 AND value IS NULL", key,
	)
	defer cancel()
	err := row.Scan(&dummy)
	switch {
	// No bucket exists, proceed to deletion of the key.
	case err == sql.ErrNoRows:

	case err != nil:
		return err

	// Bucket exists.
	default:
		return walletdb.ErrIncompatibleValue
	}

	_, err = b.tx.Exec(
		"DELETE FROM "+b.table+" WHERE key=$1 AND "+
			parentSelector(b.id)+" AND value IS NOT NULL",
		key,
	)
	if err != nil {
		return err
	}

	return nil
}

// ReadWriteCursor returns a new read-write cursor for this bucket.
func (b *readWriteBucket) ReadWriteCursor() walletdb.ReadWriteCursor {
	return newReadWriteCursor(b)
}

// Tx returns the buckets transaction.
func (b *readWriteBucket) Tx() walletdb.ReadWriteTx {
	return b.tx
}

// NextSequence returns an autoincrementing sequence number for this bucket.
// Note that this is not a thread safe function and as such it must not be used
// for synchronization.
func (b *readWriteBucket) NextSequence() (uint64, error) {
	seq := b.Sequence() + 1

	return seq, b.SetSequence(seq)
}

// SetSequence updates the sequence number for the bucket.
func (b *readWriteBucket) SetSequence(v uint64) error {
	if b.id == nil {
		panic("sequence not supported on top level bucket")
	}

	result, err := b.tx.Exec(
		"UPDATE "+b.table+" SET sequence=$2 WHERE id=$1",
		b.id, int64(v),
	)
	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("cannot set sequence")
	}

	return nil
}

// Sequence returns the current sequence number for this bucket without
// incrementing it.
func (b *readWriteBucket) Sequence() uint64 {
	if b.id == nil {
		panic("sequence not supported on top level bucket")
	}

	var seq int64
	row, cancel := b.tx.QueryRow(
		"SELECT sequence FROM "+b.table+" WHERE id=$1 "+
			"AND sequence IS NOT NULL",
		b.id,
	)
	defer cancel()
	err := row.Scan(&seq)

	switch {
	case err == sql.ErrNoRows:
		return 0

	case err != nil:
		panic(err)
	}

	return uint64(seq)
}

// Prefetch will attempt to prefetch all values under a path from the passed
// bucket.
func (b *readWriteBucket) Prefetch(paths ...[]string) {}

// ForAll is an optimized version of ForEach with the limitation that no
// additional queries can be executed within the callback.
func (b *readWriteBucket) ForAll(cb func(k, v []byte) error) error {
	rows, cancel, err := b.tx.Query(
		"SELECT key, value FROM " + b.table + " WHERE " +
			parentSelector(b.id) + " ORDER BY key",
	)
	if err != nil {
		return err
	}
	defer cancel()

	for rows.Next() {
		var key, value []byte

		err := rows.Scan(&key, &value)
		if err != nil {
			return err
		}

		err = cb(key, value)
		if err != nil {
			return err
		}
	}

	return nil
}
