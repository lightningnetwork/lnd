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
