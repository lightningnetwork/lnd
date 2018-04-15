package brontide

import (
	"bytes"
	"encoding/hex"
	"github.com/roasbeef/btcd/btcec"
	"net"
	"time"
)

// StubNetConn minimally implements the net.Conn interface for test purposes.
type StubNetConn struct {

}

func (*StubNetConn) Read(b []byte) (n int, err error) {
	return 0, nil
}

func (*StubNetConn) Write(b []byte) (n int, err error) {
	return 0, nil
}

// Close .
func (*StubNetConn) Close() error {
	return nil
}

// LocalAddr .
func (*StubNetConn) LocalAddr() net.Addr {
	return nil
}

// RemoteAddr .
func (*StubNetConn) RemoteAddr() net.Addr {
	return net.Addr(nil)
}

// SetDeadline .
func (*StubNetConn) SetDeadline(t time.Time) error {
	return nil
}

// SetReadDeadline .
func (*StubNetConn) SetReadDeadline(t time.Time) error {
	return nil
}

// SetWriteDeadline .
func (*StubNetConn) SetWriteDeadline(t time.Time) error {
	return nil
}

// CreateTestConn creates a minimal brontide.Conn to be used for testing.
// This must exist within the brontide package since it relies upon
// the private cipherState and handshakeState initializers.
func CreateTestConn() *Conn {
	pubKeyHex,_ := hex.DecodeString("028dfe1c8b9bfd7a7f8627a39a3b7b3a13d878a8b65dd26b17ca4f70a112a6dd54")
	pubKey, _ := btcec.ParsePubKey(pubKeyHex, btcec.S256())
	pubKey.Curve = nil

	sendCipher := cipherState{}
	sendCipher.InitializeKey([32]byte{})
	handshake := handshakeState{remoteStatic: pubKey}
	noise := &Machine{
		handshakeState: handshake,
		sendCipher: sendCipher,
		recvCipher: cipherState{}}
	return &Conn{conn: &StubNetConn{}, noise: noise, readBuf: bytes.Buffer{}}
}