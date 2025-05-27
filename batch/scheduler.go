package batch

import (
	"context"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/sqldb"
)

// TimeScheduler is a batching engine that executes requests within a fixed
// horizon. When the first request is received, a TimeScheduler waits a
// configurable duration for other concurrent requests to join the batch. Once
// this time has elapsed, the batch is closed and executed. Subsequent requests
// are then added to a new batch which undergoes the same process.
type TimeScheduler[Q any] struct {
	db       sqldb.BatchedTx[Q]
	locker   sync.Locker
	duration time.Duration

	mu sync.Mutex
	b  *batch[Q]
}

// NewTimeScheduler initializes a new TimeScheduler with a fixed duration at
// which to schedule batches. If the operation needs to modify a higher-level
// cache, the cache's lock should be provided to so that external consistency
// can be maintained, as successful db operations will cause a request's
// OnCommit method to be executed while holding this lock.
func NewTimeScheduler[Q any](db sqldb.BatchedTx[Q], locker sync.Locker,
	duration time.Duration) *TimeScheduler[Q] {

	return &TimeScheduler[Q]{
		db:       db,
		locker:   locker,
		duration: duration,
	}
}

// Execute schedules the provided request for batch execution along with other
// concurrent requests. The request will be executed within a fixed horizon,
// parameterizeed by the duration of the scheduler. The error from the
// underlying operation is returned to the caller.
//
// NOTE: Part of the Scheduler interface.
func (s *TimeScheduler[Q]) Execute(ctx context.Context, r *Request[Q]) error {
	if r.Opts == nil {
		r.Opts = NewDefaultSchedulerOpts()
	}

	req := request[Q]{
		Request: r,
		errChan: make(chan error, 1),
	}

	// Add the request to the current batch. If the batch has been cleared
	// or no batch exists, create a new one.
	s.mu.Lock()
	if s.b == nil {
		s.b = &batch[Q]{
			db:     s.db,
			clear:  s.clear,
			locker: s.locker,

			// By default, we assume that the batch is read-only,
			// and we only upgrade it to read-write if a request
			// is added that is not read-only.
			txOpts: sqldb.ReadTxOpt(),
		}
		trigger := s.b.trigger
		time.AfterFunc(s.duration, func() {
			trigger(ctx)
		})
	}
	s.b.reqs = append(s.b.reqs, &req)

	// We only upgrade the batch to read-write if the new request is not
	// read-only. If it is already read-write, we don't need to do anything.
	if s.b.txOpts.ReadOnly() && !r.Opts.ReadOnly {
		s.b.txOpts = sqldb.WriteTxOpt()
	}

	// If this is a non-lazy request, we'll execute the batch immediately.
	if !r.Opts.Lazy {
		go s.b.trigger(ctx)
	}

	// We need to grab a reference to the batch's txOpts so that we can
	// pass it before we unlock the scheduler's mutex since the batch may
	// be set to nil before we access the txOpts below.
	txOpts := s.b.txOpts

	s.mu.Unlock()

	// Wait for the batch to process the request. If the batch didn't
	// ask us to execute the request individually, simply return the error.
	err := <-req.errChan
	if err != errSolo {
		return err
	}

	// Obtain exclusive access to the cache if this scheduler needs to
	// modify the cache in OnCommit.
	if s.locker != nil {
		s.locker.Lock()
		defer s.locker.Unlock()
	}

	// Otherwise, run the request on its own.
	commitErr := s.db.ExecTx(ctx, txOpts, func(tx Q) error {
		return req.Do(tx)
	}, func() {
		if req.Reset != nil {
			req.Reset()
		}
	})

	// Finally, return the commit error directly or execute the OnCommit
	// closure with the commit error if present.
	if req.OnCommit != nil {
		return req.OnCommit(commitErr)
	}

	return commitErr
}

// clear resets the scheduler's batch to nil so that no more requests can be
// added.
func (s *TimeScheduler[Q]) clear(b *batch[Q]) {
	s.mu.Lock()
	if s.b == b {
		s.b = nil
	}
	s.mu.Unlock()
}
