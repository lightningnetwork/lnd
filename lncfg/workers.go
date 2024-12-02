package lncfg

import "fmt"

const (
	// DefaultReadWorkers is the default maximum number of concurrent
	// workers used by the daemon's read pool.
	DefaultReadWorkers = 100

	// DefaultWriteWorkers is the default maximum number of concurrent
	// workers used by the daemon's write pool.
	DefaultWriteWorkers = 8

	// DefaultSigWorkers is the default maximum number of concurrent workers
	// used by the daemon's sig pool.
	DefaultSigWorkers = 8
)

// Workers exposes CLI configuration for turning resources consumed by worker
// pools.
//
//nolint:ll
type Workers struct {
	// Read is the maximum number of concurrent read pool workers.
	Read int `long:"read" description:"Maximum number of concurrent read pool workers. This number should be proportional to the number of peers."`

	// Write is the maximum number of concurrent write pool workers.
	Write int `long:"write" description:"Maximum number of concurrent write pool workers. This number should be proportional to the number of CPUs on the host. "`

	// Sig is the maximum number of concurrent sig pool workers.
	Sig int `long:"sig" description:"Maximum number of concurrent sig pool workers. This number should be proportional to the number of CPUs on the host."`
}

// Validate checks the Workers configuration to ensure that the input values are
// sane.
func (w *Workers) Validate() error {
	if w.Read <= 0 {
		return fmt.Errorf("number of read workers (%d) must be "+
			"positive", w.Read)
	}
	if w.Write <= 0 {
		return fmt.Errorf("number of write workers (%d) must be "+
			"positive", w.Write)
	}
	if w.Sig <= 0 {
		return fmt.Errorf("number of sig workers (%d) must be "+
			"positive", w.Sig)
	}

	return nil
}

// Compile-time constraint to ensure Workers implements the Validator interface.
var _ Validator = (*Workers)(nil)
