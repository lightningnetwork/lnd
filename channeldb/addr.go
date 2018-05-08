package channeldb

import (
	"io"
	"net"

	"github.com/btcsuite/go-socks/socks"
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

func encodeTCPAddr(w io.Writer, addr *net.TCPAddr) error {
	var scratch [16]byte

	if addr.IP.To4() != nil {
		scratch[0] = uint8(tcp4Addr)
		if _, err := w.Write(scratch[:1]); err != nil {
			return err
		}

		copy(scratch[:4], addr.IP.To4())
		if _, err := w.Write(scratch[:4]); err != nil {
			return err
		}

	} else {
		scratch[0] = uint8(tcp6Addr)
		if _, err := w.Write(scratch[:1]); err != nil {
			return err
		}

		copy(scratch[:], addr.IP.To16())
		if _, err := w.Write(scratch[:]); err != nil {
			return err
		}
	}

	byteOrder.PutUint16(scratch[:2], uint16(addr.Port))
	if _, err := w.Write(scratch[:2]); err != nil {
		return err
	}

	return nil
}

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

	switch addr := address.(type) {
	case *net.TCPAddr:
		return encodeTCPAddr(w, addr)

	// If this is a proxied address (due to the connection being
	// established over a SOCKs proxy, then we'll convert it into its
	// corresponding TCP address.
	case *socks.ProxiedAddr:
		// If we can't parse the host as an IP (though we should be
		// able to at this point), then we'll skip this address all
		// together.
		//
		// TODO(roasbeef): would be nice to be able to store hosts
		// though...
		ip := net.ParseIP(addr.Host)
		if ip == nil {
			return nil
		}

		tcpAddr := &net.TCPAddr{
			IP:   ip,
			Port: addr.Port,
		}
		return encodeTCPAddr(w, tcpAddr)
	}

	return nil
}
