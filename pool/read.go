package pool

import (
	"time"

	"github.com/lightningnetwork/lnd/buffer"
)

// Read is a worker pool specifically designed for sharing access to buffer.Read
// objects amongst a set of worker goroutines. This enables an application to
// limit the total number of buffer.Read objects allocated at any given time.
type Read struct {
	workerPool *Worker
	bufferPool *ReadBuffer
}

// NewRead creates a new Read pool, using an underlying ReadBuffer pool to
// recycle buffer.Read objects across the lifetime of the Read pool's workers.
func NewRead(readBufferPool *ReadBuffer, numWorkers int,
	workerTimeout time.Duration) *Read {

	r := &Read{
		bufferPool: readBufferPool,
	}
	r.workerPool = NewWorker(&WorkerConfig{
		NewWorkerState: r.newWorkerState,
		NumWorkers:     numWorkers,
		WorkerTimeout:  workerTimeout,
	})

	return r
}

// Start safely spins up the Read pool.
func (r *Read) Start() error {
	return r.workerPool.Start()
}

// Stop safely shuts down the Read pool.
func (r *Read) Stop() error {
	return r.workerPool.Stop()
}

// Submit accepts a function closure that provides access to the fresh
// buffer.Read object. The function's execution will be allocated to one of the
// underlying Worker pool's goroutines.
func (r *Read) Submit(inner func(*buffer.Read) error) error {
	return r.workerPool.Submit(func(s WorkerState) error {
		state := s.(*readWorkerState)
		return inner(state.readBuf)
	})
}

// readWorkerState is the per-goroutine state maintained by a Read pool's
// goroutines.
type readWorkerState struct {
	// bufferPool is the pool to which the readBuf will be returned when the
	// goroutine exits.
	bufferPool *ReadBuffer

	// readBuf is a buffer taken from the bufferPool on initialization,
	// which will be cleaned and provided to any tasks that the goroutine
	// processes before exiting.
	readBuf *buffer.Read
}

// newWorkerState initializes a new readWorkerState, which will be called
// whenever a new goroutine is allocated to begin processing read tasks.
func (r *Read) newWorkerState() WorkerState {
	return &readWorkerState{
		bufferPool: r.bufferPool,
		readBuf:    r.bufferPool.Take(),
	}
}

// Cleanup returns the readBuf to the underlying buffer pool, and removes the
// goroutine's reference to the readBuf.
func (r *readWorkerState) Cleanup() {
	r.bufferPool.Return(r.readBuf)
	r.readBuf = nil
}

// Reset recycles the readBuf to make it ready for any subsequent tasks the
// goroutine may process.
func (r *readWorkerState) Reset() {
	r.readBuf.Recycle()
}
