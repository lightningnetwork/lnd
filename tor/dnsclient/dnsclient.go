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

	q, question, id, err := buildSRVQuery(name)
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

	return parseSRVResponse(respBytes, id, question)
}

// buildSRVQuery returns the wire-format bytes of a recursion-desired SRV query
// for name, the question used to build it, and a random transaction ID.
func buildSRVQuery(name string) ([]byte, dnsmessage.Question, uint16, error) {
	dn, err := dnsmessage.NewName(name)
	if err != nil {
		return nil, dnsmessage.Question{}, 0, fmt.Errorf(
			"invalid DNS name %q: %w", name, err,
		)
	}

	id, err := randomID()
	if err != nil {
		return nil, dnsmessage.Question{}, 0, err
	}

	question := dnsmessage.Question{
		Name:  dn,
		Type:  dnsmessage.TypeSRV,
		Class: dnsmessage.ClassINET,
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:               id,
		RecursionDesired: true,
	})
	if err := b.StartQuestions(); err != nil {
		return nil, dnsmessage.Question{}, 0, err
	}
	if err := b.Question(question); err != nil {
		return nil, dnsmessage.Question{}, 0, err
	}

	req, err := b.Finish()
	return req, question, id, err
}

// parseSRVResponse validates that msg answers the request identified by id and
// question, then returns every SRV record from the answer section in arrival
// order.
func parseSRVResponse(msg []byte, id uint16, question dnsmessage.Question) (
	[]*net.SRV, dnsmessage.RCode, error) {

	var p dnsmessage.Parser
	hdr, err := p.Start(msg)
	if err != nil {
		return nil, 0, fmt.Errorf("parse header: %w", err)
	}
	if !hdr.Response {
		return nil, hdr.RCode, fmt.Errorf("DNS message is not a response")
	}
	if hdr.ID != id {
		return nil, hdr.RCode, fmt.Errorf(
			"unexpected DNS response ID: got %d, want %d", hdr.ID, id,
		)
	}
	if hdr.OpCode != 0 {
		return nil, hdr.RCode, fmt.Errorf(
			"unexpected DNS response opcode: %d", hdr.OpCode,
		)
	}
	if hdr.Truncated {
		return nil, hdr.RCode, fmt.Errorf("truncated DNS response")
	}

	gotQuestion, err := p.Question()
	if err != nil {
		return nil, hdr.RCode, fmt.Errorf("read question: %w", err)
	}
	if gotQuestion != question {
		return nil, hdr.RCode, fmt.Errorf(
			"unexpected DNS response question: got %v, want %v",
			gotQuestion, question,
		)
	}
	_, err = p.Question()
	switch err {
	case dnsmessage.ErrSectionDone:

	case nil:
		return nil, hdr.RCode, fmt.Errorf(
			"DNS response contains more than one question",
		)

	default:
		return nil, hdr.RCode, fmt.Errorf(
			"finish question section: %w", err,
		)
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

// writeMessage emits a DNS message over conn using the TCP framing from RFC
// 1035 section 4.2.2: a 2-byte big-endian length prefix followed by the raw
// message bytes.
func writeMessage(conn net.Conn, msg []byte) error {
	if len(msg) > maxDNSMessageSize-1 {
		return fmt.Errorf("DNS message too large: %d bytes", len(msg))
	}

	frame := make([]byte, 2+len(msg))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(msg)))
	copy(frame[2:], msg)
	for len(frame) > 0 {
		n, err := conn.Write(frame)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
		frame = frame[n:]
	}

	return nil
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
