package lndc

import (
	"bytes"
	"fmt"
	"net"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/fastsha256"
	"github.com/codahale/chacha20poly1305"
)

// Listener...
type Listener struct {
	longTermPriv *btcec.PrivateKey

	tcp *net.TCPListener
}

var _ net.Listener = (*Listener)(nil)

// NewListener...
func NewListener(localPriv *btcec.PrivateKey, listenAddr string) (*Listener, error) {
	addr, err := net.ResolveTCPAddr("tcp", listenAddr)
	if err != nil {
		return nil, err
	}

	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return nil, err
	}

	return &Listener{localPriv, l}, nil
}

// Accept waits for and returns the next connection to the listener.
// Part of the net.Listener interface.
func (l *Listener) Accept() (c net.Conn, err error) {
	conn, err := l.tcp.Accept()
	if err != nil {
		return nil, err
	}

	lndc := NewConn(conn)

	// Exchange an ephemeral public key with the remote connection in order
	// to establish a confidential connection before we attempt to
	// authenticated.
	ephemeralKey, err := l.createCipherConn(lndc)
	if err != nil {
		return nil, err
	}

	// Now that we've established an encrypted connection, authenticate the
	// identity of the remote host.
	ephemeralPub := ephemeralKey.PubKey().SerializeCompressed()
	if err := l.authenticateConnection(l.longTermPriv, lndc, ephemeralPub); err != nil {
		lndc.Close()
		return nil, err
	}

	return lndc, nil
}

// createCipherConn....
func (l *Listener) createCipherConn(lnConn *LNDConn) (*btcec.PrivateKey, error) {
	var err error
	var theirEphPubBytes []byte

	// First, read and deserialize their ephemeral public key.
	theirEphPubBytes, err = readClear(lnConn.Conn)
	if err != nil {
		return nil, err
	}
	if len(theirEphPubBytes) != 33 {
		return nil, fmt.Errorf("Got invalid %d byte eph pubkey %x\n",
			len(theirEphPubBytes), theirEphPubBytes)
	}
	theirEphPub, err := btcec.ParsePubKey(theirEphPubBytes, btcec.S256())
	if err != nil {
		return nil, err
	}

	// Once we've parsed and verified their key, generate, and send own
	// ephemeral key pair for use within this session.
	myEph, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil, err
	}
	if _, err := writeClear(lnConn.Conn, myEph.PubKey().SerializeCompressed()); err != nil {
		return nil, err
	}

	// Now that we have both keys, do non-interactive diffie with ephemeral
	// pubkeys, sha256 for good luck.
	sessionKey := fastsha256.Sum256(
		btcec.GenerateSharedSecret(myEph, theirEphPub),
	)

	lnConn.chachaStream, err = chacha20poly1305.New(sessionKey[:])

	// display private key for debug only
	fmt.Printf("made session key %x\n", sessionKey)

	lnConn.remoteNonceInt = 1 << 63
	lnConn.myNonceInt = 0

	lnConn.RemotePub = theirEphPub
	lnConn.Authed = false

	return myEph, nil
}

// AuthListen...
func (l *Listener) authenticateConnection(
	myId *btcec.PrivateKey, lnConn *LNDConn, localEphPubBytes []byte) error {
	var err error

	// TODO(roasbeef): should be using read/write clear here?
	slice := make([]byte, 73)
	n, err := lnConn.Conn.Read(slice)
	if err != nil {
		fmt.Printf("Read error: %s\n", err.Error())
		return err
	}

	fmt.Printf("read %d bytes\n", n)
	authmsg := slice[:n]
	if len(authmsg) != 53 && len(authmsg) != 73 {
		return fmt.Errorf("got auth message of %d bytes, "+
			"expect 53 or 73", len(authmsg))
	}

	myPKH := btcutil.Hash160(l.longTermPriv.PubKey().SerializeCompressed())
	if !bytes.Equal(myPKH, authmsg[33:53]) {
		return fmt.Errorf(
			"remote host asking for PKH %x, that's not me", authmsg[33:53])
	}

	// do DH with id keys
	theirPub, err := btcec.ParsePubKey(authmsg[:33], btcec.S256())
	if err != nil {
		return err
	}
	idDH := fastsha256.Sum256(
		btcec.GenerateSharedSecret(l.longTermPriv, theirPub),
	)

	myDHproof := btcutil.Hash160(
		append(lnConn.RemotePub.SerializeCompressed(), idDH[:]...),
	)
	theirDHproof := btcutil.Hash160(append(localEphPubBytes, idDH[:]...))

	// If they already know our public key, then execute the fast path.
	// Verify their DH proof, and send our own.
	if len(authmsg) == 73 {
		// Verify their DH proof.
		if !bytes.Equal(authmsg[53:], theirDHproof) {
			return fmt.Errorf("invalid DH proof from %s",
				lnConn.RemoteAddr().String())
		}

		// Their DH proof checks out, so send ours now.
		if _, err = lnConn.Conn.Write(myDHproof); err != nil {
			return err
		}
	} else {
		// Otherwise, they don't yet know our public key. So we'll send
		// it over to them, so we can both compute the DH proof.
		msg := append(l.longTermPriv.PubKey().SerializeCompressed(), myDHproof...)
		if _, err = lnConn.Conn.Write(msg); err != nil {
			return err
		}

		resp := make([]byte, 20)
		if _, err := lnConn.Conn.Read(resp); err != nil {
			return err
		}

		// Verify their DH proof.
		if bytes.Equal(resp, theirDHproof) == false {
			return fmt.Errorf("Invalid DH proof %x", theirDHproof)
		}
	}

	theirAdr := btcutil.Hash160(theirPub.SerializeCompressed())
	copy(lnConn.RemoteLNId[:], theirAdr[:16])
	lnConn.RemotePub = theirPub
	lnConn.Authed = true

	return nil
}

// Close closes the listener.
// Any blocked Accept operations will be unblocked and return errors.
// Part of the net.Listener interface.
func (l *Listener) Close() error {
	return l.tcp.Close()
}

// Addr returns the listener's network address.
// Part of the net.Listener interface.
func (l *Listener) Addr() net.Addr {
	return l.tcp.Addr()
}
