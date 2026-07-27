package sqldb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"syscall"

	"github.com/btcsuite/btcwallet/walletdb"
)

var (
	// ErrRetryCanceled is returned when the transaction retry loop was
	// interrupted before it could either complete or exhaust its retry
	// budget. This happens when the context of the caller is canceled, or
	// the caller's quit channel is closed, while we're waiting to retry a
	// transaction that failed with a serialization error.
	ErrRetryCanceled = errors.New("db tx retry canceled")
)

// IsInternalDBError returns true if the passed error signals trouble with our
// local database infrastructure rather than a problem with the data that was
// read or written.
//
// Callers use this to tell apart errors that are our own fault (a busy or
// unavailable database) from errors that are caused by the input they were
// handed. This distinction matters on the channel state machine paths: a
// database hiccup must never be reported to our channel peer as a protocol
// violation, since some peers respond to such a report by force closing the
// channel.
//
// The predicate is deliberately conservative. It only matches errors that
// unambiguously mean local infrastructure trouble, so that a genuine protocol
// violation is never mistaken for one and silently swallowed.
//
// NOTE: This covers the bbolt backed kv stores as well as the SQL ones. The
// walletdb layer converts the two bbolt errors that can surface on a write path
// into the sentinels matched below, so there is no need to match on bbolt
// itself here. See convertErr in btcwallet's walletdb/bdb package.
func IsInternalDBError(err error) bool {
	if err == nil {
		return false
	}

	switch {
	// The transaction couldn't be serialized against the other concurrent
	// transactions, and the retry machinery gave up on it.
	case IsSerializationError(err):
		return true

	// The retry machinery exhausted its budget while retrying a
	// serialization error.
	case errors.Is(err, ErrRetriesExceeded):
		return true

	// The retry machinery was interrupted before it could complete.
	case errors.Is(err, ErrRetryCanceled):
		return true

	// The connection to the database is gone, or the database driver
	// decided the connection was no longer usable.
	case errors.Is(err, sql.ErrConnDone),
		errors.Is(err, sql.ErrTxDone),
		errors.Is(err, driver.ErrBadConn):

		return true

	// The query was canceled or timed out. This is either a shutdown, or a
	// database that is too slow to answer within the configured timeout.
	// Neither is the fault of our peer.
	case errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):

		return true

	// The kv store was closed out from under us, or it was opened
	// read-only. Both mean our own database is unusable, and neither says
	// anything about the data we were trying to write.
	case errors.Is(err, walletdb.ErrDbNotOpen),
		errors.Is(err, walletdb.ErrTxNotWritable):

		return true

	// We ran out of disk space. This arrives wrapped in an *os.PathError
	// from the file backed stores, which errors.Is unwraps for us.
	case errors.Is(err, syscall.ENOSPC):
		return true
	}

	return false
}
