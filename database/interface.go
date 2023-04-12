package database

import "context"

// TxOptions represents a set of options one can use to control what type of
// database transaction is created. Transaction can wither be read or write.
type TxOptions interface {
	// ReadOnly returns true if the transaction should be read only.
	ReadOnly() bool
}

// Tx represents a database transaction that can be committed or rolled back.
type Tx interface {
	// Commit commits the database transaction, an error should be returned
	// if the commit isn't possible.
	Commit() error

	// Rollback rolls back an incomplete database transaction.
	// Transactions that were able to be committed can still call this as a
	// noop.
	Rollback() error
}

// TxCreator defines a method for creating new database transactions based on
// the provided TxOptions.
type TxCreator interface {
	CreateTx(ctx context.Context, options TxOptions) (Tx, error)
}

// TxBinder binds a database transaction to a given database interface. All the
// methods called in the returned database interface will be executed in the
// context of the transaction.
type TxBinder[Q any] func(Tx) Q
