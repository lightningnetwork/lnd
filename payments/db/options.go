package paymentsdb

// StoreOptions holds parameters for the KVStore.
type StoreOptions struct {
	// NoMigration allows to open the database in readonly mode
	NoMigration bool

	// KeepFailedPaymentAttempts is a flag that determines whether to keep
	// failed payment attempts for a settled payment in the db.
	keepFailedPaymentAttempts bool

	// paginationLimit is the maximum number of payments to return per page
	// when doing cursor-based pagination.
	//
	// NOTE: Only used for the SQL store.
	paginationLimit int
}

// OptionModifier is a function signature for modifying the default
// StoreOptions.
type OptionModifier func(*StoreOptions)

// WithKeepFailedPaymentAttempts sets the KeepFailedPaymentAttempts to n.
func WithKeepFailedPaymentAttempts(n bool) OptionModifier {
	return func(o *StoreOptions) {
		o.keepFailedPaymentAttempts = n
	}
}

// WithNoMigration allows the database to be opened in read only mode by
// disabling migrations.
func WithNoMigration(b bool) OptionModifier {
	return func(o *StoreOptions) {
		o.NoMigration = b
	}
}

// WithPaginationLimit sets the pagination limit for the SQL store queries that
// paginate results.
func WithPaginationLimit(limit int) OptionModifier {
	return func(o *StoreOptions) {
		o.paginationLimit = limit
	}
}
