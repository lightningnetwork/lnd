package lndc

import (
	"encoding/hex"
	"fmt"
	"net"
	"strings"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"
)

// lnAddr...
type LNAdr struct {
	lnId   [16]byte // redundant because adr contains it
	PubKey *btcec.PublicKey

	Base58Addr btcutil.Address // Base58 encoded address (1XXX...)
	NetAddr    *net.TCPAddr    // IP address

	name        string // human readable name?  Not a thing yet.
	endorsement []byte // a sig confirming the name?  Not implemented
}

// String...
func (l *LNAdr) String() string {
	var encodedId []byte
	if l.PubKey == nil {
		encodedId = l.Base58Addr.ScriptAddress()
	} else {
		encodedId = l.PubKey.SerializeCompressed()
	}

	return fmt.Sprintf("%v@%v", encodedId, l.NetAddr)
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
		addr.Base58Addr, err = btcutil.NewAddressPubKeyHash(pkh,
			&chaincfg.TestNet3Params)
		if err != nil {
			return nil, err
		}
	// Is the ID a string encoded bitcoin address?
	case idLen > 33 && idLen < 37:
		addr.Base58Addr, err = btcutil.DecodeAddress(idHost[0],
			&chaincfg.TestNet3Params)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("invalid address %s", idHost[0])
	}

	// Finally, populate the lnid from the address.
	copy(addr.lnId[:], addr.Base58Addr.ScriptAddress())

	return addr, nil
}
