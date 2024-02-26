package lnd

import (
	"context"

	"github.com/btcsuite/btclog"
)

// SQLDatabaseBuilder is a type that builds the SQL database backends for lnd,
type SQLDatabaseBuilder struct {
	cfg    *Config
	logger btclog.Logger
}

// Compile time assertion that SQLDatabaseBuilder implements DatabaseBuilder.
var _ DatabaseBuilder = (*SQLDatabaseBuilder)(nil)

// NewSQLDatabaseBuilder returns a new instance of the SQL database builder.
func NewSQLDatabaseBuilder(cfg *Config,
	logger btclog.Logger) *SQLDatabaseBuilder {

	return &SQLDatabaseBuilder{
		cfg:    cfg,
		logger: logger,
	}
}

// BuildDatabase...
func (d *SQLDatabaseBuilder) BuildDatabase(
	ctx context.Context) (*DatabaseInstances, func(), error) {

	// TODO(yy): Fix the dup logs.
	d.logger.Infof("Opening database, this might take a few minutes...")

	// Get the default database builder.
	//
	// TODO(yy): remove it once we have migrated all kvdb to native sql.
	defaultBuiler := NewDefaultDatabaseBuilder(d.cfg, d.logger)

	// Build the default database instance. Though the database could be
	// SQL, the actual data store still uses key-value format.
	dbs, cleanUp, err := defaultBuiler.BuildDatabase(ctx)
	if err != nil {
		return nil, nil, err
	}

	// // Build the SQL database instance.
	// sqlDB, err := NewSQLDB(d.cfg)

	// // Map the database instances to the SQL database instances.
	// dbs.InvoiceDB = sqlDB.NewInvoiceDB(d.cfg)
	// dbs.PaymentDB = sqlDB.NewPaymentStore(d.cfg)

	// TODO(yy): overwrite more sql tables here
	// NOTE(yy): all the mappings should be removed eventually.

	return dbs, cleanUp, nil
}
