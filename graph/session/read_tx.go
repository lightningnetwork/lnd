package session

// RTx represents a read-only transaction that can only be used for graph
// reads during a path-finding session.
type RTx interface {
	// Close closes the transaction.
	Close() error

	// MustImplementRTx is a helper method that ensures that the RTx
	// interface is implemented by the underlying type. This is useful since
	// the other methods in the interface are quite generic and so many
	// types will satisfy the interface if it only contains those methods.
	MustImplementRTx()
}
