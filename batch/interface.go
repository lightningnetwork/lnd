package batch

import "github.com/lightningnetwork/lnd/channeldb/kvdb"

// Request defines an operation that can be batched into a single bbolt
// transaction.
type Request struct {
	// Reset is called before each invocation of Update and is used to clear
	// any possible modifications to local state as a result of previous
	// calls to Update that were not committed due to a concurrent batch
	// failure.
	//
	// NOTE: This field is optional.
	Reset func()

	// Update is applied alongside other operations in the batch.
	//
	// NOTE: This method MUST NOT acquire any mutexes.
	Update func(tx kvdb.RwTx) error

	// OnCommit is called if the batch or a subset of the batch including
	// this request all succeeded without failure. The passed error should
	// contain the result of the transaction commit, as that can still fail
	// even if none of the closures returned an error.
	//
	// NOTE: This field is optional.
	OnCommit func(commitErr error) error
}

// Scheduler abstracts a generic batching engine that accumulates an incoming
// set of Requests, executes them, and returns the error from the operation.
type Scheduler interface {
	// Execute schedules a Request for execution with the next available
	// batch. This method blocks until the the underlying closure has been
	// run against the databse. The resulting error is returned to the
	// caller.
	Execute(req *Request) error
}
