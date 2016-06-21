package lndc

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"strings"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcutil"
)

// lnAddr...
// TODO(roasbeef): revamp
type LNAdr struct {
	LnID   [16]byte // redundant because adr contains it
	PubKey *btcec.PublicKey

	Base58Adr btcutil.Address // Base58 encoded address (1XXX...)
	NetAddr   *net.TCPAddr    // IP address

	name        string // human readable name?  Not a thing yet.
	host        string // internet host this ID is reachable at. also not a thing
	endorsement []byte // a sig confirming the name?  Not implemented
}

// String...
func (l *LNAdr) String() string {
	var encodedId []byte
	if l.PubKey == nil {
		encodedId = l.Base58Adr.ScriptAddress()
	} else {
		encodedId = l.PubKey.SerializeCompressed()
	}

	return fmt.Sprintf("%v@%v", hex.EncodeToString(encodedId), l.NetAddr)
}

// newLnAddr...
func LnAddrFromString(encodedAddr string) (*LNAdr, error) {
	// The format of an lnaddr is "<pubkey or pkh>@host"
	idHost := strings.Split(encodedAddr, "@")
	if len(idHost) != 2 {
		return nil, fmt.Errorf("invalid format for lnaddr string: %v", encodedAddr)
	}

	// Attempt to resolve the IP address, this handles parsing IPv6 zones,
	// and such.
	fmt.Println("host: ", idHost[1])
	ipAddr, err := net.ResolveTCPAddr("tcp", idHost[1])
	if err != nil {
		return nil, err
	}

	addr := &LNAdr{NetAddr: ipAddr}

	idLen := len(idHost[0])
	switch {
	// Is the ID a hex-encoded compressed public key?
	case idLen > 65 && idLen < 69:
		pubkeyBytes, err := hex.DecodeString(idHost[0])
		if err != nil {
			return nil, err
		}

		addr.PubKey, err = btcec.ParsePubKey(pubkeyBytes, btcec.S256())
		if err != nil {
			return nil, err
		}

		// got pubey, populate address from pubkey
		pkh := btcutil.Hash160(addr.PubKey.SerializeCompressed())
		addr.Base58Adr, err = btcutil.NewAddressPubKeyHash(pkh,
			&chaincfg.TestNet3Params)
		if err != nil {
			return nil, err
		}
	// Is the ID a string encoded bitcoin address?
	case idLen > 33 && idLen < 37:
		addr.Base58Adr, err = btcutil.DecodeAddress(idHost[0],
			&chaincfg.TestNet3Params)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("invalid address %s", idHost[0])
	}

	// Finally, populate the lnid from the address.
	copy(addr.LnID[:], addr.Base58Adr.ScriptAddress())

	return addr, nil
}

// Deserialize an LNId from byte slice (on disk)
// Note that this does not check any internal consistency, because on local
// storage there's no point.  Check separately if needed.
// Also, old and probably needs to be changed / updated
func (l *LNAdr) Deserialize(s []byte) error {
	b := bytes.NewBuffer(s)

	// Fail if on-disk LNId too short
	if b.Len() < 24 { // 24 is min lenght
		return fmt.Errorf("can't read LNId - too short")
	}
	// read indicator of pubkey or pubkeyhash
	x, err := b.ReadByte()
	if err != nil {
		return err
	}
	if x == 0xb0 { // for pubkey storage
		// read 33 bytes of pubkey
		l.PubKey, err = btcec.ParsePubKey(b.Next(33), btcec.S256())
		if err != nil {
			return err
		}

		l.Base58Adr, err = btcutil.NewAddressPubKeyHash(
			btcutil.Hash160(l.PubKey.SerializeCompressed()),
			&chaincfg.TestNet3Params)
		if err != nil {
			return err
		}
	} else if x == 0xa0 { // for pubkeyhash storage
		l.Base58Adr, err = btcutil.NewAddressPubKeyHash(
			b.Next(20), &chaincfg.TestNet3Params)
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("Unknown lnid indicator byte %x", x)
	}

	var nameLen, hostLen, endorseLen uint8

	// read name length
	err = binary.Read(b, binary.BigEndian, &nameLen)
	if err != nil {
		return err
	}
	// if name non-zero, read name
	if nameLen > 0 {
		l.name = string(b.Next(int(nameLen)))
	}

	// read host length
	err = binary.Read(b, binary.BigEndian, &hostLen)
	if err != nil {
		return err
	}
	// if host non-zero, read host
	if hostLen > 0 {
		l.host = string(b.Next(int(hostLen)))
	}

	// read endorsement length
	err = binary.Read(b, binary.BigEndian, &endorseLen)
	if err != nil {
		return err
	}
	// if endorsement non-zero, read endorsement
	if endorseLen > 0 {
		l.endorsement = b.Next(int(endorseLen))
	}

	return nil
}
