package payments

// KVStoreOptions holds parameters for the KVStore.
type KVStoreOptions struct {
	// NoMigration allows to open the database in readonly mode
	NoMigration bool

	// KeepFailedPaymentAttempts is a flag that determines whether to keep
	// failed payment attempts for a settled payment in the db.
	keepFailedPaymentAttempts bool
}

// KVStoreOptionModifier is a function signature for modifying the default
// KVStoreOptions.
type KVStoreOptionModifier func(*KVStoreOptions)

// DefaultOptions returns a KVStoreOptions populated with default values.
func DefaultOptions() *KVStoreOptions {
	return &KVStoreOptions{
		keepFailedPaymentAttempts: false,
	}
}

// WithKeepFailedPaymentAttempts sets the KeepFailedPaymentAttempts to n.
func WithKeepFailedPaymentAttempts(n bool) KVStoreOptionModifier {
	return func(o *KVStoreOptions) {
		o.keepFailedPaymentAttempts = n
	}
}

// WithNoMigration allows the database to be opened in read only mode by
// disabling migrations.
func WithNoMigration(b bool) KVStoreOptionModifier {
	return func(o *KVStoreOptions) {
		o.NoMigration = b
	}
}
