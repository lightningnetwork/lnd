package brontide

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestDialContextHappyPath verifies that DialContext establishes an
// encrypted connection when the context is not canceled.
func TestDialContextHappyPath(t *testing.T) {
	t.Parallel()

	listener, netAddr, err := makeListener()
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })

	remotePriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	remoteKeyECDH := &keychain.PrivKeyECDH{PrivKey: remotePriv}

	// Accept connections in a goroutine.
	acceptCh := make(chan net.Conn, 1)
	acceptErrCh := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErrCh <- err
			return
		}
		acceptCh <- conn
	}()

	ctx := t.Context()
	dialer := func(ctx context.Context, network,
		addr string) (net.Conn, error) {

		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}

	conn, err := DialContext(ctx, remoteKeyECDH, netAddr, dialer)
	require.NoError(t, err)
	require.NotNil(t, conn)
	t.Cleanup(func() { conn.Close() })

	// Wait for listener to accept.
	select {
	case accepted := <-acceptCh:
		t.Cleanup(func() { accepted.Close() })

	case err := <-acceptErrCh:
		require.NoError(t, err)

	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for listener accept")
	}
}

// TestDialContextCanceledBeforeDial verifies that DialContext returns an error
// when the context is already canceled before the TCP dial begins.
func TestDialContextCanceledBeforeDial(t *testing.T) {
	t.Parallel()

	remotePriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	remoteKeyECDH := &keychain.PrivKeyECDH{PrivKey: remotePriv}

	// Create a listener just to get a valid address.
	listener, netAddr, err := makeListener()
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })

	// Cancel the context before dialing.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	dialer := func(ctx context.Context, network,
		addr string) (net.Conn, error) {

		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}

	conn, err := DialContext(ctx, remoteKeyECDH, netAddr, dialer)
	require.ErrorContains(t, err, "operation was canceled")
	require.Nil(t, conn)
}

// TestDialContextCanceledDuringHandshake verifies that DialContext returns an
// error when the context is canceled after the TCP connection is established
// but during the Brontide handshake.
func TestDialContextCanceledDuringHandshake(t *testing.T) {
	t.Parallel()

	remotePriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	remoteKeyECDH := &keychain.PrivKeyECDH{PrivKey: remotePriv}

	// Use a raw TCP listener that accepts but never completes the
	// handshake. This causes the dialer to block on the handshake read,
	// giving us time to cancel the context.
	rawListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { rawListener.Close() })

	// Accept connections but never send handshake data.
	go func() {
		conn, err := rawListener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		// Hold the connection open until the test ends. The context
		// cancel will close the dialer side.
		<-t.Context().Done()
	}()

	serverPub, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	netAddr := &lnwire.NetAddress{
		IdentityKey: serverPub.PubKey(),
		Address:     rawListener.Addr().(*net.TCPAddr),
	}

	ctx, cancel := context.WithCancel(t.Context())
	dialer := func(ctx context.Context, network,
		addr string) (net.Conn, error) {

		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}

	// Cancel the context after a short delay to allow TCP connect to
	// succeed but interrupt the handshake.
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	conn, err := DialContext(ctx, remoteKeyECDH, netAddr, dialer)
	require.ErrorContains(t, err, "use of closed network connection")
	require.Nil(t, conn)
}
