package graphdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tor"
)

// addressType specifies the network protocol and version that should be used
// when connecting to a node at a particular address.
type addressType uint8

const (
	// tcp4Addr denotes an IPv4 TCP address.
	tcp4Addr addressType = 0

	// tcp6Addr denotes an IPv6 TCP address.
	tcp6Addr addressType = 1

	// v2OnionAddr denotes a version 2 Tor onion service address.
	v2OnionAddr addressType = 2

	// v3OnionAddr denotes a version 3 Tor (prop224) onion service address.
	v3OnionAddr addressType = 3

	// opaqueAddrs denotes an address (or a set of addresses) that LND was
	// not able to parse since LND is not yet aware of the address type.
	opaqueAddrs addressType = 4

	// dnsAddr denotes a DNS address type.
	dnsAddr addressType = 5
)

// encodeDNSAddr encodes a DNS address.
func encodeDNSAddr(w io.Writer, addr *lnwire.DNSAddress) error {
	if _, err := w.Write([]byte{byte(dnsAddr)}); err != nil {
		return err
	}

	// Write the length of the hostname.
	hostLen := len(addr.Hostname)
	if _, err := w.Write([]byte{byte(hostLen)}); err != nil {
		return err
	}

	if _, err := w.Write([]byte(addr.Hostname)); err != nil {
		return err
	}

	var port [2]byte
	byteOrder.PutUint16(port[:], addr.Port)
	if _, err := w.Write(port[:]); err != nil {
		return err
	}

	return nil
}

// encodeTCPAddr serializes a TCP address into its compact raw bytes
// representation.
func encodeTCPAddr(w io.Writer, addr *net.TCPAddr) error {
	var (
		addrType byte
		ip       []byte
	)

	if addr.IP.To4() != nil {
		addrType = byte(tcp4Addr)
		ip = addr.IP.To4()
	} else {
		addrType = byte(tcp6Addr)
		ip = addr.IP.To16()
	}

	if ip == nil {
		return fmt.Errorf("unable to encode IP %v", addr.IP)
	}

	if _, err := w.Write([]byte{addrType}); err != nil {
		return err
	}

	if _, err := w.Write(ip); err != nil {
		return err
	}

	var port [2]byte
	byteOrder.PutUint16(port[:], uint16(addr.Port))
	if _, err := w.Write(port[:]); err != nil {
		return err
	}

	return nil
}

// encodeOnionAddr serializes an onion address into its compact raw bytes
// representation.
func encodeOnionAddr(w io.Writer, addr *tor.OnionAddr) error {
	var suffixIndex int
	hostLen := len(addr.OnionService)
	switch hostLen {
	case tor.V2Len:
		if _, err := w.Write([]byte{byte(v2OnionAddr)}); err != nil {
			return err
		}
		suffixIndex = tor.V2Len - tor.OnionSuffixLen
	case tor.V3Len:
		if _, err := w.Write([]byte{byte(v3OnionAddr)}); err != nil {
			return err
		}
		suffixIndex = tor.V3Len - tor.OnionSuffixLen
	default:
		return errors.New("unknown onion service length")
	}

	suffix := addr.OnionService[suffixIndex:]
	if suffix != tor.OnionSuffix {
		return fmt.Errorf("invalid suffix \"%v\"", suffix)
	}

	host, err := tor.Base32Encoding.DecodeString(
		addr.OnionService[:suffixIndex],
	)
	if err != nil {
		return err
	}

	// Sanity check the decoded length.
	switch {
	case hostLen == tor.V2Len && len(host) != tor.V2DecodedLen:
		return fmt.Errorf("onion service %v decoded to invalid host %x",
			addr.OnionService, host)

	case hostLen == tor.V3Len && len(host) != tor.V3DecodedLen:
		return fmt.Errorf("onion service %v decoded to invalid host %x",
			addr.OnionService, host)
	}

	if _, err := w.Write(host); err != nil {
		return err
	}

	var port [2]byte
	byteOrder.PutUint16(port[:], uint16(addr.Port))
	if _, err := w.Write(port[:]); err != nil {
		return err
	}

	return nil
}

// encodeOpaqueAddrs serializes the lnwire.OpaqueAddrs type to a raw set of
// bytes that we will persist.
func encodeOpaqueAddrs(w io.Writer, addr *lnwire.OpaqueAddrs) error {
	// Write the type byte.
	if _, err := w.Write([]byte{byte(opaqueAddrs)}); err != nil {
		return err
	}

	// Write the length of the payload.
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(addr.Payload)))
	if _, err := w.Write(l[:]); err != nil {
		return err
	}

	// Write the payload.
	_, err := w.Write(addr.Payload)

	return err
}

// DeserializeAddr reads the serialized raw representation of an address and
// deserializes it into the actual address. This allows us to avoid address
// resolution within the channeldb package.
func DeserializeAddr(r io.Reader) (net.Addr, error) {
	var addrType [1]byte
	if _, err := r.Read(addrType[:]); err != nil {
		return nil, err
	}

	var address net.Addr
	switch addressType(addrType[0]) {
	case tcp4Addr:
		var ip [4]byte
		if _, err := r.Read(ip[:]); err != nil {
			return nil, err
		}

		var port [2]byte
		if _, err := r.Read(port[:]); err != nil {
			return nil, err
		}

		address = &net.TCPAddr{
			IP:   net.IP(ip[:]),
			Port: int(binary.BigEndian.Uint16(port[:])),
		}

	case tcp6Addr:
		var ip [16]byte
		if _, err := r.Read(ip[:]); err != nil {
			return nil, err
		}

		var port [2]byte
		if _, err := r.Read(port[:]); err != nil {
			return nil, err
		}

		address = &net.TCPAddr{
			IP:   net.IP(ip[:]),
			Port: int(binary.BigEndian.Uint16(port[:])),
		}

	case v2OnionAddr:
		var h [tor.V2DecodedLen]byte
		if _, err := r.Read(h[:]); err != nil {
			return nil, err
		}

		var p [2]byte
		if _, err := r.Read(p[:]); err != nil {
			return nil, err
		}

		onionService := tor.Base32Encoding.EncodeToString(h[:])
		onionService += tor.OnionSuffix
		port := int(binary.BigEndian.Uint16(p[:]))

		address = &tor.OnionAddr{
			OnionService: onionService,
			Port:         port,
		}

	case v3OnionAddr:
		var h [tor.V3DecodedLen]byte
		if _, err := r.Read(h[:]); err != nil {
			return nil, err
		}

		var p [2]byte
		if _, err := r.Read(p[:]); err != nil {
			return nil, err
		}

		onionService := tor.Base32Encoding.EncodeToString(h[:])
		onionService += tor.OnionSuffix
		port := int(binary.BigEndian.Uint16(p[:]))

		address = &tor.OnionAddr{
			OnionService: onionService,
			Port:         port,
		}

	case dnsAddr:
		// Read the length of the hostname.
		var hostLen [1]byte
		if _, err := r.Read(hostLen[:]); err != nil {
			return nil, err
		}

		// Read the hostname.
		hostname := make([]byte, hostLen[0])
		if _, err := r.Read(hostname); err != nil {
			return nil, err
		}

		// Read the port.
		var port [2]byte
		if _, err := r.Read(port[:]); err != nil {
			return nil, err
		}

		address = &lnwire.DNSAddress{
			Hostname: string(hostname),
			Port:     binary.BigEndian.Uint16(port[:]),
		}

	case opaqueAddrs:
		// Read the length of the payload.
		var l [2]byte
		if _, err := r.Read(l[:]); err != nil {
			return nil, err
		}

		// Read the payload.
		payload := make([]byte, binary.BigEndian.Uint16(l[:]))
		if _, err := r.Read(payload); err != nil {
			return nil, err
		}

		address = &lnwire.OpaqueAddrs{
			Payload: payload,
		}

	default:
		return nil, ErrUnknownAddressType
	}

	return address, nil
}

// SerializeAddr serializes an address into its raw bytes representation so that
// it can be deserialized without requiring address resolution.
func SerializeAddr(w io.Writer, address net.Addr) error {
	switch addr := address.(type) {
	case *net.TCPAddr:
		return encodeTCPAddr(w, addr)
	case *tor.OnionAddr:
		return encodeOnionAddr(w, addr)
	case *lnwire.OpaqueAddrs:
		return encodeOpaqueAddrs(w, addr)
	case *lnwire.DNSAddress:
		return encodeDNSAddr(w, addr)
	default:
		return ErrUnknownAddressType
	}
}
