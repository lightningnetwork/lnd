package payments

import (
	"database/sql"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/sqldb"
)

// NewPaymentStore...
func NewPaymentStore(db sqldb.DatabaseBackend,
	clock clock.Clock) *sqldb.Store[PaymentDB] {

	sqlDB := sqldb.NewTransactionExecutor(
		db, func(tx *sql.Tx) PaymentDB {
			// TODO(yy): need to gen the sql for payments.
			return db.WithTx(tx)
		},
	)

	return &sqldb.Store[PaymentDB]{
		DB:    sqlDB,
		Clock: clock,
	}
}

// Use it in config.go.
// db, err := sqldb.NewSqliteStore(cfg.Sqlite)
// paymentDB := payments.NewPaymentStore(db, clock)
