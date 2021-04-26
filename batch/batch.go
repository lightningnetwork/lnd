package batch

import (
	"errors"
	"sync"

	"github.com/lightningnetwork/lnd/kvdb"
)

// errSolo is a sentinel error indicating that the requester should re-run the
// operation in isolation.
var errSolo = errors.New(
	"batch function returned an error and should be re-run solo",
)

type request struct {
	*Request
	errChan chan error
}

type batch struct {
	db     kvdb.Backend
	start  sync.Once
	reqs   []*request
	clear  func(b *batch)
	locker sync.Locker
}

// trigger is the entry point for the batch and ensures that run is started at
// most once.
func (b *batch) trigger() {
	b.start.Do(b.run)
}

// run executes the current batch of requests. If any individual requests fail
// alongside others they will be retried by the caller.
func (b *batch) run() {
	// Clear the batch from its scheduler, ensuring that no new requests are
	// added to this batch.
	b.clear(b)

	// If a cache lock was provided, hold it until the this method returns.
	// This is critical for ensuring external consistency of the operation,
	// so that caches don't get out of sync with the on disk state.
	if b.locker != nil {
		b.locker.Lock()
		defer b.locker.Unlock()
	}

	// Apply the batch until a subset succeeds or all of them fail. Requests
	// that fail will be retried individually.
	for len(b.reqs) > 0 {
		var failIdx = -1
		err := kvdb.Update(b.db, func(tx kvdb.RwTx) error {
			for i, req := range b.reqs {
				err := req.Update(tx)
				if err != nil {
					failIdx = i
					return err
				}
			}
			return nil
		}, func() {
			for _, req := range b.reqs {
				if req.Reset != nil {
					req.Reset()
				}
			}
		})

		// If a request's Update failed, extract it and re-run the
		// batch. The removed request will be retried individually by
		// the caller.
		if failIdx >= 0 {
			req := b.reqs[failIdx]

			// It's safe to shorten b.reqs here because the
			// scheduler's batch no longer points to us.
			b.reqs[failIdx] = b.reqs[len(b.reqs)-1]
			b.reqs = b.reqs[:len(b.reqs)-1]

			// Tell the submitter re-run it solo, continue with the
			// rest of the batch.
			req.errChan <- errSolo
			continue
		}

		// None of the remaining requests failed, process the errors
		// using each request's OnCommit closure and return the error
		// to the requester. If no OnCommit closure is provided, simply
		// return the error directly.
		for _, req := range b.reqs {
			if req.OnCommit != nil {
				req.errChan <- req.OnCommit(err)
			} else {
				req.errChan <- err
			}
		}

		return
	}
}
