package database

import (
	"time"
)

var (
	// DefaultStoreTimeout is the default timeout used for any interaction
	// with the storage/database.
	DefaultStoreTimeout = time.Second * 10
)

// DBTx is an interface that wraps the database transaction. This is used to
// allow multiple database backends to be used with the same interface.
type DBTx interface {
	// Commit commits all changes that have been executed during the
	// transaction to persistent storage.
	Commit() error

	// Rollback undoes all changes that have been executed during the
	// transaction.
	Rollback() error
}
