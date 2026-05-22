// Package pmp implements the small subset of NAT-PMP (RFC 6886) that lnd
// uses for outbound port mapping, plus a tiny default-gateway discovery
// helper. It is an internal fork of github.com/jackpal/go-nat-pmp and
// github.com/jackpal/gateway combined into a single package so the daemon
// no longer depends on either upstream module.
//
// Only the four operations lnd actually calls are exposed:
//
//   - NewClientWithTimeout    -> construct a client bound to a gateway IP
//   - (*Client).GetExternalAddress -> NAT-PMP opcode 0
//   - (*Client).AddPortMapping     -> NAT-PMP opcodes 1 (UDP) / 2 (TCP)
//   - DiscoverGateway              -> per-OS default-gateway discovery
package pmp

import (
	"fmt"
	"net"
	"time"
)

// natPMPPort is the well-known UDP port the NAT-PMP server listens on
// (RFC 6886, section 3).
const natPMPPort = 5351

// Default retry parameters from RFC 6886 section 3.1: nine doubling
// retries starting at 250ms.
const (
	natRetries      = 9
	natInitialDelay = 250 * time.Millisecond
)

// Client is a NAT-PMP protocol client bound to a single gateway address.
type Client struct {
	gateway net.IP
	timeout time.Duration
}

// NewClientWithTimeout returns a NAT-PMP client targeting the given
// gateway. A zero timeout falls back to the RFC retry schedule (~128s
// worst case).
func NewClientWithTimeout(gateway net.IP, timeout time.Duration) *Client {
	return &Client{gateway: gateway, timeout: timeout}
}

// GetExternalAddressResult mirrors the fields lnd reads off a successful
// NAT-PMP opcode 0 reply.
type GetExternalAddressResult struct {
	SecondsSinceStartOfEpoc uint32
	ExternalIPAddress       [4]byte
}

// GetExternalAddress issues opcode 0 and parses the 12-byte reply.
func (c *Client) GetExternalAddress() (*GetExternalAddressResult, error) {
	msg := []byte{0, 0} // version 0, opcode 0
	resp, err := c.rpc(msg, 12)
	if err != nil {
		return nil, err
	}
	r := &GetExternalAddressResult{}
	r.SecondsSinceStartOfEpoc = beUint32(resp[4:8])
	copy(r.ExternalIPAddress[:], resp[8:12])
	return r, nil
}

// AddPortMappingResult mirrors the fields returned by NAT-PMP opcodes
// 1/2. lnd does not currently read any of these but the upstream API
// returns them and we preserve that contract.
type AddPortMappingResult struct {
	SecondsSinceStartOfEpoc      uint32
	InternalPort                 uint16
	MappedExternalPort           uint16
	PortMappingLifetimeInSeconds uint32
}

// AddPortMapping issues opcode 1 (UDP) or 2 (TCP). Passing
// requestedExternalPort=0 and lifetime=0 deletes an existing mapping.
func (c *Client) AddPortMapping(protocol string, internalPort,
	requestedExternalPort, lifetime int) (*AddPortMappingResult, error) {

	var opcode byte
	switch protocol {
	case "udp":
		opcode = 1
	case "tcp":
		opcode = 2
	default:
		return nil, fmt.Errorf("unknown protocol %q", protocol)
	}

	msg := make([]byte, 12)
	msg[0] = 0 // version
	msg[1] = opcode
	beWriteUint16(msg[4:6], uint16(internalPort))
	beWriteUint16(msg[6:8], uint16(requestedExternalPort))
	beWriteUint32(msg[8:12], uint32(lifetime))

	resp, err := c.rpc(msg, 16)
	if err != nil {
		return nil, err
	}
	r := &AddPortMappingResult{}
	r.SecondsSinceStartOfEpoc = beUint32(resp[4:8])
	r.InternalPort = beUint16(resp[8:10])
	r.MappedExternalPort = beUint16(resp[10:12])
	r.PortMappingLifetimeInSeconds = beUint32(resp[12:16])
	return r, nil
}

// rpc sends the message and validates the reply per RFC 6886 section
// 3.5: the response opcode must equal request|0x80 and the result code
// must be zero.
func (c *Client) rpc(msg []byte, resultSize int) ([]byte, error) {
	resp, err := c.call(msg)
	if err != nil {
		return nil, err
	}
	if len(resp) != resultSize {
		return nil, fmt.Errorf("unexpected result size %d, expected %d",
			len(resp), resultSize)
	}
	if resp[0] != 0 {
		return nil, fmt.Errorf("unknown protocol version %d", resp[0])
	}
	expectedOp := msg[1] | 0x80
	if resp[1] != expectedOp {
		return nil, fmt.Errorf("unexpected opcode %d, expected %d",
			resp[1], expectedOp)
	}
	if rc := beUint16(resp[2:4]); rc != 0 {
		return nil, fmt.Errorf("non-zero result code %d", rc)
	}
	return resp, nil
}

// call performs the UDP round-trip with retries per RFC 6886 section
// 3.1. The retry schedule doubles the deadline each attempt, capped at
// the caller's overall timeout.
func (c *Client) call(msg []byte) ([]byte, error) {
	server := &net.UDPAddr{IP: c.gateway, Port: natPMPPort}
	conn, err := net.DialUDP("udp", nil, server)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	buf := make([]byte, 16)
	var finalDeadline time.Time
	if c.timeout != 0 {
		finalDeadline = time.Now().Add(c.timeout)
	}

	needNewDeadline := true
	for tries := uint(0); (tries < natRetries && finalDeadline.IsZero()) ||
		time.Now().Before(finalDeadline); {

		if needNewDeadline {
			next := time.Now().Add(
				(natInitialDelay << tries),
			)
			if err := conn.SetDeadline(
				minTime(next, finalDeadline),
			); err != nil {

				return nil, err
			}
			needNewDeadline = false
		}

		if _, err := conn.Write(msg); err != nil {
			return nil, err
		}

		n, remote, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				tries++
				needNewDeadline = true
				continue
			}
			return nil, err
		}

		// RFC 6886 section 3.2: reject responses not coming from
		// the configured gateway, but stay in the retry loop
		// without bumping the timeout.
		if !remote.IP.Equal(c.gateway) {
			continue
		}

		return buf[:n], nil
	}
	return nil, fmt.Errorf("timed out trying to contact gateway")
}

// minTime returns the earlier of two deadlines, treating the zero value
// as "no deadline".
func minTime(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	if b.IsZero() {
		return a
	}
	if a.Before(b) {
		return a
	}
	return b
}

// beUint16 / beUint32 / beWriteUint16 / beWriteUint32 are inlined
// big-endian helpers; using encoding/binary would pull a tiny extra
// abstraction layer for two-byte and four-byte reads where the
// hand-rolled version is clearer.
func beUint16(b []byte) uint16 {
	return uint16(b[0])<<8 | uint16(b[1])
}

func beUint32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 |
		uint32(b[2])<<8 | uint32(b[3])
}

func beWriteUint16(b []byte, v uint16) {
	b[0] = byte(v >> 8)
	b[1] = byte(v)
}

func beWriteUint32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}
