package lndc

import (
	"bytes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/fastsha256"
	"github.com/codahale/chacha20poly1305"
	"golang.org/x/crypto/ripemd160"
	"li.lan/labs/plasma/lnwire"
)

/* good ol' OP_HASH160, which is just ripemd160(sha256(input)) */
func H160(input []byte) []byte {
	rp := ripemd160.New()
	shaout := fastsha256.Sum256(input)
	_, _ = rp.Write(shaout[:])
	return rp.Sum(nil)
}

// Lightning Network Data Conection.  Encrypted.
type LNDConn struct {
	Version        uint8
	Cn             net.Conn
	RemotePub      *btcec.PublicKey
	RemoteLNId     [16]byte
	MyNonceInt     uint64
	RemoteNonceInt uint64
	// if Authed == false, the RemotePub is the EPHEMERAL key.
	// once authed == true, RemotePub is who you're actually talking to.
	Authed bool
	// chachaStream saves some time as you don't have to init it with
	// the session key every time.  Make SessionKey redundant; remove later
	chachaStream cipher.AEAD

	// ViaPbx specifies whether this is a direct TCP connection or an
	// encapsulated PBX connection.
	// if going ViaPbx, Cn isn't used
	// channels are used for Read() and Write(),
	// which are filled by the PBXhandler.
	ViaPbx      bool
	PbxIncoming chan []byte
	PbxOutgoing chan []byte
}

/* new & improved tcp open session.
There's connector A and listener B.  Once the connection is set up there's no
difference, but there can be during the setup.
Setup:
1 -> A sends B ephemeral secp256k1 pubkey (33 bytes)
2 <- B sends A ephemeral secp256k1 pubkey (33 bytes)
A and B do DH, get a shared secret.
==========
Seesion is open!  Done!  Well not quite.  Session is confidential but not
yet authenticated.  From here on, can use the Send() and Recv() functions with
chacha20poly1305.
==========

Nodes authenticate by doing a DH with their persistent identity keys, and then
exchanging hash based proofs that they got the same shared IDDH secret.
The DH proof is h160(remote eph pubkey, IDDH secret)
A initiates auth.

If A does not know B's pubkey but only B's pubkey hash:

1 -> A sends [PubKeyA, PubKeyHashB] (53 bytes)
B computes ID pubkey DH
2 <- B sends [PubkeyB, DH proof] (53 bytes)
3 -> A sends DH proof (20 bytes)
done.

This exchange can be sped up if A already knows B's pubkey:

A already knows who they're talking to, or trying to talk to
1 -> A sends [PubKeyA, PubkeyHashB, DH proof] (73 bytes)
2 <- B sends DH proof (20 bytes)

A and B both verify those H160 hashes, and if matching consider their
session counterparty authenticated.

A possible weakness of the DH proof is if B re-uses eph keys.  That potentially
makes *A*'s proof weaker though.  A gets to choose the proof B creates.  As
long as your software makes new eph keys each time, you should be OK.
*/

// Open creates and auths an lndc connections,
// after the TCP or pbx connection is already assigned / dialed.
func (lndc *LNDConn) Open(
	me *btcec.PrivateKey, remote []byte) error {
	//	make TCP connection to listening host
	var err error

	if len(remote) != 33 && len(remote) != 20 {
		return fmt.Errorf("must supply either remote pubkey or pubkey hash")
	}
	// calc remote LNId; need this for creating pbx connections
	// just because LNid is in the struct does not mean it's authed!
	if len(remote) == 20 {
		copy(lndc.RemoteLNId[:], remote[:16])
	} else {
		theirAdr := H160(remote)
		copy(lndc.RemoteLNId[:], theirAdr[:16])
	}

	// make up an ephtemeral keypair.  Doesn't exit this function.
	myEph, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil
	}

	// Sned 1.  Send my ephemeral pubkey.  Can add version bits.
	err = lndc.writeClear(myEph.PubKey().SerializeCompressed())
	if err != nil {
		return err
	}

	// read their ephemeral pubkey
	TheirEphPubBytes, err := lndc.readClear()
	if err != nil {
		return err
	}

	// deserialize their ephemeral pubkey
	TheirEphPub, err := btcec.ParsePubKey(TheirEphPubBytes, btcec.S256())
	if err != nil {
		return err
	}
	// do non-interactive diffie with ephemeral pubkeys
	// sha256 for good luck
	sessionKey :=
		fastsha256.Sum256(btcec.GenerateSharedSecret(myEph, TheirEphPub))

	lndc.chachaStream, err = chacha20poly1305.New(sessionKey[:])
	if err != nil {
		return err
	}
	// display private key for debug only
	fmt.Printf("made session key %x\n", sessionKey)

	lndc.MyNonceInt = 1 << 63
	lndc.RemoteNonceInt = 0

	lndc.RemotePub = TheirEphPub
	lndc.Authed = false

	// session is now open and confidential but not yet authenticated.
	//	So auth!
	if len(remote) == 20 { // only know pubkey hash (20 bytes)
		return lndc.AuthPKH(me, remote, myEph.PubKey().SerializeCompressed())
	} else { // must be 33 byte pubkey
		return lndc.AuthPubKey(me, remote, myEph.PubKey().SerializeCompressed())
	}
}

// TcpListen takes a lndc that's just connected, and sets it up.
// Call this just after a connection comes in.
// calls AuthListen, waiting for the auth step before returning.
// in the case of a pbx connection, there is no lndc.Cn
func (lndc *LNDConn) Setup(me *btcec.PrivateKey) error {
	var err error
	var TheirEphPubBytes []byte

	TheirEphPubBytes, err = lndc.readClear()
	if err != nil {
		return err
	}

	if len(TheirEphPubBytes) != 33 {
		return fmt.Errorf("Got invalid %d byte eph pubkey %x\n",
			len(TheirEphPubBytes), TheirEphPubBytes)
	}
	// deserialize their ephemeral pubkey
	TheirEphPub, err := btcec.ParsePubKey(TheirEphPubBytes, btcec.S256())
	if err != nil {
		return err
	}

	// their key looks OK, make our own
	myEph, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		return nil
	}

	// and send out our eph key
	err = lndc.writeClear(myEph.PubKey().SerializeCompressed())
	if err != nil {
		return err
	}

	// do non-interactive diffie with ephemeral pubkeys
	// sha256 for good luck
	sessionKey :=
		fastsha256.Sum256(btcec.GenerateSharedSecret(myEph, TheirEphPub))

	lndc.chachaStream, err = chacha20poly1305.New(sessionKey[:])

	// display private key for debug only
	fmt.Printf("made session key %x\n", sessionKey)

	lndc.RemoteNonceInt = 1 << 63
	lndc.MyNonceInt = 0

	lndc.RemotePub = TheirEphPub
	lndc.Authed = false

	// session is open and confidential but not yet authenticated.
	// Listen for auth message
	return lndc.AuthListen(me, myEph.PubKey().SerializeCompressed())
}

func (lndc *LNDConn) AuthListen(
	myId *btcec.PrivateKey, localEphPubBytes []byte) error {

	var err error
	slice := make([]byte, 65535)
	n, err := lndc.Read(slice)
	if err != nil {
		return err
	}
	fmt.Printf("read %d bytes\n", n)
	slice = slice[:n]
	if err != nil {
		fmt.Printf("Read error: %s\n", err.Error())
		err2 := lndc.Close()
		if err2 != nil {
			return err2
		}
		return err
	}

	authmsg := slice
	if len(authmsg) != 53 && len(authmsg) != 73 {
		err = lndc.Close()
		if err != nil {
			return err
		}
		return fmt.Errorf(
			"Got auth message of %d bytes, expect 53 or 73", len(authmsg))
	}
	theirPub, err := btcec.ParsePubKey(authmsg[:33], btcec.S256())
	if err != nil {
		return err
	}
	myPKH := H160(myId.PubKey().SerializeCompressed())
	if bytes.Equal(myPKH, authmsg[33:53]) == false {
		err = lndc.Close()
		if err != nil {
			return err
		}
		return fmt.Errorf(
			"remote host asking for PKH %x, that's not me", authmsg[33:53])
	}

	// do DH with id keys
	idDH := fastsha256.Sum256(btcec.GenerateSharedSecret(myId, theirPub))
	myDHproof := H160(append(lndc.RemotePub.SerializeCompressed(), idDH[:]...))
	theirDHproof := H160(append(localEphPubBytes, idDH[:]...))
	if len(authmsg) == 73 { // quick mode
		// verify their DH proof
		if bytes.Equal(authmsg[53:], theirDHproof) == false {
			err = lndc.Close()
			if err != nil {
				return err
			}
			return fmt.Errorf(
				"Invalid DH proof from %s", lndc.Cn.RemoteAddr().String())
		}
		// looks good, send my own
		_, err = lndc.Write(myDHproof)
		if err != nil {
			err2 := lndc.Close()
			if err2 != nil {
				return err2
			}
			return err
		}
		// and we're authed
	} else { // 53 byte, they don't know my pubkey
		msg := append(myId.PubKey().SerializeCompressed(), myDHproof...)
		_, err = lndc.Write(msg)
		if err != nil {
			err2 := lndc.Close()
			if err2 != nil {
				return err2
			}
			return err
		}
		resp := make([]byte, 65535)
		n, err := lndc.Read(resp)
		if err != nil {
			return err
		}
		resp = resp[:n]

		if n != 20 {
			err2 := lndc.Close()
			if err2 != nil {
				return err2
			}
			fmt.Errorf("expected 20 byte DH proof, got %d", n)
		}

		// verify their DH proof
		if bytes.Equal(resp, theirDHproof) == false {
			err = lndc.Close()
			if err != nil {
				return err
			}
			return fmt.Errorf("Invalid DH proof %x", theirDHproof)
		}
		//proof looks good, auth
	}
	lndc.RemotePub = theirPub
	theirAdr := H160(theirPub.SerializeCompressed())
	copy(lndc.RemoteLNId[:], theirAdr[:16])
	lndc.Authed = true
	return nil
}

func (lndc *LNDConn) AuthPKH(
	myId *btcec.PrivateKey, theirPKH, localEphPubBytes []byte) error {
	var err error

	if myId == nil {
		return fmt.Errorf("can't auth: supplied privkey is nil")
	}
	if lndc.Authed {
		return fmt.Errorf("%s already authed", lndc.RemotePub)
	}
	if len(theirPKH) != 20 {
		return fmt.Errorf("remote PKH must be 20 bytes, got %d", len(theirPKH))
	}

	// send 53 bytes; my pubkey, and remote pubkey hash
	msg := myId.PubKey().SerializeCompressed()
	msg = append(msg, theirPKH...)

	_, err = lndc.Write(msg)
	if err != nil {
		return err
	}
	// wait for their response.
	// TODO add timeout here
	resp := make([]byte, 65535)
	n, err := lndc.Read(resp)
	if err != nil {
		return err
	}
	resp = resp[:n]
	// response should be 53 bytes, their pubkey and DH proof
	if n != 53 {
		return fmt.Errorf(
			"PKH auth response should be 53 bytes, got %d", len(resp))
	}
	theirPub, err := btcec.ParsePubKey(resp[:33], btcec.S256())
	if err != nil {
		return err
	}
	idDH := fastsha256.Sum256(btcec.GenerateSharedSecret(myId, theirPub))
	fmt.Printf("made idDH %x\n", idDH)

	theirDHproof := H160(append(localEphPubBytes, idDH[:]...))
	// verify their DH proof
	if bytes.Equal(resp[33:], theirDHproof) == false {
		return fmt.Errorf("Invalid DH proof %x", theirDHproof)
	}
	// their DH proof checks out, send our own
	myDHproof := H160(append(lndc.RemotePub.SerializeCompressed(), idDH[:]...))
	_, err = lndc.Write(myDHproof)
	if err != nil {
		return err
	}
	// proof sent, auth complete
	lndc.RemotePub = theirPub
	theirAdr := H160(theirPub.SerializeCompressed())
	copy(lndc.RemoteLNId[:], theirAdr[:16])
	lndc.Authed = true
	return nil
}

func (lndc *LNDConn) AuthPubKey(
	myId *btcec.PrivateKey, remotePubBytes, localEphPubBytes []byte) error {

	var err error
	if myId == nil {
		return fmt.Errorf("can't auth: supplied privkey is nil")
	}
	if lndc.Authed {
		return fmt.Errorf("%s already authed", lndc.RemotePub)
	}
	theirPub, err := btcec.ParsePubKey(remotePubBytes, btcec.S256())
	if err != nil {
		return err
	}
	theirPKH := H160(remotePubBytes)
	// I know enough to generate DH, do so
	idDH := fastsha256.Sum256(btcec.GenerateSharedSecret(myId, theirPub))
	myDHproof := H160(append(lndc.RemotePub.SerializeCompressed(), idDH[:]...))
	// message is 73 bytes; my pubkey, their pubkey hash, DH proof
	msg := myId.PubKey().SerializeCompressed()
	msg = append(msg, theirPKH...)
	msg = append(msg, myDHproof...)

	_, err = lndc.Write(msg)
	if err != nil {
		return err
	}
	resp := make([]byte, 65535)
	n, err := lndc.Read(resp)
	if err != nil {
		return err
	}
	resp = resp[:n]
	if n != 20 {
		fmt.Errorf("expected 20 byte DH proof, got %d", len(resp))
	}

	theirDHproof := H160(append(localEphPubBytes, idDH[:]...))
	// verify their DH proof
	if bytes.Equal(resp, theirDHproof) == false {
		return fmt.Errorf("Invalid DH proof %x", theirDHproof)
	}
	// proof checks out, auth complete
	lndc.RemotePub = theirPub
	theirAdr := H160(theirPub.SerializeCompressed())
	copy(lndc.RemoteLNId[:], theirAdr[:16])
	lndc.Authed = true
	return nil
}

func (lndc *LNDConn) WhoAreYou(host string) (*btcec.PublicKey, error) {
	var err error
	if lndc.Cn == nil {
		return nil, fmt.Errorf("no connection to ask on")
	}
	fmt.Printf("connecting to address %s\n", host)
	//		make TCP connection to listening host
	lndc.Cn, err = net.Dial("tcp", host)
	if err != nil {
		return nil, err
	}
	err = lndc.writeClear([]byte("WHOAREYOU"))
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	lndc.Read(b.Bytes())

	IamResp, err := lndc.readClear()
	if err != nil {
		return nil, err
	}
	return btcec.ParsePubKey(IamResp[:33], btcec.S256())
}

/*
Make ETcpCons adhere to Conn interface.
need:
Read(b []byte) (n int, err error)
Write(b []byte) (n int, err error)
Close() error
LocalAddr() Addr
RemoteAddr() Addr
SetDeadline(t time.Time) error
SetReadDeadline(t time.Time) error
SetWriteDeadline(t time.Time) error

ETcpCons can be either regular TCP connections, which is good, or PBX-routed
connections, which is worse, but there are levels of connectivity we are
prepared to accept.

When it's PBX, don't try to use Cn
*/

func (lndc *LNDConn) Read(b []byte) (n int, err error) {
	// first get message length from first 2 bytes
	var ctext []byte
	ctext, err = lndc.readClear()
	if err != nil {
		return 0, err
	}

	// now decrypt
	nonceBuf := new(bytes.Buffer)
	err = binary.Write(nonceBuf, binary.BigEndian, lndc.RemoteNonceInt)
	fmt.Printf("decrypt %d byte from %x nonce %d\n",
		len(ctext), lndc.RemoteLNId, lndc.RemoteNonceInt)
	lndc.RemoteNonceInt++ // increment remote nonce, no matter what...

	msg, err := lndc.chachaStream.Open(nil, nonceBuf.Bytes(), ctext, nil)
	if err != nil {
		fmt.Printf("decrypt %d byte ciphertext failed\n", len(ctext))
		return 0, err
	}

	n = copy(b, msg)
	if n < len(msg) {
		return 0, fmt.Errorf(
			"Can't read from %x: Slice provided too small.  %d bytes, need %d",
			lndc.RemoteLNId, len(b), len(msg))
	}

	return n, nil
}

func (lndc *LNDConn) Write(b []byte) (n int, err error) {
	if b == nil {
		return 0, fmt.Errorf("Write to %x nil", lndc.RemoteLNId)
	}
	fmt.Printf("Encrypt %d byte plaintext to %x nonce %d\n",
		len(b), lndc.RemoteLNId, lndc.MyNonceInt)
	// first encrypt message with shared key
	nonceBuf := new(bytes.Buffer)
	err = binary.Write(nonceBuf, binary.BigEndian, lndc.MyNonceInt)
	if err != nil {
		return 0, err
	}
	lndc.MyNonceInt++ // increment mine

	ctext := lndc.chachaStream.Seal(nil, nonceBuf.Bytes(), b, nil)
	if err != nil {
		return 0, err
	}
	if len(ctext) > 65530 {
		return 0, fmt.Errorf("Write to %x too long, %d bytes",
			lndc.RemoteLNId, len(ctext))
	}

	// use writeClear to prepend length / destination header
	err = lndc.writeClear(ctext)
	// returns len of how much you put in, not how much written on the wire
	return len(b), err
}

func (lndc *LNDConn) Close() error {
	//	return lndc.Cn.Close()
	//	return nil
	lndc.MyNonceInt = 0
	lndc.RemoteNonceInt = 0
	lndc.RemotePub = nil

	//	if lndc.ViaPbx {
	//		return nil // don't close pbx connection
	//	}
	err := lndc.Cn.Close()

	return err
}

// network address; if PBX reports address of pbx host
func (lndc *LNDConn) LocalAddr() net.Addr {
	return lndc.Cn.LocalAddr()
}
func (lndc *LNDConn) RemoteAddr() net.Addr {
	return lndc.Cn.RemoteAddr()
}

// timing stuff for net.conn compatibility
func (lndc *LNDConn) SetDeadline(t time.Time) error {
	return lndc.Cn.SetDeadline(t)
}
func (lndc *LNDConn) SetReadDeadline(t time.Time) error {

	return lndc.Cn.SetReadDeadline(t)
}
func (lndc *LNDConn) SetWriteDeadline(t time.Time) error {
	return lndc.Cn.SetWriteDeadline(t)
}

/* ReadClear and WriteClear don't encrypt but directly read and write to the
underlying data link, only adding or subtracting a 2 byte length header.
All Read() and Write() calls for lndc's use these functions internally
(they aren't exported).  They're also used in the key agreement phase.
*/

// encapsulation for sending to Pbx host.
// put FWDMSG, then 16 byte destination ID, then message (with msgtype)
func (lndc *LNDConn) PbxEncapsulate(b *[]byte) {
	fmt.Printf("PbxEncapsulate %d byte message, dest ID %x\n",
		len(*b), lndc.RemoteLNId)
	*b = append(lndc.RemoteLNId[:], *b...)
	*b = append([]byte{lnwire.MSGID_FWDMSG}, *b...)
}

func (lndc *LNDConn) readClear() ([]byte, error) {
	var msgLen uint16
	// needs to be pointer or will silently not do anything.
	var msg []byte

	if lndc.ViaPbx { //pbx mode
		msg = <-lndc.PbxIncoming
	} else { // normal tcp mode
		err := binary.Read(lndc.Cn, binary.BigEndian, &msgLen)
		if err != nil {
			return nil, err
		}
		//	fmt.Printf("%d byte LMsg incoming\n", msgLen)
		msg = make([]byte, msgLen)
		_, err = io.ReadFull(lndc.Cn, msg)
		if err != nil {
			return nil, err
		}
	}
	return msg, nil
}

func (lndc *LNDConn) writeClear(msg []byte) error {
	var err error
	if len(msg) > 65530 {
		return fmt.Errorf("LMsg too long, %d bytes", len(msg))
	}
	if msg == nil {
		return fmt.Errorf("LMsg nil")
	}
	if lndc.ViaPbx {
		lndc.PbxEncapsulate(&msg)
		lndc.PbxOutgoing <- msg
	} else {
		// add 2 byte length header (pbx doesn't need it) and send over TCP
		hdr := new(bytes.Buffer)
		err = binary.Write(hdr, binary.BigEndian, uint16(len(msg)))
		if err != nil {
			return err
		}
		msg = append(hdr.Bytes(), msg...)
		_, err = lndc.Cn.Write(msg)
		return err
	}
	return nil
}
