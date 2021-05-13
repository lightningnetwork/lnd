package batch

import "github.com/lightningnetwork/lnd/kvdb"

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

	// lazy should be true if we don't have to immediately execute this
	// request when it comes in. This means that it can be scheduled later,
	// allowing larger batches.
	lazy bool
}

// SchedulerOption is a type that can be used to supply options to a scheduled
// request.
type SchedulerOption func(r *Request)

// LazyAdd will make the request be executed lazily, added to the next batch to
// reduce db contention.
func LazyAdd() SchedulerOption {
	return func(r *Request) {
		r.lazy = true
	}
}

// Scheduler abstracts a generic batching engine that accumulates an incoming
// set of Requests, executes them, and returns the error from the operation.
type Scheduler interface {
	// Execute schedules a Request for execution with the next available
	// batch. This method blocks until the the underlying closure has been
	// run against the database. The resulting error is returned to the
	// caller.
	Execute(req *Request) error
}
