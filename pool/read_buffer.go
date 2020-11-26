package pool

import (
	"time"

	"github.com/lightningnetwork/lnd/buffer"
)

const (
	// DefaultReadBufferGCInterval is the default interval that a Read will
	// perform a sweep to see which expired buffer.Reads can be released to
	// the runtime.
	DefaultReadBufferGCInterval = 15 * time.Second

	// DefaultReadBufferExpiryInterval is the default, minimum interval that
	// must elapse before a Read will release a buffer.Read. The maximum
	// time before the buffer can be released is equal to the expiry
	// interval plus the gc interval.
	DefaultReadBufferExpiryInterval = 30 * time.Second
)

// ReadBuffer is a pool of buffer.Read items, that dynamically allocates and
// reclaims buffers in response to load.
type ReadBuffer struct {
	pool *Recycle
}

// NewReadBuffer returns a freshly instantiated ReadBuffer, using the given
// gcInterval and expiryInterval.
func NewReadBuffer(gcInterval, expiryInterval time.Duration) *ReadBuffer {
	return &ReadBuffer{
		pool: NewRecycle(
			func() interface{} { return new(buffer.Read) },
			100, gcInterval, expiryInterval,
		),
	}
}

// Take returns a fresh buffer.Read to the caller.
func (p *ReadBuffer) Take() *buffer.Read {
	return p.pool.Take().(*buffer.Read)
}

// Return returns the buffer.Read to the pool, so that it can be cycled or
// released.
func (p *ReadBuffer) Return(buf *buffer.Read) {
	p.pool.Return(buf)
}
