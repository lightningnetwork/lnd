package brontide

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTierForSize verifies that tierForSize returns the correct tier for
// various plaintext sizes.
func TestTierForSize(t *testing.T) {
	tests := []struct {
		name          string
		plaintextSize int
		expectedTier  int
	}{
		{
			name:          "zero size",
			plaintextSize: 0,
			expectedTier:  tier256Size,
		},
		{
			name:          "tiny message",
			plaintextSize: 10,
			expectedTier:  tier256Size,
		},
		{
			name:          "at 256B boundary (minus MAC)",
			plaintextSize: 256,
			expectedTier:  tier256Size,
		},
		{
			name:          "just over 256B tier",
			plaintextSize: 257,
			expectedTier:  tier1KSize,
		},
		{
			name:          "at 1KB boundary (minus MAC)",
			plaintextSize: 1024,
			expectedTier:  tier1KSize,
		},
		{
			name:          "just over 1KB tier",
			plaintextSize: 1025,
			expectedTier:  tier4KSize,
		},
		{
			name:          "at 4KB boundary (minus MAC)",
			plaintextSize: 4096,
			expectedTier:  tier4KSize,
		},
		{
			name:          "just over 4KB tier",
			plaintextSize: 4097,
			expectedTier:  tier64KSize,
		},
		{
			name:          "large message",
			plaintextSize: 32000,
			expectedTier:  tier64KSize,
		},
		{
			name:          "max size message",
			plaintextSize: 65535,
			expectedTier:  tier64KSize,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tier := tierForSize(tc.plaintextSize)
			require.Equal(t, tc.expectedTier, tier,
				"unexpected tier for size %d", tc.plaintextSize)
		})
	}
}

// TestTieredBufferPoolTakeAndPut verifies that the tiered buffer pool
// correctly allocates and returns buffers of appropriate sizes.
func TestTieredBufferPoolTakeAndPut(t *testing.T) {
	pool := newTieredBufferPool()

	tests := []struct {
		name          string
		plaintextSize int
		expectedCap   int
	}{
		{
			name:          "tiny message gets 256B buffer",
			plaintextSize: 50,
			expectedCap:   tier256Size,
		},
		{
			name:          "medium-small message gets 1KB buffer",
			plaintextSize: 500,
			expectedCap:   tier1KSize,
		},
		{
			name:          "medium message gets 4KB buffer",
			plaintextSize: 2000,
			expectedCap:   tier4KSize,
		},
		{
			name:          "large message gets 64KB buffer",
			plaintextSize: 50000,
			expectedCap:   tier64KSize,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Take a buffer from the pool.
			buf := pool.Take(tc.plaintextSize)
			require.NotNil(t, buf)

			// Verify the buffer has zero length.
			require.Equal(t, 0, len(*buf),
				"buffer should have zero length")

			// Verify the buffer has the expected tier capacity.
			require.Equal(t, tc.expectedCap, cap(*buf),
				"buffer capacity should match expected tier size")

			// Simulate using the buffer.
			*buf = append(*buf, make([]byte, tc.plaintextSize+macSize)...)

			// Return the buffer to the pool.
			pool.Put(buf)
		})
	}
}

// TestTieredBufferPoolReuse verifies that buffers are properly reused when
// returned to the pool.
func TestTieredBufferPoolReuse(t *testing.T) {
	pool := newTieredBufferPool()

	// Take a buffer, use it, and put it back.
	buf1 := pool.Take(100) // Should get 256B tier
	require.NotNil(t, buf1)

	// Write some data to the buffer.
	*buf1 = append(*buf1, []byte("test data")...)
	require.Greater(t, len(*buf1), 0)

	// Store the capacity for later comparison.
	bufCap1 := cap(*buf1)

	// Return the buffer.
	pool.Put(buf1)

	// Take another buffer of the same size tier.
	buf2 := pool.Take(100) // Should get 256B tier again
	require.NotNil(t, buf2)

	// The buffer should be reset to zero length.
	require.Equal(t, 0, len(*buf2),
		"reused buffer should have zero length")

	// The capacity should match the tier.
	require.Equal(t, bufCap1, cap(*buf2),
		"reused buffer should have same capacity")
}

// TestTieredBufferPoolNilPut verifies that Put handles nil buffers gracefully.
func TestTieredBufferPoolNilPut(t *testing.T) {
	pool := newTieredBufferPool()

	// This should not panic.
	pool.Put(nil)
}

// TestTieredBufferPoolConcurrency tests that the pool handles concurrent
// access safely.
func TestTieredBufferPoolConcurrency(t *testing.T) {
	pool := newTieredBufferPool()
	const goroutines = 100
	const iterations = 100

	done := make(chan bool, goroutines)

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			for j := 0; j < iterations; j++ {
				// Vary the size to hit different tiers.
				size := (id*j)%math.MaxUint16 + 1
				buf := pool.Take(size)

				// Simulate some work.
				*buf = append(*buf, byte(id), byte(j))

				pool.Put(buf)
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines to complete.
	for i := 0; i < goroutines; i++ {
		<-done
	}
}
