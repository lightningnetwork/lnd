package batch

import (
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/kvdb"
)

// TimeScheduler is a batching engine that executes requests within a fixed
// horizon. When the first request is received, a TimeScheduler waits a
// configurable duration for other concurrent requests to join the batch. Once
// this time has elapsed, the batch is closed and executed. Subsequent requests
// are then added to a new batch which undergoes the same process.
type TimeScheduler struct {
	db       kvdb.Backend
	locker   sync.Locker
	duration time.Duration

	mu sync.Mutex
	b  *batch
}

// NewTimeScheduler initializes a new TimeScheduler with a fixed duration at
// which to schedule batches. If the operation needs to modify a higher-level
// cache, the cache's lock should be provided to so that external consistency
// can be maintained, as successful db operations will cause a request's
// OnCommit method to be executed while holding this lock.
func NewTimeScheduler(db kvdb.Backend, locker sync.Locker,
	duration time.Duration) *TimeScheduler {

	return &TimeScheduler{
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
func (s *TimeScheduler) Execute(r *Request) error {
	req := request{
		Request: r,
		errChan: make(chan error, 1),
	}

	// Add the request to the current batch. If the batch has been cleared
	// or no batch exists, create a new one.
	s.mu.Lock()
	if s.b == nil {
		s.b = &batch{
			db:     s.db,
			clear:  s.clear,
			locker: s.locker,
		}
		time.AfterFunc(s.duration, s.b.trigger)
	}
	s.b.reqs = append(s.b.reqs, &req)

	// If this is a non-lazy request, we'll execute the batch immediately.
	if !r.lazy {
		go s.b.trigger()
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
	commitErr := kvdb.Update(s.db, req.Update, func() {
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
func (s *TimeScheduler) clear(b *batch) {
	s.mu.Lock()
	if s.b == b {
		s.b = nil
	}
	s.mu.Unlock()
}
