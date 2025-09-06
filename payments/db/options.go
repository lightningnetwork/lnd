package paymentsdb

// StoreOptions holds parameters for the KVStore.
type StoreOptions struct {
	// NoMigration allows to open the database in readonly mode
	NoMigration bool

	// KeepFailedPaymentAttempts is a flag that determines whether to keep
	// failed payment attempts for a settled payment in the db.
	KeepFailedPaymentAttempts bool
}

// DefaultOptions returns a StoreOptions populated with default values.
func DefaultOptions() *StoreOptions {
	return &StoreOptions{
		KeepFailedPaymentAttempts: false,
		NoMigration:               false,
	}
}

// OptionModifier is a function signature for modifying the default
// StoreOptions.
type OptionModifier func(*StoreOptions)

// WithKeepFailedPaymentAttempts sets the KeepFailedPaymentAttempts to n.
func WithKeepFailedPaymentAttempts(n bool) OptionModifier {
	return func(o *StoreOptions) {
		o.KeepFailedPaymentAttempts = n
	}
}

// WithNoMigration allows the database to be opened in read only mode by
// disabling migrations.
func WithNoMigration(b bool) OptionModifier {
	return func(o *StoreOptions) {
		o.NoMigration = b
	}
}
