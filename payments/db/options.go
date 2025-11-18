package paymentsdb

// StoreOptions holds parameters for the KVStore.
type StoreOptions struct {
	// NoMigration allows to open the database in readonly mode
	NoMigration bool
}

// DefaultOptions returns a StoreOptions populated with default values.
func DefaultOptions() *StoreOptions {
	return &StoreOptions{
		NoMigration: false,
	}
}

// OptionModifier is a function signature for modifying the default
// StoreOptions.
type OptionModifier func(*StoreOptions)

// WithNoMigration allows the database to be opened in read only mode by
// disabling migrations.
func WithNoMigration(b bool) OptionModifier {
	return func(o *StoreOptions) {
		o.NoMigration = b
	}
}
