package channeldb

import (
	"io"
	"net"
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

	// v3OnionAddr denotes a version 3 Tor (prop224) onion service addresses.
	v3OnionAddr addressType = 3
)

// deserializeAddr reads the serialized raw representation of an address and
// deserializes it into the actual address, to avoid performing address
// resolution in the database module
func deserializeAddr(r io.Reader) (net.Addr, error) {
	var scratch [8]byte
	var address net.Addr

	if _, err := r.Read(scratch[:1]); err != nil {
		return nil, err
	}

	// TODO(roasbeef): also add onion addrs
	switch addressType(scratch[0]) {
	case tcp4Addr:
		addr := &net.TCPAddr{}
		var ip [4]byte
		if _, err := r.Read(ip[:]); err != nil {
			return nil, err
		}
		addr.IP = (net.IP)(ip[:])
		if _, err := r.Read(scratch[:2]); err != nil {
			return nil, err
		}
		addr.Port = int(byteOrder.Uint16(scratch[:2]))
		address = addr
	case tcp6Addr:
		addr := &net.TCPAddr{}
		var ip [16]byte
		if _, err := r.Read(ip[:]); err != nil {
			return nil, err
		}
		addr.IP = (net.IP)(ip[:])
		if _, err := r.Read(scratch[:2]); err != nil {
			return nil, err
		}
		addr.Port = int(byteOrder.Uint16(scratch[:2]))
		address = addr
	default:
		return nil, ErrUnknownAddressType
	}

	return address, nil
}

// serializeAddr serializes an address into a raw byte representation so it
// can be deserialized without requiring address resolution
func serializeAddr(w io.Writer, address net.Addr) error {
	var scratch [16]byte

	if address.Network() == "tcp" {
		if address.(*net.TCPAddr).IP.To4() != nil {
			scratch[0] = uint8(tcp4Addr)
			if _, err := w.Write(scratch[:1]); err != nil {
				return err
			}
			copy(scratch[:4], address.(*net.TCPAddr).IP.To4())
			if _, err := w.Write(scratch[:4]); err != nil {
				return err
			}
		} else {
			scratch[0] = uint8(tcp6Addr)
			if _, err := w.Write(scratch[:1]); err != nil {
				return err
			}
			copy(scratch[:], address.(*net.TCPAddr).IP.To16())
			if _, err := w.Write(scratch[:]); err != nil {
				return err
			}
		}
		byteOrder.PutUint16(scratch[:2],
			uint16(address.(*net.TCPAddr).Port))
		if _, err := w.Write(scratch[:2]); err != nil {
			return err
		}
	}

	return nil
}
