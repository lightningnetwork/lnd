package proxy_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/brontide"
	"github.com/lightningnetwork/lnd/brontide/proxy"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/pool"
	"github.com/stretchr/testify/require"
)

const (
	// p2pPrivStr is the hex private key of the node in a test context.
	// It is also copied to the proxy so it can pretend to be the node.
	p2pPrivStr = "677f456999a1ec3db53f4cb51e64db15941a37aaeb67eaed018bb6f637a933ed"

	// from the node.
	proxyPrivStr = "7568a50fe45d3681e66e915a1ded0e39e92a512f8278fae6540e57868533c093"

	// peer1PrivStr is the hex private key of a peer that connects to the
	// proxy as if it's connecting directly to the node.
	peer1PrivStr = "7568a507345d3681e66e915a1ded0e39e92a512f8278fae6540e57868533c093"

	// peer1PrivStr is the hex private key of a peer that connects to the
	// proxy as if it's connecting directly to the node.
	peer2PrivStr = "7568a50fe81d3681e66e915a1ded0e39e92a512f8278fae6540e57868533c093"
)

// proxyTestContext is a struct which holds required structs for running proxy
// tests.
type proxyTestContext struct {
	// Inherit from context.Context.
	context.Context

	// cancel is the cancel function for this context.
	cancel context.CancelFunc

	// t is the test context
	t *testing.T

	// p2pAddrStr is the address on which the proxy listens for p2p
	// connections.
	p2pAddrStr string

	// proxyAddrStr is the address on which the proxy listens for a
	// connection from the node.
	proxyAddrStr string

	// p2pPrivKey is the node private key which is also exported to the
	// proxy for impersonation.
	p2pPrivKey *btcec.PrivateKey

	// p2pPubKey is the node public key derived from the private key above.
	// It is used for the node to identify itself to the proxy and for the
	// proxy to impersonate the node to peers.
	p2pPubKey *btcec.PublicKey

	// proxyPrivKey is the proxy private key used for communication with
	// the node.
	proxyPrivKey *btcec.PrivateKey

	// proxyPubKey is the proxy public key derived from the private key
	// above. It is used for the proxy to identify itself to the node so
	// the node can trust it to proxy traffic.
	proxyPubKey *btcec.PublicKey

	// p2pPrivECDH is used to connect the node mux to the proxy mux
	// as well as start the p2pListener below.
	p2pPrivECDH keychain.SingleKeyECDH

	// p2pListener listens for connections from peers. It impersonates
	// a listener that the node would bring up by using the node key.
	p2pListener *brontide.Listener

	// proxyListener listens for mux connections from the node. It uses its
	// own keypair to identify itself to the node.
	proxyListener *brontide.Listener

	// p2pNetAddr represents the network address, including public key,
	// IP address/hostname, and TCP port, for peer connections.
	p2pNetAddr *lnwire.NetAddress

	// proxyNetAddr represents the network address, including public key,
	// IP address/hostname, and TCP port, for the node connection to the
	// proxy.
	proxyNetAddr *lnwire.NetAddress

	// readPool is a read buffer pool.
	readPool *pool.Read

	// proxyMux runs on the proxy side. It starts with a connection from
	// the node on the proxyListener, and then connections are added to
	// it as they're accepted on p2pListener.
	proxyMux *proxy.Mux

	// nodeMux runs on the node side. It connects to the proxyMux using
	// a single brontide connection. All of the peer connections are
	// multiplexed over the single connection between the node and proxy.
	nodeMux *proxy.Mux
}

// newProxyTestContext creates a new proxy test context and populates it with
// an initial node and proxy mux. It starts them running but does not create
// any peers.
func newProxyTestContext(t *testing.T) (*proxyTestContext, error) {
	t.Helper()

	// Create context with a cancel function.
	ctx, cancel := context.WithCancel(context.Background())

	// Generate random ports to listen on.
	p2pAddrStr := fmt.Sprintf("localhost:%d", lntest.NextAvailablePort())
	proxyAddrStr := fmt.Sprintf("localhost:%d", lntest.NextAvailablePort())

	// Get keys from hex constants.
	p2pPrivKey, p2pPubKey, err := hexToKey(p2pPrivStr)
	require.NoError(t, err, "error parsing p2p key")

	proxyPrivKey, proxyPubKey, err := hexToKey(proxyPrivStr)
	require.NoError(t, err, "error parsing proxy key")

	// Wrap keys for ECDH.
	var p2pPrivECDH keychain.SingleKeyECDH = &keychain.PrivKeyECDH{
		PrivKey: p2pPrivKey,
	}

	var proxyPrivECDH keychain.SingleKeyECDH = &keychain.PrivKeyECDH{
		PrivKey: proxyPrivKey,
	}

	// Start the listeners on the required ports.
	p2pListener, err := brontide.NewListener(p2pPrivECDH, p2pAddrStr)
	require.NoError(t, err, "couldn't start p2p listener")

	proxyListener, err := brontide.NewListener(proxyPrivECDH, proxyAddrStr)
	require.NoError(t, err, "couldn't start proxy listener")

	// Construct the network addresses for testing.
	p2pNetAddr := &lnwire.NetAddress{
		IdentityKey: p2pPubKey,
		Address:     p2pListener.Addr(),
		ChainNet:    wire.SimNet,
	}

	proxyNetAddr := &lnwire.NetAddress{
		IdentityKey: proxyPubKey,
		Address:     proxyListener.Addr(),
		ChainNet:    wire.SimNet,
	}

	// Make a read buffer pool.
	readPool := pool.NewRead(
		pool.NewReadBuffer(
			pool.DefaultReadBufferGCInterval,
			pool.DefaultReadBufferExpiryInterval,
		),
		5,
		pool.DefaultWorkerTimeout,
	)

	err = readPool.Start()
	require.NoError(t, err, "couldn't start read pool")

	// Construct the test context object.
	c := &proxyTestContext{
		ctx,
		cancel,
		t,
		p2pAddrStr,
		proxyAddrStr,
		p2pPrivKey,
		p2pPubKey,
		proxyPrivKey,
		proxyPubKey,
		p2pPrivECDH,
		p2pListener,
		proxyListener,
		p2pNetAddr,
		proxyNetAddr,
		readPool,
		nil,
		nil,
	}

	// Now we start the handler goroutines and return the test
	// context.
	return c, c.handleMuxes()
}

// handleMuxes connects and starts the node and proxy muxes, and starts a
// goroutine that accepts connections over the p2pListener and adds them to
// the proxyMux, thus signaling the nodeMux. Dialing the p2pListener and
// accepting nodeMux connections is left up to tests themselves.
func (c *proxyTestContext) handleMuxes() error {
	// doneChan lets goroutines signal when their mux is set up
	doneChan := make(chan struct{})

	// Start a goroutine to handle the proxy mux.
	go func() {
		// First, accept a connection from the node.
		proxyMuxConn, err := c.proxyListener.Accept()
		require.NoError(c.t, err,
			"accepting proxyMux connection failed")

		// Cast as *brontide.Conn
		brontideConn, ok := proxyMuxConn.(*brontide.Conn)
		require.True(c.t, ok, "couldn't cast proxyMuxConn as "+
			"*brontide.Conn")

		// Authenticate the node connecting to the proxy.
		require.Equal(c.t, brontideConn.RemotePub(), c.p2pPubKey)

		// Spin up the proxy mux.
		c.proxyMux = proxy.NewMux(c, c.readPool, brontideConn)

		err = c.proxyMux.Start()
		require.NoError(c.t, err, "starting proxyMux failed")

		// Signal the proxy mux is started.
		select {
		case doneChan <- struct{}{}:
		case <-c.Done():
			return
		}

		// Accept connections on the p2pListener and add them to the
		// proxy mux. The proxy mux will signal the node mux about
		// the new connection.
		for {
			// Accept a connection.
			p2pConn, err := c.p2pListener.Accept()

			// If the listener is closed, it's time to exit. Any
			// other error is a failure.
			if err != nil && err.Error() ==
				"brontide connection closed" {

				return
			}

			require.NoError(c.t, err, "accepting p2p "+
				"connection failed")

			// Add it to the mux.
			err = c.proxyMux.Add(p2pConn.(*brontide.Conn))
			require.NoError(c.t, err, "adding p2p connection to "+
				"proxyMux failed")
		}
	}()

	// Dial the proxyListener and set up the node mux on the resulting
	// connection. This automatically authenticates the pubkey of the
	// proxyListener.
	nodeMuxConn, err := brontide.Dial(c.p2pPrivECDH, c.proxyNetAddr,
		time.Second, net.DialTimeout)
	require.NoError(c.t, err, "connecting to proxy mux failed")

	c.nodeMux = proxy.NewMux(c, c.readPool, nodeMuxConn)

	err = c.nodeMux.Start()
	require.NoError(c.t, err, "starting nodeMux failed")

	// Ensure the proxyMux has also started.
	select {
	case <-doneChan:

	case <-c.Done():
		return fmt.Errorf("context ended early")
	}

	return nil
}

// stop cleanly stops all the goroutines under the proxyTestContext.
func (c *proxyTestContext) stop() {
	c.cancel()

	c.proxyListener.Close()

	c.p2pListener.Close()

	c.proxyMux.Close()

	c.nodeMux.Close()

	require.NoError(c.t, c.readPool.Stop())
}

// TestProxy creates a test context with a node mux and proxy mux, connects
// two peers to the proxy, and ensures the node sees them connect and can
// send data to and receive data from them.
func TestProxy(t *testing.T) {
	// Create the proxy test context.
	c, err := newProxyTestContext(t)

	require.NoError(t, err)
	require.NotNil(t, c)

	defer c.stop()

	// Derive keys for the peers from their hex constants.
	peer1PrivKey, peer1PubKey, err := hexToKey(peer1PrivStr)
	require.NoError(t, err, "error parsing peer1 key")

	peer2PrivKey, peer2PubKey, err := hexToKey(peer2PrivStr)
	require.NoError(t, err, "error parsing peer2 key")

	// Wrap keys for ECDH.
	var peer1PrivECDH keychain.SingleKeyECDH = &keychain.PrivKeyECDH{
		PrivKey: peer1PrivKey,
	}

	var peer2PrivECDH keychain.SingleKeyECDH = &keychain.PrivKeyECDH{
		PrivKey: peer2PrivKey,
	}

	doneChan := make(chan struct{})

	var peer1Conn, peer2Conn *brontide.Conn
	var nodePeer1Conn, nodePeer2Conn *proxy.Conn

	// Accept two connections at the node, one per peer, in a goroutine.
	go func() {
		conn, err := c.nodeMux.Accept()
		require.NoError(t, err, "accepting nodePeer1Conn failed")

		nodePeer1Conn = conn.(*proxy.Conn)

		conn, err = c.nodeMux.Accept()
		require.NoError(t, err, "accepting nodePeer2Conn failed")

		nodePeer2Conn = conn.(*proxy.Conn)

		select {
		case doneChan <- struct{}{}:

		case <-c.Done():
		}
	}()

	// Connect the first peer to the proxy, which should allow the above
	// goroutine to populate nodePeer1.
	peer1Conn, err = brontide.Dial(peer1PrivECDH, c.p2pNetAddr,
		time.Second, net.DialTimeout)
	require.NoError(t, err, "connecting peer 1 to p2p proxy failed")
	defer peer1Conn.Close()

	// Connect the second peer to the proxy, which should allow the above
	// goroutine to populate nodePeer2.
	peer2Conn, err = brontide.Dial(peer2PrivECDH, c.p2pNetAddr,
		time.Second, net.DialTimeout)
	require.NoError(t, err, "connecting peer 1 to p2p proxy failed")
	defer peer2Conn.Close()

	// Await for nodePeer1 and nodePeer2 to populate.
	select {
	case <-doneChan:

	case <-c.Done():
		t.Fatalf("context ended early")
	}

	defer nodePeer1Conn.Close()
	defer nodePeer2Conn.Close()

	// Ensure everyone's got each other's correct public keys.
	require.Equal(t, *(peer1Conn.LocalPub()),
		*(nodePeer1Conn.RemotePub()), *peer1PubKey)
	require.Equal(t, *(peer2Conn.LocalPub()),
		*(nodePeer2Conn.RemotePub()), *peer2PubKey)

	// Send test messages between the node and each peer through the proxy.
	//
	// TODO(aakselrod): Test reconnections, large packets, etc.
	testCases := []struct {
		from proxy.MessageConnWithPubkey
		to   proxy.MessageConnWithPubkey
	}{
		{
			// From node to peer 1.
			from: nodePeer1Conn,
			to:   peer1Conn,
		},
		{
			// From peer 1 to node.
			from: peer1Conn,
			to:   nodePeer1Conn,
		},
		{
			// From node to peer 2.
			from: nodePeer2Conn,
			to:   peer2Conn,
		},
		{
			// From peer 2 to node.
			from: peer2Conn,
			to:   nodePeer2Conn,
		},
	}

	for i, testCase := range testCases {
		// Create a unique string for data to send across the wire.
		buf := []byte(fmt.Sprintf("case %d", i))

		// Set up a goroutine to receive a message and check that
		// it's the one we expect.
		go func() {
			// Receive the data.
			n, err := testCase.to.ReadNextHeader()
			require.NoError(t, err, "couldn't read header")

			buf2 := make([]byte, n)
			buf2, err = testCase.to.ReadNextBody(buf2)
			require.NoError(t, err, "couldn't read body")

			// Make sure it's the data we expected.
			require.Equal(t, buf, buf2)

			// Signal that we're done and exit.
			select {
			case doneChan <- struct{}{}:

			case <-c.Done():
			}
		}()

		// Write a message and flush it.
		err := testCase.from.WriteMessage(buf)
		require.NoError(t, err, "couldn't write message")

		_, err = testCase.from.Flush()
		require.NoError(t, err, "couldn't flush")

		// Wait for the message to be received and checked.
		select {
		case <-doneChan:

		case <-c.Done():
			return
		}
	}
}

// hexToKey returns a private key decoded from a hex string, a public key
// derived from the private key, and an error if unsuccessful.
func hexToKey(keyStr string) (*btcec.PrivateKey, *btcec.PublicKey, error) {
	keyBytes, err := hex.DecodeString(keyStr)
	if err != nil {
		return nil, nil, err
	}

	privKey, pubKey := btcec.PrivKeyFromBytes(keyBytes)
	return privKey, pubKey, nil
}
