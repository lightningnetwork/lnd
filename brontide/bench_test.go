package brontide

import (
	"bytes"
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
