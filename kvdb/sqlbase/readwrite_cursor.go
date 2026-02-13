//go:build kvdb_postgres || (kvdb_sqlite && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64)))

package sqlbase

import (
	"database/sql"

	"github.com/btcsuite/btcwallet/walletdb"
)

// readWriteCursor holds a reference to the cursors bucket, the value
// prefix and the current key used while iterating.
type readWriteCursor struct {
	bucket *readWriteBucket

	// currKey holds the current key of the cursor.
	currKey []byte
}

func newReadWriteCursor(b *readWriteBucket) *readWriteCursor {
	return &readWriteCursor{
		bucket: b,
	}
}

// First positions the cursor at the first key/value pair and returns
// the pair.
func (c *readWriteCursor) First() ([]byte, []byte) {
	var (
		key   []byte
		value []byte
	)
	row, cancel := c.bucket.tx.QueryRow(
		"SELECT key, value FROM " + c.bucket.table + " WHERE " +
			parentSelector(c.bucket.id) +
			" ORDER BY key LIMIT 1",
	)
	defer cancel()
	err := row.Scan(&key, &value)

	switch {
	case err == sql.ErrNoRows:
		return nil, nil

	case err != nil:
		panic(err)
	}

	// Copy current key to prevent modification by the caller.
	c.currKey = make([]byte, len(key))
	copy(c.currKey, key)

	return key, value
}

// Last positions the cursor at the last key/value pair and returns the
// pair.
func (c *readWriteCursor) Last() ([]byte, []byte) {
	var (
		key   []byte
		value []byte
	)
	row, cancel := c.bucket.tx.QueryRow(
		"SELECT key, value FROM " + c.bucket.table + " WHERE " +
			parentSelector(c.bucket.id) +
			" ORDER BY key DESC LIMIT 1",
	)
	defer cancel()
	err := row.Scan(&key, &value)

	switch {
	case err == sql.ErrNoRows:
		return nil, nil

	case err != nil:
		panic(err)
	}

	// Copy current key to prevent modification by the caller.
	c.currKey = make([]byte, len(key))
	copy(c.currKey, key)

	return key, value
}

// Next moves the cursor one key/value pair forward and returns the new
// pair.
func (c *readWriteCursor) Next() ([]byte, []byte) {
	var (
		key   []byte
		value []byte
	)
	row, cancel := c.bucket.tx.QueryRow(
		"SELECT key, value FROM "+c.bucket.table+" WHERE "+
			parentSelector(c.bucket.id)+
			" AND key>$1 ORDER BY key LIMIT 1",
		c.currKey,
	)
	defer cancel()
	err := row.Scan(&key, &value)

	switch {
	case err == sql.ErrNoRows:
		return nil, nil

	case err != nil:
		panic(err)
	}

	// Copy current key to prevent modification by the caller.
	c.currKey = make([]byte, len(key))
	copy(c.currKey, key)

	return key, value
}

// Prev moves the cursor one key/value pair backward and returns the new
// pair.
func (c *readWriteCursor) Prev() ([]byte, []byte) {
	var (
		key   []byte
		value []byte
	)
	row, cancel := c.bucket.tx.QueryRow(
		"SELECT key, value FROM "+c.bucket.table+" WHERE "+
			parentSelector(c.bucket.id)+
			" AND key<$1 ORDER BY key DESC LIMIT 1",
		c.currKey,
	)
	defer cancel()
	err := row.Scan(&key, &value)

	switch {
	case err == sql.ErrNoRows:
		return nil, nil

	case err != nil:
		panic(err)
	}

	// Copy current key to prevent modification by the caller.
	c.currKey = make([]byte, len(key))
	copy(c.currKey, key)

	return key, value
}

// Seek positions the cursor at the passed seek key.  If the key does
// not exist, the cursor is moved to the next key after seek.  Returns
// the new pair.
func (c *readWriteCursor) Seek(seek []byte) ([]byte, []byte) {
	// Convert nil to empty slice, otherwise sql mapping won't be correct
	// and no keys are found.
	if seek == nil {
		seek = []byte{}
	}

	var (
		key   []byte
		value []byte
	)
	row, cancel := c.bucket.tx.QueryRow(
		"SELECT key, value FROM "+c.bucket.table+" WHERE "+
			parentSelector(c.bucket.id)+
			" AND key>=$1 ORDER BY key LIMIT 1",
		seek,
	)
	defer cancel()
	err := row.Scan(&key, &value)

	switch {
	case err == sql.ErrNoRows:
		return nil, nil

	case err != nil:
		panic(err)
	}

	// Copy current key to prevent modification by the caller.
	c.currKey = make([]byte, len(key))
	copy(c.currKey, key)

	return key, value
}

// Delete removes the current key/value pair the cursor is at without
// invalidating the cursor. Returns ErrIncompatibleValue if attempted when the
// cursor points to a nested bucket.
func (c *readWriteCursor) Delete() error {
	// Use a single atomic query with CTEs to:
	// 1. Find the first key at or after cursor position
	// 2. Delete it if it's a value (not a bucket)
	// 3. Return whether it was deleted or is a bucket
	var deleted bool
	var isBucket bool
	
	row, cancel := c.bucket.tx.QueryRow(
		"WITH target AS ("+
			"  SELECT key, value FROM "+c.bucket.table+
			"  WHERE "+parentSelector(c.bucket.id)+
			"  AND key >= $1"+
			"  ORDER BY key"+
			"  LIMIT 1"+
			"), "+
			"deleted AS ("+
			"  DELETE FROM "+c.bucket.table+
			"  WHERE "+parentSelector(c.bucket.id)+
			"  AND key = (SELECT key FROM target)"+
			"  AND value IS NOT NULL"+
			"  RETURNING 1"+
			") "+
			"SELECT "+
			"  EXISTS(SELECT 1 FROM deleted) AS was_deleted, "+
			"  EXISTS(SELECT 1 FROM target WHERE value IS NULL) AS is_bucket",
		c.currKey,
	)
	defer cancel()

	err := row.Scan(&deleted, &isBucket)
	if err == sql.ErrNoRows {
		// No key at or after cursor position.
		return nil
	}
	if err != nil {
		panic(err)
	}

	// If we deleted the key, we're done.
	if deleted {
		return nil
	}

	// If the key is a bucket, return incompatible value error.
	if isBucket {
		return walletdb.ErrIncompatibleValue
	}

	// No key found (target was empty).
	return nil
}
