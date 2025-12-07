package lnwire

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"

	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// ErrEmptyDNSHostname is returned when a DNS hostname is empty.
	ErrEmptyDNSHostname = errors.New("hostname cannot be empty")

	// ErrZeroPort is returned when a DNS port is zero.
	ErrZeroPort = errors.New("port cannot be zero")

	// ErrHostnameTooLong is returned when a DNS hostname exceeds 255 bytes.
	ErrHostnameTooLong = errors.New("DNS hostname length exceeds limit " +
		"of 255 bytes")

	// ErrInvalidHostnameCharacter is returned when a DNS hostname contains
	// an invalid character.
	ErrInvalidHostnameCharacter = errors.New("hostname contains invalid " +
		"character")
)

// DNSAddress is used to represent a DNS address of a node.
type DNSAddress struct {
	// Hostname is the DNS hostname of the address. This MUST only contain
	// ASCII characters as per Bolt #7. The maximum length that this may
	// be is 255 bytes.
	Hostname string

	// Port is the port number of the address.
	Port uint16
}

// A compile-time check to ensure that DNSAddress implements the net.Addr
// interface.
var _ net.Addr = (*DNSAddress)(nil)

// Network returns the network that this address uses, which is "tcp".
func (d *DNSAddress) Network() string {
	return "tcp"
}

// String returns the address in the form "hostname:port".
func (d *DNSAddress) String() string {
	return net.JoinHostPort(d.Hostname, strconv.Itoa(int(d.Port)))
}

// ValidateDNSAddr validates that the DNS hostname is not empty and contains
// only ASCII characters and of max length 255 characters and port is non zero
// according to BOLT #7.
func ValidateDNSAddr(hostname string, port uint16) error {
	if hostname == "" {
		return ErrEmptyDNSHostname
	}

	// Per BOLT 7, ports must not be zero for type 5 address (DNS address).
	if port == 0 {
		return ErrZeroPort
	}

	if len(hostname) > 255 {
		return fmt.Errorf("%w: DNS hostname length %d",
			ErrHostnameTooLong, len(hostname))
	}

	// Check if hostname contains only ASCII characters.
	for i, r := range hostname {
		// Check for valid hostname characters, excluding ASCII control
		// characters (0-31), spaces, underscores, delete character
		// (127), and the special characters (like /, \, @, #, $, etc.).
		if !((r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' ||
			r == '.') {

			return fmt.Errorf("%w: hostname '%s' contains invalid "+
				"character '%c' at position %d",
				ErrInvalidHostnameCharacter, hostname, r, i)
		}
	}

	return nil
}

// Record returns a TLV record that can be used to encode/decode the DNSAddress.
//
// NOTE: this is part of the tlv.RecordProducer interface.
func (d *DNSAddress) Record() tlv.Record {
	sizeFunc := func() uint64 {
		// Hostname length + 2 bytes for port.
		return uint64(len(d.Hostname) + 2)
	}

	return tlv.MakeDynamicRecord(
		0, d, sizeFunc, dnsAddressEncoder, dnsAddressDecoder,
	)
}

// dnsAddressEncoder is a TLV encoder for DNSAddress.
func dnsAddressEncoder(w io.Writer, val any, _ *[8]byte) error {
	if v, ok := val.(*DNSAddress); ok {
		var buf bytes.Buffer

		// Write the hostname as raw bytes (no length prefix for TLV).
		if _, err := buf.WriteString(v.Hostname); err != nil {
			return err
		}

		// Write the port as 2 bytes.
		err := WriteUint16(&buf, v.Port)
		if err != nil {
			return err
		}

		_, err = w.Write(buf.Bytes())

		return err
	}

	return tlv.NewTypeForEncodingErr(val, "DNSAddress")
}

// dnsAddressDecoder is a TLV decoder for DNSAddress.
func dnsAddressDecoder(r io.Reader, val any, _ *[8]byte,
	l uint64) error {

	if v, ok := val.(*DNSAddress); ok {
		if l < 2 {
			return fmt.Errorf("DNS address must be at least 2 " +
				"bytes")
		}

		// Read hostname (all bytes except last 2).
		hostnameLen := l - 2
		hostnameBytes := make([]byte, hostnameLen)
		if _, err := io.ReadFull(r, hostnameBytes); err != nil {
			return err
		}
		v.Hostname = string(hostnameBytes)

		// Read port (last 2 bytes).
		if err := ReadElement(r, &v.Port); err != nil {
			return err
		}

		return ValidateDNSAddr(v.Hostname, v.Port)
	}

	return tlv.NewTypeForDecodingErr(val, "DNSAddress", l, 0)
}
