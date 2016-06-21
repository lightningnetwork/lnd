package lndc

import (
	"bytes"
	"crypto/cipher"
	"crypto/hmac"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"github.com/btcsuite/fastsha256"
	"github.com/codahale/chacha20poly1305"
	"github.com/roasbeef/btcutil"

	"github.com/roasbeef/btcd/btcec"
)

// Conn...
type LNDConn struct {
	RemotePub  *btcec.PublicKey
	RemoteLNId [16]byte

	myNonceInt     uint64
	remoteNonceInt uint64

	// If Authed == false, the remotePub is the EPHEMERAL key.
	// once authed == true, remotePub is who you're actually talking to.
	Authed bool

	// chachaStream saves some time as you don't have to init it with
	// the session key every time.  Make SessionKey redundant; remove later.
	chachaStream cipher.AEAD

	// ViaPbx specifies whether this is a direct TCP connection or an
	// encapsulated PBX connection.
	// If going ViaPbx, Cn isn't used channels are used for Read() and
	// Write(), which are filled by the PBXhandler.
	ViaPbx      bool
	PbxIncoming chan []byte
	PbxOutgoing chan []byte

	version uint8

	readBuf bytes.Buffer

	Conn net.Conn
}

// NewConn...
func NewConn(conn net.Conn) *LNDConn {
	return &LNDConn{Conn: conn}
}

// Dial...
func (c *LNDConn) Dial(
	myId *btcec.PrivateKey, address string, remoteId []byte) error {
	var err error

	if !c.ViaPbx {
		if c.Conn != nil {
			return fmt.Errorf("connection already established")
		}

		// First, open the TCP connection itself.
		c.Conn, err = net.Dial("tcp", address)
		if err != nil {
			return err
		}
	}

	// Before dialing out to the remote host, verify that `remoteId` is either
	// a pubkey or a pubkey hash.
	if len(remoteId) != 33 && len(remoteId) != 20 {
		return fmt.Errorf("must supply either remote pubkey or " +
			"pubkey hash")
	}

	// Calc remote LNId; need this for creating pbx connections just because
	// LNid is in the struct does not mean it's authed!
	if len(remoteId) == 20 {
		copy(c.RemoteLNId[:], remoteId[:16])
	} else {
		theirAdr := btcutil.Hash160(remoteId)
		copy(c.RemoteLNId[:], theirAdr[:16])
	}

	// Make up an ephemeral keypair for this session.
	ourEphemeralPriv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return err
	}
	ourEphemeralPub := ourEphemeralPriv.PubKey()

	// Send 1. Send my ephemeral pubkey. Can add version bits.
	if _, err = writeClear(c.Conn, ourEphemeralPub.SerializeCompressed()); err != nil {
		return err
	}

	// Read, then deserialize their ephemeral public key.
	theirEphPubBytes, err := readClear(c.Conn)
	if err != nil {
		return err
	}
	theirEphPub, err := btcec.ParsePubKey(theirEphPubBytes, btcec.S256())
	if err != nil {
		return err
	}

	// Do non-interactive diffie with ephemeral pubkeys. Sha256 for good
	// luck.
	sessionKey := fastsha256.Sum256(
		btcec.GenerateSharedSecret(ourEphemeralPriv, theirEphPub),
	)

	// Now that we've derive the session key, we can initialize the
	// chacha20poly1305 AEAD instance which will be used for the remainder of
	// the session.
	c.chachaStream, err = chacha20poly1305.New(sessionKey[:])
	if err != nil {
		return err
	}

	// display private key for debug only
	fmt.Printf("made session key %x\n", sessionKey)

	c.myNonceInt = 1 << 63
	c.remoteNonceInt = 0

	c.RemotePub = theirEphPub
	c.Authed = false

	// Session is now open and confidential but not yet authenticated...
	// So auth!
	if len(remoteId) == 20 {
		// Only know pubkey hash (20 bytes).
		err = c.authPKH(myId, remoteId, ourEphemeralPub.SerializeCompressed())
	} else {
		// Must be 33 byte pubkey.
		err = c.authPubKey(myId, remoteId, ourEphemeralPub.SerializeCompressed())
	}
	if err != nil {
		return err
	}

	return nil
}

// authPubKey...
func (c *LNDConn) authPubKey(
	myId *btcec.PrivateKey, remotePubBytes, localEphPubBytes []byte) error {
	if c.Authed {
		return fmt.Errorf("%s already authed", c.RemotePub)
	}

	// Since we already know their public key, we can immediately generate
	// the DH proof without an additional round-trip.
	theirPub, err := btcec.ParsePubKey(remotePubBytes, btcec.S256())
	if err != nil {
		return err
	}
	theirPKH := btcutil.Hash160(remotePubBytes)
	idDH := fastsha256.Sum256(btcec.GenerateSharedSecret(myId, theirPub))
	myDHproof := btcutil.Hash160(append(c.RemotePub.SerializeCompressed(), idDH[:]...))

	// Send over the 73 byte authentication message: my pubkey, their
	// pubkey hash, DH proof.
	var authMsg [73]byte
	copy(authMsg[:33], myId.PubKey().SerializeCompressed())
	copy(authMsg[33:], theirPKH)
	copy(authMsg[53:], myDHproof)
	if _, err = c.Conn.Write(authMsg[:]); err != nil {
		return nil
	}

	// Await, their response. They should send only the 20-byte DH proof.
	resp := make([]byte, 20)
	_, err = c.Conn.Read(resp)
	if err != nil {
		return err
	}

	// Verify that their proof matches our locally computed version.
	theirDHproof := btcutil.Hash160(append(localEphPubBytes, idDH[:]...))
	if !hmac.Equal(resp, theirDHproof) {
		return fmt.Errorf("invalid DH proof %x", theirDHproof)
	}

	// Proof checks out, auth complete.
	c.RemotePub = theirPub
	theirAdr := btcutil.Hash160(theirPub.SerializeCompressed())
	copy(c.RemoteLNId[:], theirAdr[:16])
	c.Authed = true

	return nil
}

// authPKH...
func (c *LNDConn) authPKH(
	myId *btcec.PrivateKey, theirPKH, localEphPubBytes []byte) error {
	if c.Authed {
		return fmt.Errorf("%s already authed", c.RemotePub)
	}
	if len(theirPKH) != 20 {
		return fmt.Errorf("remote PKH must be 20 bytes, got %d",
			len(theirPKH))
	}

	// Send 53 bytes: our pubkey, and the remote's pubkey hash.
	var greetingMsg [53]byte
	copy(greetingMsg[:33], myId.PubKey().SerializeCompressed())
	copy(greetingMsg[33:], theirPKH)
	if _, err := c.Conn.Write(greetingMsg[:]); err != nil {
		return err
	}

	// Wait for their response.
	// TODO(tadge): add timeout here
	//  * NOTE(roasbeef): read timeout should be set on the underlying
	//    net.Conn.
	resp := make([]byte, 53)
	if _, err := c.Conn.Read(resp); err != nil {
		return err
	}

	// Parse their long-term public key, and generate the DH proof.
	theirPub, err := btcec.ParsePubKey(resp[:33], btcec.S256())
	if err != nil {
		return err
	}
	idDH := fastsha256.Sum256(btcec.GenerateSharedSecret(myId, theirPub))
	fmt.Printf("made idDH %x\n", idDH)
	theirDHproof := btcutil.Hash160(append(localEphPubBytes, idDH[:]...))

	// Verify that their DH proof matches the one we just generated.
	if !hmac.Equal(resp[33:], theirDHproof) {
		return fmt.Errorf("Invalid DH proof %x", theirDHproof)
	}

	// If their DH proof checks out, then send our own.
	myDHproof := btcutil.Hash160(append(c.RemotePub.SerializeCompressed(), idDH[:]...))
	if _, err = c.Conn.Write(myDHproof); err != nil {
		return err
	}

	// Proof sent, auth complete.
	c.RemotePub = theirPub
	theirAdr := btcutil.Hash160(theirPub.SerializeCompressed())
	copy(c.RemoteLNId[:], theirAdr[:16])
	c.Authed = true

	return nil
}

// Read reads data from the connection.
// Read can be made to time out and return a Error with Timeout() == true
// after a fixed time limit; see SetDeadline and SetReadDeadline.
// Part of the net.Conn interface.
func (c *LNDConn) Read(b []byte) (n int, err error) {
	// In order to reconcile the differences between the record abstraction
	// of our AEAD connection, and the stream abstraction of TCP, we maintain
	// an intermediate read buffer. If this buffer becomes depleated, then
	// we read the next record, and feed it into the buffer. Otherwise, we
	// read directly from the buffer.
	if c.readBuf.Len() == 0 {
		// The buffer is empty, so read the next cipher text.
		ctext, err := readClear(c.Conn)
		if err != nil {
			return 0, err
		}

		// Encode the current remote nonce, so we can use it to decrypt
		// the cipher text.
		var nonceBuf [8]byte
		binary.BigEndian.PutUint64(nonceBuf[:], c.remoteNonceInt)

		fmt.Printf("decrypt %d byte from %x nonce %d\n",
			len(ctext), c.RemoteLNId, c.remoteNonceInt)

		c.remoteNonceInt++ // increment remote nonce, no matter what...

		msg, err := c.chachaStream.Open(nil, nonceBuf[:], ctext, nil)
		if err != nil {
			fmt.Printf("decrypt %d byte ciphertext failed\n", len(ctext))
			return 0, err
		}

		if _, err := c.readBuf.Write(msg); err != nil {
			return 0, err
		}
	}

	return c.readBuf.Read(b)
}

// Write writes data to the connection.
// Write can be made to time out and return a Error with Timeout() == true
// after a fixed time limit; see SetDeadline and SetWriteDeadline.
// Part of the net.Conn interface.
func (c *LNDConn) Write(b []byte) (n int, err error) {
	if b == nil {
		return 0, fmt.Errorf("write to %x nil", c.RemoteLNId)
	}
	fmt.Printf("Encrypt %d byte plaintext to %x nonce %d\n",
		len(b), c.RemoteLNId, c.myNonceInt)

	// first encrypt message with shared key
	var nonceBuf [8]byte
	binary.BigEndian.PutUint64(nonceBuf[:], c.myNonceInt)
	c.myNonceInt++ // increment mine

	ctext := c.chachaStream.Seal(nil, nonceBuf[:], b, nil)
	if err != nil {
		return 0, err
	}
	if len(ctext) > 65530 {
		return 0, fmt.Errorf("Write to %x too long, %d bytes",
			c.RemoteLNId, len(ctext))
	}

	// use writeClear to prepend length / destination header
	return writeClear(c.Conn, ctext)
}

// Close closes the connection.
// Any blocked Read or Write operations will be unblocked and return errors.
// Part of the net.Conn interface.
func (c *LNDConn) Close() error {
	c.myNonceInt = 0
	c.remoteNonceInt = 0
	c.RemotePub = nil

	return c.Conn.Close()
}

// LocalAddr returns the local network address.
// Part of the net.Conn interface.
// If PBX reports address of pbx host.
func (c *LNDConn) LocalAddr() net.Addr {
	return c.Conn.LocalAddr()
}

// RemoteAddr returns the remote network address.
// Part of the net.Conn interface.
func (c *LNDConn) RemoteAddr() net.Addr {
	return c.Conn.RemoteAddr()
}

// SetDeadline sets the read and write deadlines associated
// with the connection. It is equivalent to calling both
// SetReadDeadline and SetWriteDeadline.
// Part of the net.Conn interface.
func (c *LNDConn) SetDeadline(t time.Time) error {
	return c.Conn.SetDeadline(t)
}

// SetReadDeadline sets the deadline for future Read calls.
// A zero value for t means Read will not time out.
// Part of the net.Conn interface.
func (c *LNDConn) SetReadDeadline(t time.Time) error {
	return c.Conn.SetReadDeadline(t)
}

// SetWriteDeadline sets the deadline for future Write calls.
// Even if write times out, it may return n > 0, indicating that
// some of the data was successfully written.
// A zero value for t means Write will not time out.
// Part of the net.Conn interface.
func (c *LNDConn) SetWriteDeadline(t time.Time) error {
	return c.Conn.SetWriteDeadline(t)
}

var _ net.Conn = (*LNDConn)(nil)
