package brontide

import (
	"bytes"
	"io"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func BenchmarkReadHeaderAndBody(t *testing.B) {
	// Create a test connection, grabbing either side of the connection
	// into local variables. If the initial crypto handshake fails, then
	// we'll get a non-nil error here.
	localConn, remoteConn, err := establishTestConnection(t)
	require.NoError(t, err, "unable to establish test connection: %v", err)

	rand.Seed(time.Now().Unix())

	noiseRemoteConn := remoteConn.(*Conn)
	noiseLocalConn := localConn.(*Conn)

	// Now that we have a local and remote side (to set up the initial
	// handshake state, we'll have the remote side write out something
	// similar to a large message in the protocol.
	const pktSize = 60_000
	msg := bytes.Repeat([]byte("a"), pktSize)
	err = noiseRemoteConn.WriteMessage(msg)
	require.NoError(t, err, "unable to write encrypted message: %v", err)

	cipherHeader := noiseRemoteConn.noise.nextHeaderSend
	cipherMsg := noiseRemoteConn.noise.nextBodySend

	var (
		benchErr error
		msgBuf   [math.MaxUint16]byte
	)

	t.ReportAllocs()
	t.ResetTimer()

	nonceValue := noiseLocalConn.noise.recvCipher.nonce
	for i := 0; i < t.N; i++ {
		pktLen, benchErr := noiseLocalConn.noise.ReadHeader(
			bytes.NewReader(cipherHeader),
		)
		require.NoError(
			t, benchErr, "#%v: failed decryption: %v", i, benchErr,
		)
		_, benchErr = noiseLocalConn.noise.ReadBody(
			bytes.NewReader(cipherMsg), msgBuf[:pktLen],
		)
		require.NoError(
			t, benchErr, "#%v: failed decryption: %v", i, benchErr,
		)

		// We reset the internal nonce each time as otherwise, we'd
		// continue to increment it which would cause a decryption
		// failure.
		noiseLocalConn.noise.recvCipher.nonce = nonceValue
	}
	require.NoError(t, benchErr)
}

// BenchmarkWriteMessage benchmarks the performance of writing a maximum-sized
// message and flushing it to an io.Discard to measure the allocation and CPU
// overhead of the encryption and writing logic.
func BenchmarkWriteMessage(b *testing.B) {
	localConn, remoteConn, err := establishTestConnection(b)
	require.NoError(b, err, "unable to establish test connection: %v", err)

	noiseLocalConn, ok := localConn.(*Conn)
	require.True(b, ok, "expected *Conn type for localConn")

	// Create the largest possible message we can write (MaxUint16 bytes).
	// This is the maximum message size allowed by the protocol.
	const maxMsgSize = math.MaxUint16
	largeMsg := bytes.Repeat([]byte("a"), maxMsgSize)

	// Use io.Discard to simulate writing to a network connection that
	// continuously accepts data without needing resets.
	discard := io.Discard

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Write our massive message, then call flush to actually write
		// the encrypted message This simulates a full write operation
		// to a network.
		err := noiseLocalConn.noise.WriteMessage(largeMsg)
		if err != nil {
			b.Fatalf("WriteMessage failed: %v", err)
		}
		_, err = noiseLocalConn.noise.Flush(discard)
		if err != nil {
			b.Fatalf("Flush failed: %v", err)
		}
	}

	// We'll make sure to clean up the connections at the end of the
	// benchmark.
	b.Cleanup(func() {
		localConn.Close()
		remoteConn.Close()
	})
}

// BenchmarkWriteMessageSizes benchmarks WriteMessage performance across
// different message sizes to measure the effectiveness of the tiered buffer
// pool. Each sub-benchmark tests a different message size tier.
func BenchmarkWriteMessageSizes(b *testing.B) {
	// Define the message sizes to benchmark. These correspond to the
	// tiered buffer pool tiers plus some intermediate sizes.
	sizes := []struct {
		name string
		size int
	}{
		{"100B", 100},   // Tiny message (256B tier)
		{"256B", 256},   // At 256B tier boundary
		{"500B", 500},   // Small message (1KB tier)
		{"1KB", 1024},   // At 1KB tier boundary
		{"2KB", 2048},   // Medium message (4KB tier)
		{"4KB", 4096},   // At 4KB tier boundary
		{"8KB", 8192},   // Large message (64KB tier)
		{"32KB", 32768}, // Large message
		{"64KB", 65535}, // Max message size
	}

	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			localConn, remoteConn, err := establishTestConnection(b)
			require.NoError(b, err, "unable to establish test connection")

			noiseLocalConn, ok := localConn.(*Conn)
			require.True(b, ok, "expected *Conn type for localConn")

			msg := bytes.Repeat([]byte("x"), sz.size)
			discard := io.Discard

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				err := noiseLocalConn.noise.WriteMessage(msg)
				if err != nil {
					b.Fatalf("WriteMessage failed: %v", err)
				}
				_, err = noiseLocalConn.noise.Flush(discard)
				if err != nil {
					b.Fatalf("Flush failed: %v", err)
				}
			}

			b.Cleanup(func() {
				localConn.Close()
				remoteConn.Close()
			})
		})
	}
}

// BenchmarkTieredBufferPoolTakeAndPut benchmarks the overhead of the tiered
// buffer pool's Take and Put operations.
func BenchmarkTieredBufferPoolTakeAndPut(b *testing.B) {
	pool := newTieredBufferPool()

	sizes := []struct {
		name string
		size int
	}{
		{"256B_tier", 100},
		{"1KB_tier", 500},
		{"4KB_tier", 2000},
		{"64KB_tier", 50000},
	}

	for _, sz := range sizes {
		b.Run(sz.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				buf := pool.Take(sz.size)
				pool.Put(buf)
			}
		})
	}
}
