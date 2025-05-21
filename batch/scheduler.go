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

type writeOpts struct{}

func (*writeOpts) ReadOnly() bool {
	return false
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
		}
		trigger := s.b.trigger
		time.AfterFunc(s.duration, func() {
			trigger(ctx)
		})
	}
	s.b.reqs = append(s.b.reqs, &req)

	// If this is a non-lazy request, we'll execute the batch immediately.
	if !r.Opts.Lazy {
		go s.b.trigger(ctx)
	}

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
	var writeTx writeOpts
	commitErr := s.db.ExecTx(ctx, &writeTx, func(tx Q) error {
		return req.Update(tx)
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
