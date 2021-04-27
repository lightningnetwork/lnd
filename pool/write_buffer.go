package pool

import (
	"time"

	"github.com/lightningnetwork/lnd/buffer"
)

const (
	// DefaultWriteBufferGCInterval is the default interval that a Write
	// will perform a sweep to see which expired buffer.Writes can be
	// released to the runtime.
	DefaultWriteBufferGCInterval = 15 * time.Second

	// DefaultWriteBufferExpiryInterval is the default, minimum interval
	// that must elapse before a Write will release a buffer.Write. The
	// maximum time before the buffer can be released is equal to the expiry
	// interval plus the gc interval.
	DefaultWriteBufferExpiryInterval = 30 * time.Second
)

// WriteBuffer is a pool of recycled buffer.Write items, that dynamically
// allocates and reclaims buffers in response to load.
type WriteBuffer struct {
	pool *Recycle
}

// NewWriteBuffer returns a freshly instantiated WriteBuffer, using the given
// gcInterval and expiryIntervals.
func NewWriteBuffer(gcInterval, expiryInterval time.Duration) *WriteBuffer {
	return &WriteBuffer{
		pool: NewRecycle(
			func() interface{} { return new(buffer.Write) },
			100, gcInterval, expiryInterval,
		),
	}
}

// Take returns a fresh buffer.Write to the caller.
func (p *WriteBuffer) Take() *buffer.Write {
	return p.pool.Take().(*buffer.Write)
}

// Return returns the buffer.Write to the pool, so that it can be recycled or
// released.
func (p *WriteBuffer) Return(buf *buffer.Write) {
	p.pool.Return(buf)
}
