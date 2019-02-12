package lnpeer

import (
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/queue"
)

const (
	// DefaultGCInterval is the default interval that the WriteBufferPool
	// will perform a sweep to see which expired buffers can be released to
	// the runtime.
	DefaultGCInterval = 15 * time.Second

	// DefaultExpiryInterval is the default, minimum interval that must
	// elapse before a WriteBuffer will be released. The maximum time before
	// the buffer can be released is equal to the expiry interval plus the
	// gc interval.
	DefaultExpiryInterval = 30 * time.Second
)

// WriteBuffer is static byte array occupying to maximum-allowed
// plaintext-message size.
type WriteBuffer [lnwire.MaxMessagePayload]byte

// Recycle zeroes the WriteBuffer, making it fresh for another use.
// Zeroing the buffer using a logarithmic number of calls to the optimized copy
// method. Benchmarking shows this to be ~30 times faster than a for loop that
// sets each index to 0 for this buffer size. Inspired by:
// https://stackoverflow.com/questions/30614165/is-there-analog-of-memset-in-go
//
// This is part of the queue.Recycler interface.
func (b *WriteBuffer) Recycle() {
	b[0] = 0
	for i := 1; i < lnwire.MaxMessagePayload; i *= 2 {
		copy(b[i:], b[:i])
	}
}

// newRecyclableWriteBuffer is a constructor that returns a WriteBuffer typed as
// a queue.Recycler.
func newRecyclableWriteBuffer() queue.Recycler {
	return new(WriteBuffer)
}

// A compile-time constraint to ensure that *WriteBuffer implements the
// queue.Recycler interface.
var _ queue.Recycler = (*WriteBuffer)(nil)

// WriteBufferPool acts a global pool of WriteBuffers, that dynamically
// allocates and reclaims buffers in response to load.
type WriteBufferPool struct {
	pool *queue.GCQueue
}

// NewWriteBufferPool returns a freshly instantiated WriteBufferPool, using the
// given gcInterval and expiryIntervals.
func NewWriteBufferPool(
	gcInterval, expiryInterval time.Duration) *WriteBufferPool {

	return &WriteBufferPool{
		pool: queue.NewGCQueue(
			newRecyclableWriteBuffer, 100,
			gcInterval, expiryInterval,
		),
	}
}

// Take returns a fresh WriteBuffer to the caller.
func (p *WriteBufferPool) Take() *WriteBuffer {
	return p.pool.Take().(*WriteBuffer)
}

// Return returns the WriteBuffer to the pool, so that it can be recycled or
// released.
func (p *WriteBufferPool) Return(buf *WriteBuffer) {
	p.pool.Return(buf)
}
