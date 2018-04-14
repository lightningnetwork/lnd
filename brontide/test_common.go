package brontide

import (
	"bytes"
	"encoding/hex"
	"github.com/roasbeef/btcd/btcec"
	"net"
	"time"
)

type StubNetConn struct {

}

func (*StubNetConn) Read(b []byte) (n int, err error) {
	return 0, nil
}

func (*StubNetConn) Write(b []byte) (n int, err error) {
	return 0, nil
}

func (*StubNetConn) Close() error {
	return nil
}

func (*StubNetConn) LocalAddr() net.Addr {
	return nil
}

func (*StubNetConn) RemoteAddr() net.Addr {
	return net.Addr(nil)
}

func (*StubNetConn) SetDeadline(t time.Time) error {
	return nil
}

func (*StubNetConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (*StubNetConn) SetWriteDeadline(t time.Time) error {
	return nil
}

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