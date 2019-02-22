package pool

import (
	"bytes"
	"time"

	"github.com/lightningnetwork/lnd/buffer"
)

// Write is a worker pool specifically designed for sharing access to
// buffer.Write objects amongst a set of worker goroutines. This enables an
// application to limit the total number of buffer.Write objects allocated at
// any given time.
type Write struct {
	workerPool *Worker
	bufferPool *WriteBuffer
}

// NewWrite creates a Write pool, using an underlying Writebuffer pool to
// recycle buffer.Write objects accross the lifetime of the Write pool's
// workers.
func NewWrite(writeBufferPool *WriteBuffer, numWorkers int,
	workerTimeout time.Duration) *Write {

	w := &Write{
		bufferPool: writeBufferPool,
	}
	w.workerPool = NewWorker(&WorkerConfig{
		NewWorkerState: w.newWorkerState,
		NumWorkers:     numWorkers,
		WorkerTimeout:  workerTimeout,
	})

	return w
}

// Start safely spins up the Write pool.
func (w *Write) Start() error {
	return w.workerPool.Start()
}

// Stop safely shuts down the Write pool.
func (w *Write) Stop() error {
	return w.workerPool.Stop()
}

// Submit accepts a function closure that provides access to a fresh
// bytes.Buffer backed by a buffer.Write object. The function's execution will
// be allocated to one of the underlying Worker pool's goroutines.
func (w *Write) Submit(inner func(*bytes.Buffer) error) error {
	return w.workerPool.Submit(func(s WorkerState) error {
		state := s.(*writeWorkerState)
		return inner(state.buf)
	})
}

// writeWorkerState is the per-goroutine state maintained by a Write pool's
// goroutines.
type writeWorkerState struct {
	// bufferPool is the pool to which the writeBuf will be returned when
	// the goroutine exits.
	bufferPool *WriteBuffer

	// writeBuf is the buffer taken from the bufferPool on initialization,
	// which will be used to back the buf object provided to any tasks that
	// the goroutine processes before exiting.
	writeBuf *buffer.Write

	// buf is a buffer backed by writeBuf, that can be written to by tasks
	// submitted to the Write pool. The buf will be reset between each task
	// processed by a goroutine before exiting, and allows the task
	// submitters to interact with the writeBuf as if it were an io.Writer.
	buf *bytes.Buffer
}

// newWorkerState initializes a new writeWorkerState, which will be called
// whenever a new goroutine is allocated to begin processing write tasks.
func (w *Write) newWorkerState() WorkerState {
	writeBuf := w.bufferPool.Take()

	return &writeWorkerState{
		bufferPool: w.bufferPool,
		writeBuf:   writeBuf,
		buf:        bytes.NewBuffer(writeBuf[0:0:len(writeBuf)]),
	}
}

// Cleanup returns the writeBuf to the underlying buffer pool, and removes the
// goroutine's reference to the readBuf and encapsulating buf.
func (w *writeWorkerState) Cleanup() {
	w.bufferPool.Return(w.writeBuf)
	w.writeBuf = nil
	w.buf = nil
}

// Reset resets the bytes.Buffer so that it is zero-length and has the capacity
// of the underlying buffer.Write.k
func (w *writeWorkerState) Reset() {
	w.buf.Reset()
}
