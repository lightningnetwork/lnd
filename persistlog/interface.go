package persistlog

// PersistLog is an interface that defines a new on-disk data structure that
// contains a persistent log. The interface is general to allow implementations
// near-complete autonomy. All of these calls should be safe for concurrent
// access.
type PersistLog interface {
	// Delete deletes an entry from the persistent log given []byte
	Delete([]byte) error

	// Get retrieves an entry from the persistent log given a []byte. It
	// returns the value stored and an error if one occurs.
	Get([]byte) (uint32, error)

	// Put stores an entry into the persistent log given a []byte and an
	// accompanying purposefully general type. It returns an error if one
	// occurs.
	Put([]byte, uint32) error

	// Start starts up the on-disk persistent log. It returns an error if
	// one occurs.
	Start() error

	// Stop safely stops the on-disk persistent log.
	Stop()
}
