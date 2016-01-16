package lndc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"github.com/btcsuite/fastsha256"
	"golang.org/x/crypto/ripemd160"
)

// New & improved tcp open session.
// There's connector A and listener B.  Once the connection is set up there's no
// difference, but there can be during the setup.
// Setup:
// 1 -> A sends B ephemeral secp256k1 pubkey (33 bytes)
// 2 <- B sends A ephemeral secp256k1 pubkey (33 bytes)
// A and B do DH, get a shared secret.
// ==========
// Seesion is open!  Done!  Well not quite.  Session is confidential but not
// yet authenticated.  From here on, can use the Send() and Recv() functions with
// chacha20poly1305.
// ==========

// Nodes authenticate by doing a DH with their persistent identity keys, and then
// exchanging hash based proofs that they got the same shared IDDH secret.
// The DH proof is h160(remote eph pubkey, IDDH secret)
// A initiates auth.
//
// If A does not know B's pubkey but only B's pubkey hash:
//
// 1 -> A sends [PubKeyA, PubKeyHashB] (53 bytes)
// B computes ID pubkey DH
// 2 <- B sends [PubkeyB, DH proof] (53 bytes)
// 3 -> A sends DH proof (20 bytes)
// done.
//
// This exchange can be sped up if A already knows B's pubkey:
//
// A already knows who they're talking to, or trying to talk to
// 1 -> A sends [PubKeyA, PubkeyHashB, DH proof] (73 bytes)
// 2 <- B sends DH proof (20 bytes)
//
// A and B both verify those H160 hashes, and if matching consider their
// session counterparty authenticated.
//
// A possible weakness of the DH proof is if B re-uses eph keys.  That potentially
// makes *A*'s proof weaker though.  A gets to choose the proof B creates.  As
// long as your software makes new eph keys each time, you should be OK.

// WhoAreYou...
/*func (lndc *LNDConn) WhoAreYou(host string) (*btcec.PublicKey, error) {
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
}*/

// PbxEncapsulate...
// encapsulation for sending to Pbx host.
// put FWDMSG, then 16 byte destination ID, then message (with msgtype)
//func (lndc *LNDConn) PbxEncapsulate(b *[]byte) {
//	fmt.Printf("PbxEncapsulate %d byte message, dest ID %x\n",
//		len(*b), lndc.RemoteLNId)
//	*b = append(lndc.RemoteLNId[:], *b...)
//	*b = append([]byte{lnwire.MSGID_FWDMSG}, *b...)
//}

func H160(input []byte) []byte {
	rp := ripemd160.New()
	shaout := fastsha256.Sum256(input)
	_, _ = rp.Write(shaout[:])
	return rp.Sum(nil)
}

// readClear and writeClear don't encrypt but directly read and write to the
// underlying data link, only adding or subtracting a 2 byte length header.
// All Read() and Write() calls for lndc's use these functions internally
// (they aren't exported).  They're also used in the key agreement phase.

// readClear reads the next length-prefixed message from the underlying raw
// TCP connection.
func readClear(c net.Conn) ([]byte, error) {
	var msgLen uint16

	if err := binary.Read(c, binary.BigEndian, &msgLen); err != nil {
		return nil, err
	}

	msg := make([]byte, msgLen)
	if _, err := io.ReadFull(c, msg); err != nil {
		return nil, err
	}

	return msg, nil
}

// TODO(roasbeef): incorporate buffer pool

// writeClear writes the passed message with a prefixed 2-byte length header.
func writeClear(conn net.Conn, msg []byte) (int, error) {
	if len(msg) > 65530 {
		return 0, fmt.Errorf("lmsg too long, %d bytes", len(msg))
	}

	// Add 2 byte length header (pbx doesn't need it) and send over TCP.
	var msgBuf bytes.Buffer
	if err := binary.Write(&msgBuf, binary.BigEndian, uint16(len(msg))); err != nil {
		return 0, err
	}

	if _, err := msgBuf.Write(msg); err != nil {
		return 0, err
	}

	return conn.Write(msgBuf.Bytes())
}
