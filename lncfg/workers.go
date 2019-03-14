package lncfg

const (
	// DefaultReadWorkers is the default maximum number of concurrent
	// workers used by the daemon's read pool.
	DefaultReadWorkers = 16

	// DefaultWriteWorkers is the default maximum number of concurrent
	// workers used by the daemon's write pool.
	DefaultWriteWorkers = 16

	// DefaultSigWorkers is the default maximum number of concurrent workers
	// used by the daemon's sig pool.
	DefaultSigWorkers = 8
)

// Workers exposes CLI configuration for turning resources consumed by worker
// pools.
type Workers struct {
	// Read is the maximum number of concurrent read pool workers.
	Read int `long:"read" description:"Maximum number of concurrent read pool workers."`

	// Write is the maximum number of concurrent write pool workers.
	Write int `long:"write" description:"Maximum number of concurrent write pool workers."`

	// Sig is the maximum number of concurrent sig pool workers.
	Sig int `long:"sig" description:"Maximum number of concurrent sig pool workers."`
}
