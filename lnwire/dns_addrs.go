package lnwire

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/lightningnetwork/lnd/tlv"
)

// dnsAddrEntryOverhead is the number of bytes that each dns_hostname entry
// carries in addition to its hostname: a u16 length and a u16 port.
const dnsAddrEntryOverhead = 2 + 2

// DNSAddrs is the dns_hostnames list of a node_announcement_2. Each entry is
// encoded as a u16 hostname length, the hostname and a u16 port.
//
// The codec does not validate the hostname or the port. The list is in the
// signed range and the signature digest is rebuilt from the decoded records,
// so the codec must round-trip every entry exactly.
type DNSAddrs []*DNSAddress

// Record returns a Record that can be used to encode/decode a DNSAddrs to/from
// a TLV stream.
func (a *DNSAddrs) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		0, a, a.encodedSize, dnsAddrsEncoder, dnsAddrsDecoder,
	)
}

// encodedSize returns the number of bytes required to encode a DNSAddrs
// variable.
func (a *DNSAddrs) encodedSize() uint64 {
	var size uint64
	for _, addr := range *a {
		size += uint64(dnsAddrEntryOverhead + len(addr.Hostname))
	}

	return size
}

// dnsAddrsEncoder encodes a list of DNS addresses as TLV bytes.
func dnsAddrsEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	if v, ok := val.(*DNSAddrs); ok {
		for _, addr := range *v {
			if len(addr.Hostname) > math.MaxUint16 {
				return fmt.Errorf("dns_hostname length %d "+
					"exceeds %d", len(addr.Hostname),
					math.MaxUint16)
			}

			var buf [2]byte
			binary.BigEndian.PutUint16(
				buf[:], uint16(len(addr.Hostname)),
			)
			if _, err := w.Write(buf[:]); err != nil {
				return err
			}

			_, err := w.Write([]byte(addr.Hostname))
			if err != nil {
				return err
			}

			binary.BigEndian.PutUint16(buf[:], addr.Port)
			if _, err := w.Write(buf[:]); err != nil {
				return err
			}
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.DNSAddrs")
}

// dnsAddrsDecoder decodes TLV bytes into a list of DNS addresses.
func dnsAddrsDecoder(r io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	if v, ok := val.(*DNSAddrs); ok {
		var (
			addrs     DNSAddrs
			remaining = l
			buf       [2]byte
		)
		for remaining > 0 {
			if remaining < dnsAddrEntryOverhead {
				return fmt.Errorf("truncated dns_hostname: %d "+
					"bytes left", remaining)
			}

			if _, err := io.ReadFull(r, buf[:]); err != nil {
				return err
			}
			hostLen := uint64(binary.BigEndian.Uint16(buf[:]))

			entryLen := dnsAddrEntryOverhead + hostLen
			if entryLen > remaining {
				return fmt.Errorf("dns_hostname length %d "+
					"exceeds the %d bytes left", hostLen,
					remaining-2)
			}

			hostname := make([]byte, hostLen)
			if _, err := io.ReadFull(r, hostname); err != nil {
				return err
			}

			if _, err := io.ReadFull(r, buf[:]); err != nil {
				return err
			}

			addrs = append(addrs, &DNSAddress{
				Hostname: string(hostname),
				Port:     binary.BigEndian.Uint16(buf[:]),
			})
			remaining -= entryLen
		}
		*v = addrs

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "lnwire.DNSAddrs", l, l)
}
