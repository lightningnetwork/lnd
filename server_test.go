// +build !rpctest

package main

import (
	"testing"
	"github.com/roasbeef/btcd/btcec"
	"github.com/lightningnetwork/lnd/lnwire"
	"net"
	"time"
	"github.com/stretchr/testify/require"
	"encoding/hex"
	"github.com/lightningnetwork/lnd/brontide"
	"github.com/roasbeef/btcd/connmgr"
	"fmt"
	"strings"
	"io"
)

var (
	privKey = createPrivateKey()
	pubKey = createPubKey()
	compressedPubKey = string(pubKey.SerializeCompressed())
	address = lnwire.NetAddress{IdentityKey: pubKey, Address: &StubAddress{}}
)

// Non-permanent connects should result in the peer being added to
// peersByPub and inboundPeers.
func TestConnectToPeer_NonPerm(t *testing.T) {
	srv := createServer()
	err := srv.ConnectToPeer(&address, false)

	require.NoError(t, err)

	require.NotNil(t, srv.peersByPub[compressedPubKey], "Peer not added")
	require.NotNil(t, srv.inboundPeers[compressedPubKey], "Peer not added")

	require.Equal(t, 0, len(srv.persistentPeers),
		"No persistent peers should be present.")
}

// Permanent connects should result in additions to
// persistentPeers, persistentPeersBackoff, and persistentConnReqs.
func TestConnectToPeer_Perm(t *testing.T) {
	srv := createServer()
	err := srv.ConnectToPeer(&address, true)

	require.NoError(t, err)

	// Interestingly, when perm is specified, the peer does NOT
	// end up in peersByPub nor inboundPeers, which seems likely to be a bug
	// since it is inconsistent with non-perm scenarios.
	require.Nil(t, srv.peersByPub[compressedPubKey])
	require.Nil(t, srv.inboundPeers[compressedPubKey])

	require.NotNil(t, srv.persistentPeers[compressedPubKey], "Peer not added")
	require.Equal(t, time.Duration(1000000000) , srv.persistentPeersBackoff[compressedPubKey], "Peer backoff not added")
	require.NotNil(t, srv.persistentConnReqs[compressedPubKey], "ConnReq not added")
}

// Permanent connects should result in additions to
// persistentPeers, persistentPeersBackoff, and persistentConnReqs.
// The persistentPeerBackoff entry should not be the default, but rather the
// value that was explicitly set.
func TestConnectToPeer_PermOverrideBackoff(t *testing.T) {
	srv := createServer()
	srv.persistentPeersBackoff[compressedPubKey] = 5
	err := srv.ConnectToPeer(&address, true)

	require.NoError(t, err)

	// Interestingly, when perm is specified, the peer does NOT
	// end up in peersByPub nor inboundPeers, which seems likely to be a bug
	// since it is inconsistent with non-perm scenarios.
	require.Nil(t, srv.peersByPub[compressedPubKey])
	require.Nil(t, srv.inboundPeers[compressedPubKey])

	require.NotNil(t, srv.persistentPeers[compressedPubKey],
		"Peer not added")
	require.Equal(t, time.Duration(5), srv.persistentPeersBackoff[compressedPubKey],
		"Peer backoff not added")
	require.NotNil(t, srv.persistentConnReqs[compressedPubKey],
		"ConnReq not added")
}

// An error should occur if a peer is already connected.
func TestConnectToPeer_AlreadyConnected(t *testing.T) {
	srv := createServer()
	srv.peersByPub[compressedPubKey] = &peer{}
	err := srv.ConnectToPeer(&address, false)

	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "already connected"))
}

// Attempting to connect again while there is already a pending attempt
// should not cause an error.
func TestConnectToPeer_PendingConnectionAttempt(t *testing.T) {
	srv := createServer()
	srv.persistentConnReqs[compressedPubKey] = nil
	err := srv.ConnectToPeer(&address, false)

	require.NoError(t, err)

	require.NotNil(t, srv.peersByPub[compressedPubKey], "Peer not added")
	require.NotNil(t, srv.inboundPeers[compressedPubKey], "Peer not added")

	require.Equal(t, 0, len(srv.persistentPeers),
		"No persistent peers should be present.")
}

// If the Dial fails, the error should be propagated up.
func TestConnectToPeer_DialFailed(t *testing.T) {
	testConnectToPeerFailedDial(t, io.ErrClosedPipe, io.ErrClosedPipe)
}

// If the Dial fails with an EOF, it should be replaced with a friendly message.
func TestConnectToPeer_DialFailedEOF(t *testing.T) {
	testConnectToPeerFailedDial(t, io.EOF, ErrFriendlyEOF)
}

func testConnectToPeerFailedDial(t *testing.T, dialErr error, expectedErr error) {
	srv := createServer()
	srv.connDialer = &FailingStubDialer{dialErr}
	err := srv.ConnectToPeer(&address, false)

	require.Error(t, err)
	require.Equal(t, expectedErr, err)
}

func TestParseHexColor(t *testing.T) {
	empty := ""
	color, err := parseHexColor(empty)
	if err == nil {
		t.Fatalf("Empty color string should return error, but did not")
	}

	tooLong := "#1234567"
	color, err = parseHexColor(tooLong)
	if err == nil {
		t.Fatalf("Invalid color string %s should return error, but did not",
			tooLong)
	}

	invalidFormat := "$123456"
	color, err = parseHexColor(invalidFormat)
	if err == nil {
		t.Fatalf("Invalid color string %s should return error, but did not",
			invalidFormat)
	}

	valid := "#C0FfeE"
	color, err = parseHexColor(valid)
	if err != nil {
		t.Fatalf("Color %s valid to parse: %s", valid, err)
	}
	if color.R != 0xc0 || color.G != 0xff || color.B != 0xee {
		t.Fatalf("Color %s incorrectly parsed as %v", valid, color)
	}
}

// createServer makes a server that performs no IO nor concurrent operations,
// making unit testing possible, unlike a standard server.
func createServer() *server {
	srv := server{}
	var err error
	srv.connMgr, err = connmgr.New(
		&connmgr.Config{Dial: func(address net.Addr) (net.Conn, error) {
			return nil, nil

		}})
	if err != nil {
		fmt.Println("Failed to create connMgr", err)
		return nil
	}
	srv.connMgr.Stop()

	srv.peersByPub = make(map[string]*peer)
	srv.inboundPeers = make(map[string]*peer)
	srv.persistentPeers = make(map[string]struct{})
	srv.persistentPeersBackoff = make(map[string]time.Duration)
	srv.persistentConnReqs = make(map[string][]*connmgr.ConnReq)
	srv.identityPriv = privKey
	srv.connDialer = &StubDialer{}
	srv.peerCreator = &StubPeerCreator{}
	srv.globalFeatures = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(),
		lnwire.GlobalFeatures)
	return &srv
}

func createPubKey() *btcec.PublicKey {
	pubkeyHex,_ := hex.DecodeString("028dfe1c8b9bfd7a7f8627a39a3b7b3a13d878a8b65dd26b17ca4f70a112a6dd54")
	pubKey, _ := btcec.ParsePubKey(pubkeyHex, btcec.S256())
	pubKey.Curve = nil
	return pubKey
}

func createPrivateKey() *btcec.PrivateKey {
	e := "121212121212121212121212121212121212121212121212121212" +
		"1212121212"
	eBytes, _ := hex.DecodeString(e)

	privKey, _ := btcec.PrivKeyFromBytes(btcec.S256(), eBytes)

	return privKey
}

// StubAddress is a minimal Address to be used for testing purposes.
type StubAddress struct {

}

func (a *StubAddress) Network() string {
	return "StubNetwork"
}

func (a *StubAddress) String() string {
	return "173.249.37.208:9735"
}

// StubDialer is a minimal Dialer to be used for testing purposes.
type StubDialer struct {

}

func (d *StubDialer) Dial(addressess *lnwire.NetAddress) (*brontide.Conn, error) {
	return brontide.CreateTestConn(), nil
}

// FailingStubDialer is a Dialer that always fails with the specified error.
type FailingStubDialer struct {
	err error
}

func (d *FailingStubDialer) Dial(addressess *lnwire.NetAddress) (*brontide.Conn, error) {
	return nil, d.err
}

// StubPeerCreator is a PeerCreator that avoids creating Peers
// that use concurrency/IO, to make unit testing more sane.
type StubPeerCreator struct {

}

func (pc *StubPeerCreator) newPeer(
	conn net.Conn, connReq *connmgr.ConnReq, server *server,
	address *lnwire.NetAddress, inbound bool,
	localFeatures *lnwire.RawFeatureVector) (*peer, error) {

	// Use the standard newPeer function to bootstrap this one.
	peerCreator := peerCreator{}
	peer, err := peerCreator.newPeer(
			conn, connReq, server, address, inbound, localFeatures)

	peer.remoteLocalFeatures = lnwire.NewFeatureVector(
		nil,
		lnwire.LocalFeatures)

	// Mark the peer as started so we don't have to go through the peer
	// initialization process, making unit testing much easier.
	peer.started = 1

	return peer, err
}
