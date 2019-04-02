package pool

import (
	"errors"
	"sync"
	"time"
)

// ErrWorkerPoolExiting signals that a shutdown of the Worker has been
// requested.
var ErrWorkerPoolExiting = errors.New("worker pool exiting")

// DefaultWorkerTimeout is the default duration after which a worker goroutine
// will exit to free up resources after having received no newly submitted
// tasks.
const DefaultWorkerTimeout = 5 * time.Second

type (
	// WorkerState is an interface used by the Worker to abstract the
	// lifecycle of internal state used by a worker goroutine.
	WorkerState interface {
		// Reset clears any internal state that may have been dirtied in
		// processing a prior task.
		Reset()

		// Cleanup releases any shared state before a worker goroutine
		// exits.
		Cleanup()
	}

	// WorkerConfig parameterizes the behavior of a Worker pool.
	WorkerConfig struct {
		// NewWorkerState allocates a new state for a worker goroutine.
		// This method is called each time a new worker goroutine is
		// spawned by the pool.
		NewWorkerState func() WorkerState

		// NumWorkers is the maximum number of workers the Worker pool
		// will permit to be allocated. Once the maximum number is
		// reached, any newly submitted tasks are forced to be processed
		// by existing worker goroutines.
		NumWorkers int

		// WorkerTimeout is the duration after which a worker goroutine
		// will exit after having received no newly submitted tasks.
		WorkerTimeout time.Duration
	}

	// Worker maintains a pool of goroutines that process submitted function
	// closures, and enable more efficient reuse of expensive state.
	Worker struct {
		started sync.Once
		stopped sync.Once

		cfg *WorkerConfig

		// requests is a channel where new tasks are submitted. Tasks
		// submitted through this channel may cause a new worker
		// goroutine to be allocated.
		requests chan *request

		// work is a channel where new tasks are submitted, but is only
		// read by active worker gorotuines.
		work chan *request

		// workerSem is a channel-based sempahore that is used to limit
		// the total number of worker goroutines to the number
		// prescribed by the WorkerConfig.
		workerSem chan struct{}

		wg   sync.WaitGroup
		quit chan struct{}
	}

	// request is a tuple of task closure and error channel that is used to
	// both submit a task to the pool and respond with any errors
	// encountered during the task's execution.
	request struct {
		fn      func(WorkerState) error
		errChan chan error
	}
)

// NewWorker initializes a new Worker pool using the provided WorkerConfig.
func NewWorker(cfg *WorkerConfig) *Worker {
	return &Worker{
		cfg:       cfg,
		requests:  make(chan *request),
		workerSem: make(chan struct{}, cfg.NumWorkers),
		work:      make(chan *request),
		quit:      make(chan struct{}),
	}
}

// Start safely spins up the Worker pool.
func (w *Worker) Start() error {
	w.started.Do(func() {
		w.wg.Add(1)
		go w.requestHandler()
	})
	return nil
}

// Stop safely shuts down the Worker pool.
func (w *Worker) Stop() error {
	w.stopped.Do(func() {
		close(w.quit)
		w.wg.Wait()
	})
	return nil
}

// Submit accepts a function closure to the worker pool. The returned error will
// be either the result of the closure's execution or an ErrWorkerPoolExiting if
// a shutdown is requested.
func (w *Worker) Submit(fn func(WorkerState) error) error {
	req := &request{
		fn:      fn,
		errChan: make(chan error, 1),
	}

	select {

	// Send request to requestHandler, where either a new worker is spawned
	// or the task will be handed to an existing worker.
	case w.requests <- req:

	// Fast path directly to existing worker.
	case w.work <- req:

	case <-w.quit:
		return ErrWorkerPoolExiting
	}

	select {

	// Wait for task to be processed.
	case err := <-req.errChan:
		return err

	case <-w.quit:
		return ErrWorkerPoolExiting
	}
}

// requestHandler processes incoming tasks by either allocating new worker
// goroutines to process the incoming tasks, or by feeding a submitted task to
// an already running worker goroutine.
func (w *Worker) requestHandler() {
	defer w.wg.Done()

	for {
		select {
		case req := <-w.requests:
			select {

			// If we have not reached our maximum number of workers,
			// spawn one to process the submitted request.
			case w.workerSem <- struct{}{}:
				w.wg.Add(1)
				go w.spawnWorker(req)

			// Otherwise, submit the task to any of the active
			// workers.
			case w.work <- req:

			case <-w.quit:
				return
			}

		case <-w.quit:
			return
		}
	}
}

// spawnWorker is used when the Worker pool wishes to create a new worker
// goroutine. The worker's state is initialized by calling the config's
// NewWorkerState method, and will continue to process incoming tasks until the
// pool is shut down or no new tasks are received before the worker's timeout
// elapses.
//
// NOTE: This method MUST be run as a goroutine.
func (w *Worker) spawnWorker(req *request) {
	defer w.wg.Done()
	defer func() { <-w.workerSem }()

	state := w.cfg.NewWorkerState()
	defer state.Cleanup()

	req.errChan <- req.fn(state)

	// We'll use a timer to implement the worker timeouts, as this reduces
	// the number of total allocations that would otherwise be necessary
	// with time.After.
	var t *time.Timer
	for {
		// Before processing another request, we'll reset the worker
		// state to that each request is processed against a clean
		// state.
		state.Reset()

		select {

		// Process any new requests that get submitted. We use a
		// non-blocking case first so that under high load we can spare
		// allocating a timeout.
		case req := <-w.work:
			req.errChan <- req.fn(state)
			continue

		case <-w.quit:
			return

		default:
		}

		// There were no new requests that could be taken immediately
		// from the work channel. Initialize or reset the timeout, which
		// will fire if the worker doesn't receive a new task before
		// needing to exit.
		if t != nil {
			t.Reset(w.cfg.WorkerTimeout)
		} else {
			t = time.NewTimer(w.cfg.WorkerTimeout)
		}

		select {

		// Process any new requests that get submitted.
		case req := <-w.work:
			req.errChan <- req.fn(state)

			// Stop the timer, draining the timer's channel if a
			// notification was already delivered.
			if !t.Stop() {
				<-t.C
			}

		// The timeout has elapsed, meaning the worker did not receive
		// any new tasks. Exit to allow the worker to return and free
		// its resources.
		case <-t.C:
			return

		case <-w.quit:
			return
		}
	}
}
