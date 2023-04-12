package database

import "context"

// TxExecutor is a wrapper around a database interface that allows the caller
// to execute a function in the context of a database transaction.
type TxExecutor[Q any] struct {
	// db is the database interface where the queries are executed.
	db Q

	// txCreator is used for creating new database transactions.
	txCreator TxCreator

	// txBinder is used for binding all the queries of the returned db to
	// the same transaction.
	txBinder TxBinder[Q]

	// errMapper is used for mapping the errors returned by the database.
	errMapper func(error) error
}

// NewTxExecutor creates a new TxExecutor that will use the given db interface
// to execute the queries.
//
// NOTE: A txCreator compatible with the underlying database must be provided.
//
// NOTE: errMapper is optional, if not provided, the errors returned by the
// database will be returned as is.
func NewTxExecutor[Q any](db Q, txCreator TxCreator, txBinder TxBinder[Q],
	errMapper func(error) error) *TxExecutor[Q] {

	// If no error mapper is provided, we'll just return the error as is.
	// The easiest way to do not branch on the error mapper is to just set
	// it to a noop function.
	if errMapper == nil {
		errMapper = func(err error) error {
			return err
		}
	}

	return &TxExecutor[Q]{
		db:        db,
		txCreator: txCreator,
		txBinder:  txBinder,
		errMapper: errMapper,
	}
}

// ExecTx creates a new database transaction based on the given options and
// executes the given function in the context of that transaction. All the db
// methods called in the txBody will be executed in the context of the
// transaction.
func (t *TxExecutor[Q]) ExecTx(ctx context.Context, txOptions TxOptions,
	txBody func(Q) error) error {

	// Create the db transaction.
	tx, err := t.txCreator.CreateTx(ctx, txOptions)
	if err != nil {
		return err
	}

	// Rollback is safe to call even if the tx is already closed, so if the
	// tx commits successfully, this is a no-op.
	defer func() {
		_ = tx.Rollback()
	}()

	// Bind the transaction to the given db interface. This will make sure
	// that all the db methods called in the txBody will be executed in the
	// context of the transaction.
	txQuerier := t.txBinder(tx)

	// Execute the transaction body.
	if err := txBody(txQuerier); err != nil {
		return t.errMapper(err)
	}

	// Commit transaction.
	if err = tx.Commit(); err != nil {
		return t.errMapper(err)
	}

	return nil
}
