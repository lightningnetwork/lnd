package brontide

import (
	"bytes"
	"encoding/hex"
	"github.com/roasbeef/btcd/btcec"
	"golang.org/x/crypto/chacha20poly1305"
	"math"
	"net"
	"time"
)

// StubNetConn minimally implements the net.Conn interface for test purposes.
type StubNetConn struct {
}

func (*StubNetConn) Read(b []byte) (n int, err error) {
	return 1, nil
}

func (*StubNetConn) Write(b []byte) (n int, err error) {
	return 1, nil
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
	pubKeyHex, _ := hex.DecodeString("028dfe1c8b9bfd7a7f8627a39a3b7b3a13d878a8b65dd26b17ca4f70a112a6dd54")
	pubKey, _ := btcec.ParsePubKey(pubKeyHex, btcec.S256())
	pubKey.Curve = nil

	cipher, _ := chacha20poly1305.New(
		[]byte{
			221, 15, 232, 219, 244, 108, 110, 16, 117, 128, 150, 55, 79, 112, 2, 113,
			199, 222, 228, 204, 172, 88, 53, 234, 142, 239, 135, 26, 27, 106, 228, 73})
	sendCipherState := cipherState{
		nonce:     0,
		secretKey: [32]byte{},
		salt:      [32]byte{},
		cipher:    cipher,
	}
	recvCipherState := cipherState{
		nonce:     0,
		secretKey: [32]byte{},
		salt:      [32]byte{},
		cipher:    cipher,
	}

	cipherHeader := [18]byte{
		22, 158, 71, 49, 138, 94, 222, 225, 242,
		1, 231, 190, 107, 121, 179, 127, 100, 141}
	cipherText := [math.MaxUint16 + 16]byte{
		118, 229, 26, 182, 0, 87, 200, 198, 149, 7, 145,
		157, 225, 91, 253, 235, 13, 241, 64, 201, 80, 78, 154}
	handshake := handshakeState{remoteStatic: pubKey}
	noise := &Machine{
		handshakeState:   handshake,
		sendCipher:       sendCipherState,
		recvCipher:       recvCipherState,
		nextCipherHeader: cipherHeader,
		nextCipherText:   cipherText,
	}
	return &Conn{conn: &StubNetConn{}, noise: noise, readBuf: bytes.Buffer{}}
}
