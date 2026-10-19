package pmp

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeNATPMPServer is a minimal in-process NAT-PMP server bound to
// 127.0.0.1 on an ephemeral port. The reply function lets each test
// dictate the exact bytes returned (or sleep to force a timeout).
type fakeNATPMPServer struct {
	t        *testing.T
	conn     *net.UDPConn
	reply    func(req []byte) []byte
	gateway  net.IP
	doneOnce sync.Once
	done     chan struct{}
}

func newFakeNATPMPServer(t *testing.T,
	reply func(req []byte) []byte) *fakeNATPMPServer {

	t.Helper()

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{
		IP:   net.IPv4(127, 0, 0, 1),
		Port: 0,
	})
	require.NoError(t, err)

	srv := &fakeNATPMPServer{
		t:       t,
		conn:    conn,
		reply:   reply,
		gateway: net.IPv4(127, 0, 0, 1),
		done:    make(chan struct{}),
	}
	go srv.serve()
	t.Cleanup(srv.Close)
	return srv
}

func (s *fakeNATPMPServer) serve() {
	buf := make([]byte, 64)
	for {
		select {
		case <-s.done:
			return
		default:
		}
		_ = s.conn.SetReadDeadline(
			time.Now().Add(100 * time.Millisecond),
		)
		n, addr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			return
		}
		resp := s.reply(buf[:n])
		if resp == nil {
			continue // simulate dropped packet
		}
		_, _ = s.conn.WriteToUDP(resp, addr)
	}
}

// Close stops the server. Safe to call multiple times.
func (s *fakeNATPMPServer) Close() {
	s.doneOnce.Do(func() {
		close(s.done)
		_ = s.conn.Close()
	})
}

// Port returns the server's bound UDP port.
func (s *fakeNATPMPServer) Port() int {
	addr, ok := s.conn.LocalAddr().(*net.UDPAddr)
	require.True(s.t, ok)

	return addr.Port
}

// newClientForServer points a Client at the test server's ephemeral port.
func newClientForServer(srv *fakeNATPMPServer) *Client {
	return &Client{
		gateway: srv.gateway,
		port:    srv.Port(),
		timeout: 2 * time.Second,
	}
}

// TestGetExternalAddress drives a successful opcode-0 exchange against
// the fake server and verifies the parsed reply matches the bytes the
// server returned.
func TestGetExternalAddress(t *testing.T) {
	t.Parallel()

	srv := newFakeNATPMPServer(t, func(req []byte) []byte {
		require.Equal(t, []byte{0, 0}, req)

		// 12-byte reply: version, opcode|0x80, rc=0, epoch, ip.
		resp := make([]byte, 12)
		resp[0] = 0
		resp[1] = 0 | 0x80
		beWriteUint16(resp[2:4], 0)        // result code = success
		beWriteUint32(resp[4:8], 12345678) // epoch
		resp[8], resp[9], resp[10], resp[11] = 203, 0, 113, 7
		return resp
	})

	c := newClientForServer(srv)
	resp, err := c.GetExternalAddress()
	require.NoError(t, err)
	require.Equal(t, uint32(12345678), resp.SecondsSinceStartOfEpoc)
	require.Equal(t, [4]byte{203, 0, 113, 7}, resp.ExternalIPAddress)
}

// TestRPCRejectsUnexpectedOpcode confirms the RFC-mandated response-
// opcode check (request|0x80) rejects a server that echoes the request
// opcode without flipping the high bit.
func TestRPCRejectsUnexpectedOpcode(t *testing.T) {
	t.Parallel()

	srv := newFakeNATPMPServer(t, func(req []byte) []byte {
		resp := make([]byte, 12)
		resp[0] = 0
		resp[1] = req[1] // missing 0x80 high bit
		return resp
	})

	c := newClientForServer(srv)
	_, err := c.GetExternalAddress()
	require.ErrorContains(t, err, "unexpected opcode")
}

// TestRPCRejectsNonZeroResultCode covers the RFC failure path.
func TestRPCRejectsNonZeroResultCode(t *testing.T) {
	t.Parallel()

	srv := newFakeNATPMPServer(t, func(req []byte) []byte {
		resp := make([]byte, 12)
		resp[0] = 0
		resp[1] = req[1] | 0x80
		beWriteUint16(resp[2:4], 3) // 3 = network failure
		return resp
	})

	c := newClientForServer(srv)
	_, err := c.GetExternalAddress()
	require.ErrorContains(t, err, "non-zero result code")
}

// TestTimeoutOnSilentServer confirms the retry loop bottoms out when the
// fake server drops every packet.
func TestTimeoutOnSilentServer(t *testing.T) {
	t.Parallel()

	srv := newFakeNATPMPServer(t, func(_ []byte) []byte {
		return nil // drop everything
	})

	c := &Client{
		gateway: srv.gateway,
		port:    srv.Port(),
		timeout: 250 * time.Millisecond,
	}
	_, err := c.GetExternalAddress()
	require.ErrorContains(t, err, "timed out trying to contact gateway")
}
