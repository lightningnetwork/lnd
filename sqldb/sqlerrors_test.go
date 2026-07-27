package sqldb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/require"
)

// serializationErr returns an error that looks like the one postgres hands us
// when a transaction couldn't be serialized against the other concurrent
// transactions.
func serializationErr() error {
	return MapSQLError(errors.New("ERROR: could not serialize access due " +
		"to read/write dependencies among transactions (SQLSTATE " +
		"40001)"))
}

// TestIsInternalDBError tests that we correctly tell errors caused by our own
// database infrastructure apart from errors caused by the data we were handed.
func TestIsInternalDBError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		exp  bool
	}{
		{
			name: "nil error",
			err:  nil,
			exp:  false,
		},
		{
			name: "unrelated error",
			err:  errors.New("revocation key mismatch"),
			exp:  false,
		},
		{
			name: "unique constraint violation",
			err: &ErrSQLUniqueConstraintViolation{
				DBError: errors.New("duplicate key"),
			},
			exp: false,
		},
		{
			name: "serialization error",
			err:  serializationErr(),
			exp:  true,
		},
		{
			name: "wrapped serialization error",
			err: fmt.Errorf("unable to restore remote unsigned "+
				"local updates: %w", serializationErr()),
			exp: true,
		},
		{
			name: "retries exceeded",
			err:  ErrRetriesExceeded,
			exp:  true,
		},
		{
			name: "wrapped retries exceeded",
			err: fmt.Errorf("%w: %w", ErrRetriesExceeded,
				serializationErr()),
			exp: true,
		},
		{
			name: "canceled retry",
			err: fmt.Errorf("%w: %w", ErrRetryCanceled,
				serializationErr()),
			exp: true,
		},
		{
			name: "connection done",
			err:  fmt.Errorf("query failed: %w", sql.ErrConnDone),
			exp:  true,
		},
		{
			name: "tx done",
			err:  fmt.Errorf("commit failed: %w", sql.ErrTxDone),
			exp:  true,
		},
		{
			name: "bad conn",
			err:  fmt.Errorf("query failed: %w", driver.ErrBadConn),
			exp:  true,
		},
		{
			name: "kv store not open",
			err: fmt.Errorf("unable to fetch chan bucket: %w",
				walletdb.ErrDbNotOpen),
			exp: true,
		},
		{
			name: "kv tx not writable",
			err:  walletdb.ErrTxNotWritable,
			exp:  true,
		},
		{
			name: "kv bucket not found is not infra",
			err:  walletdb.ErrBucketNotFound,
			exp:  false,
		},
		{
			name: "out of disk space",
			err: fmt.Errorf("write failed: %w", &os.PathError{
				Op:   "write",
				Path: "/lnd/channel.db",
				Err:  syscall.ENOSPC,
			}),
			exp: true,
		},
		{
			name: "context canceled",
			err: fmt.Errorf("query failed: %w",
				context.Canceled),
			exp: true,
		},
		{
			name: "context deadline exceeded",
			err:  context.DeadlineExceeded,
			exp:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, test.exp, IsInternalDBError(test.err))
		})
	}
}
