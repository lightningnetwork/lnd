//go:build kvdb_postgres || (kvdb_sqlite && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64)))

package sqlbase

import (
	"database/sql"
	"errors"
	"fmt"

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
	if c.currKey == nil {
		// Cursor might not be positioned on a valid key.
		return errors.New("cursor not positioned on a key")
	}

	// Attempt to delete the key the cursor is pointing at, but only if it's
	// a value.
	result, err := c.bucket.tx.Exec(
		"DELETE FROM "+c.bucket.table+" WHERE "+
			parentSelector(c.bucket.id)+
			" AND key=$1 AND value IS NOT NULL",
		c.currKey,
	)
	if err != nil {
		panic(err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("error getting rows affected "+
			"for cursor key %x: %w", c.currKey, err)
	}

	// If deletion succeeded, we are done.
	if rows == 1 {
		return nil
	}

	// If rows == 0, the key either didn't exist anymore (concurrent
	// delete?) or it was a bucket. Check if it's a bucket.
	var existsAsBucket int
	rowCheck, cancelCheck := c.bucket.tx.QueryRow(
		"SELECT 1 FROM "+c.bucket.table+" WHERE "+
			parentSelector(c.bucket.id)+
			" AND key=$1 AND value IS NULL", c.currKey,
	)
	defer cancelCheck()

	errCheck := rowCheck.Scan(&existsAsBucket)
	if errCheck == nil {
		// Found it, and value IS NULL -> It's a bucket.
		return walletdb.ErrIncompatibleValue
	}

	if errCheck == sql.ErrNoRows {
		return nil
	}

	// Some other error during the check.
	//
	// TODO(roasbeef): panic here like above/
	return fmt.Errorf("error checking if cursor key %x is "+
		"bucket: %w", c.currKey, errCheck)
}
