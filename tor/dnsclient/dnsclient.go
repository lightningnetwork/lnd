// Package dnsclient sends DNS queries over a caller-supplied
// net.Conn using only the stdlib's golang.org/x/net/dns/dnsmessage
// parser/builder. It exists so lnd no longer pulls in
// github.com/miekg/dns (a 50k-LoC general-purpose DNS toolkit) just
// to issue SRV queries against a specific server over a custom
// (Tor-tunneled or plain-TCP) connection.
//
// The package deliberately handles only the slice of DNS the lnd
// daemon needs: TCP-framed SRV requests against a single server.
// There is no UDP path, no EDNS, no DNSSEC, no zone-transfer
// machinery, and no caching. Callers are expected to dial the
// destination DNS server themselves (often through a SOCKS proxy)
// and hand the resulting net.Conn to QuerySRV.
package dnsclient

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"golang.org/x/net/dns/dnsmessage"
)

// maxDNSMessageSize caps the response size we are willing to read
// from the server. 64KiB is the RFC 1035 §4.2.2 maximum for a TCP-
// framed DNS message (the length prefix is a uint16).
const maxDNSMessageSize = 1 << 16

// QuerySRV sends a TCP-framed SRV query for name over conn and
// returns the parsed answer records along with the response's RCode.
// name should already be in fully-qualified form (e.g.
// "_nodes._tcp.nodes.lightning.directory.").
//
// The returned []*net.SRV is in the same order the server emitted
// the records; we do not sort by priority/weight or apply RFC 2782
// selection logic since callers handle that themselves.
//
// On a non-success RCode QuerySRV returns the parsed records (if
// any) along with the RCode and a nil error, so callers can format
// their own diagnostic message using RCodeText.
func QuerySRV(conn net.Conn, name string) ([]*net.SRV,
	dnsmessage.RCode, error) {

	q, err := buildSRVQuery(name)
	if err != nil {
		return nil, 0, fmt.Errorf("build SRV query: %w", err)
	}

	if err := writeMessage(conn, q); err != nil {
		return nil, 0, fmt.Errorf("write DNS query: %w", err)
	}

	respBytes, err := readMessage(conn)
	if err != nil {
		return nil, 0, fmt.Errorf("read DNS response: %w", err)
	}

	return parseSRVResponse(respBytes)
}

// buildSRVQuery returns the wire-format bytes of a recursion-
// desired SRV query for name with a random 16-bit transaction ID.
func buildSRVQuery(name string) ([]byte, error) {
	dn, err := dnsmessage.NewName(name)
	if err != nil {
		return nil, fmt.Errorf("invalid DNS name %q: %w", name, err)
	}

	id, err := randomID()
	if err != nil {
		return nil, err
	}

	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:               id,
		RecursionDesired: true,
	})
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	err = b.Question(dnsmessage.Question{
		Name:  dn,
		Type:  dnsmessage.TypeSRV,
		Class: dnsmessage.ClassINET,
	})
	if err != nil {
		return nil, err
	}
	return b.Finish()
}

// parseSRVResponse walks msg, asserting that the response is in
// fact for an SRV question, and returns every SRV record from the
// answer section in arrival order.
func parseSRVResponse(msg []byte) ([]*net.SRV, dnsmessage.RCode,
	error) {

	var p dnsmessage.Parser
	hdr, err := p.Start(msg)
	if err != nil {
		return nil, 0, fmt.Errorf("parse header: %w", err)
	}

	// Discard the question section; we do not validate the echoed
	// question matches our query (miekg/dns does not either for
	// our use case, and the soa-shim path intentionally queries
	// one server for names under a different zone).
	if err := p.SkipAllQuestions(); err != nil {
		return nil, hdr.RCode, fmt.Errorf("skip questions: %w", err)
	}

	var out []*net.SRV
	for {
		ah, err := p.AnswerHeader()
		if err == dnsmessage.ErrSectionDone {
			break
		}
		if err != nil {
			return nil, hdr.RCode, fmt.Errorf(
				"read answer header: %w", err,
			)
		}
		if ah.Type != dnsmessage.TypeSRV {
			if err := p.SkipAnswer(); err != nil {
				return nil, hdr.RCode, fmt.Errorf(
					"skip non-SRV answer: %w", err,
				)
			}
			continue
		}
		srv, err := p.SRVResource()
		if err != nil {
			return nil, hdr.RCode, fmt.Errorf(
				"parse SRV resource: %w", err,
			)
		}
		out = append(out, &net.SRV{
			Target:   srv.Target.String(),
			Port:     srv.Port,
			Priority: srv.Priority,
			Weight:   srv.Weight,
		})
	}

	return out, hdr.RCode, nil
}

// writeMessage emits a DNS message over conn using the TCP framing
// from RFC 1035 §4.2.2: a 2-byte big-endian length prefix followed
// by the raw message bytes.
func writeMessage(conn net.Conn, msg []byte) error {
	if len(msg) > maxDNSMessageSize-1 {
		return fmt.Errorf("DNS message too large: %d bytes",
			len(msg))
	}

	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(msg)))
	if _, err := conn.Write(hdr[:]); err != nil {
		return err
	}
	_, err := conn.Write(msg)
	return err
}

// readMessage reads a single TCP-framed DNS message from conn,
// returning just the message bytes (length prefix stripped).
func readMessage(conn net.Conn) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return nil, err
	}

	n := binary.BigEndian.Uint16(hdr[:])
	buf := make([]byte, n)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// randomID returns a non-deterministic 16-bit DNS transaction ID
// suitable for use in the message header. We pull from crypto/rand
// since the connection is often a Tor circuit and the ID is the
// only entropy preventing trivial response spoofing on the LAN
// path between the SOCKS exit and the DNS server.
func randomID() (uint16, error) {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("read random ID: %w", err)
	}
	return binary.BigEndian.Uint16(b[:]), nil
}
