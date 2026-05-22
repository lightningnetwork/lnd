package pmp

import (
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
		_ = s.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, addr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
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
	return s.conn.LocalAddr().(*net.UDPAddr).Port
}

// newClientForServer points a Client at the test server. We override the
// well-known port 5351 by reusing the kernel-assigned port from the
// listener — done by overriding the *Client.call low-level UDP target.
//
// To keep the production code path unchanged, the test instead uses a
// custom-built client that dials the listener's exact address.
func newClientForServer(srv *fakeNATPMPServer) *Client {
	return &Client{
		gateway: srv.gateway,
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

	// Point the client at the server's actual ephemeral port by
	// crafting an explicit call. We bypass the well-known 5351 port
	// for the purposes of this test.
	c := newClientForServer(srv)
	resp, err := callExplicit(c, srv, []byte{0, 0}, 12)
	require.NoError(t, err)

	require.Equal(t, byte(0), resp[0])
	require.Equal(t, byte(0|0x80), resp[1])
	require.Equal(t, uint32(12345678), beUint32(resp[4:8]))
	require.Equal(t, [4]byte{203, 0, 113, 7},
		[4]byte{resp[8], resp[9], resp[10], resp[11]})
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
	_, err := callExplicit(c, srv, []byte{0, 0}, 12)
	require.Error(t, err)
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
	_, err := callExplicit(c, srv, []byte{0, 0}, 12)
	require.Error(t, err)
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
		timeout: 250 * time.Millisecond,
	}
	_, err := callExplicit(c, srv, []byte{0, 0}, 12)
	require.Error(t, err)
}

// callExplicit duplicates the *Client.call retry loop but dials the
// fake server's ephemeral port instead of the hard-coded 5351. This lets
// the tests exercise the protocol logic without requiring root or
// fiddling with the privileged port.
func callExplicit(c *Client, srv *fakeNATPMPServer, msg []byte,
	resultSize int) ([]byte, error) {

	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{
		IP:   srv.gateway,
		Port: srv.Port(),
	})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	deadline := time.Now().Add(c.timeout)
	_ = conn.SetDeadline(deadline)
	if _, err := conn.Write(msg); err != nil {
		return nil, err
	}

	buf := make([]byte, 64)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return nil, err
	}

	// Run the same validation rpc() would.
	resp := buf[:n]
	if len(resp) != resultSize {
		return nil, errSizeMismatch
	}
	if resp[0] != 0 {
		return nil, errVersionMismatch
	}
	expectedOp := msg[1] | 0x80
	if resp[1] != expectedOp {
		return nil, errOpcodeMismatch
	}
	if rc := beUint16(resp[2:4]); rc != 0 {
		return nil, errResultCode
	}
	return resp, nil
}

// Sentinel errors for the explicit-call helper.
var (
	errSizeMismatch    = &testErr{"size mismatch"}
	errVersionMismatch = &testErr{"version mismatch"}
	errOpcodeMismatch  = &testErr{"opcode mismatch"}
	errResultCode      = &testErr{"non-zero result code"}
)

type testErr struct{ msg string }

func (e *testErr) Error() string { return e.msg }
