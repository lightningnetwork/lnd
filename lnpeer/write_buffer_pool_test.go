package lnpeer_test

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnpeer"
)

// TestWriteBufferPool verifies that buffer pool properly resets used write
// buffers.
func TestWriteBufferPool(t *testing.T) {
	const (
		gcInterval     = time.Second
		expiryInterval = 250 * time.Millisecond
	)

	bp := lnpeer.NewWriteBufferPool(gcInterval, expiryInterval)

	// Take a fresh write buffer from the pool.
	writeBuf := bp.Take()

	// Dirty the write buffer.
	for i := range writeBuf[:] {
		writeBuf[i] = 0xff
	}

	// Return the buffer to the pool.
	bp.Return(writeBuf)

	// Take buffers from the pool until we find the original. We expect at
	// most two, in the even that a fresh buffer is populated after the
	// first is taken.
	for i := 0; i < 2; i++ {
		// Wait a small duration to ensure the tests behave reliable,
		// and don't activate the non-blocking case unintentionally.
		<-time.After(time.Millisecond)

		// Take a buffer, skipping those whose pointer does not match
		// the one we dirtied.
		writeBuf2 := bp.Take()
		if writeBuf2 != writeBuf {
			continue
		}

		// Finally, verify that the buffer has been properly cleaned.
		for i := range writeBuf2[:] {
			if writeBuf2[i] != 0 {
				t.Fatalf("buffer was not recycled")
			}
		}

		return
	}

	t.Fatalf("original buffer not found")
}

// BenchmarkWriteBufferRecycle tests how quickly a WriteBuffer can be zeroed.
func BenchmarkWriteBufferRecycle(b *testing.B) {
	b.ReportAllocs()

	buffer := new(lnpeer.WriteBuffer)
	for i := 0; i < b.N; i++ {
		buffer.Recycle()
	}
}
