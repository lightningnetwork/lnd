package dnsclient

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"
)

// TestQuerySRVRoundTrip stands up a fake DNS server on a loopback
// TCP listener, has it answer a single SRV query with two records,
// and confirms the parsed []*net.SRV matches what the server sent
// (preserving arrival order, target trailing dot, and the full
// {Priority, Weight, Port} tuple).
func TestQuerySRVRoundTrip(t *testing.T) {
	t.Parallel()

	want := []*net.SRV{
		{Target: "node1.example.com.", Port: 9735, Priority: 10, Weight: 5},
		{Target: "node2.example.com.", Port: 9736, Priority: 20, Weight: 10},
	}

	conn := dialFake(t, fakeServer{
		rcode:   dnsmessage.RCodeSuccess,
		answers: want,
	})

	got, rcode, err := QuerySRV(conn, "_nodes._tcp.example.com.")
	require.NoError(t, err)
	require.Equal(t, dnsmessage.RCodeSuccess, rcode)
	require.Equal(t, want, got)
}

// TestQuerySRVNonSuccessRcode confirms that a server-side NXDOMAIN
// surfaces as a (nil-records, RCodeNameError, nil-error) tuple, so
// callers can format their own diagnostic string via RCodeText
// rather than getting a Go error.
func TestQuerySRVNonSuccessRcode(t *testing.T) {
	t.Parallel()

	conn := dialFake(t, fakeServer{rcode: dnsmessage.RCodeNameError})

	got, rcode, err := QuerySRV(conn, "_nodes._tcp.example.com.")
	require.NoError(t, err)
	require.Equal(t, dnsmessage.RCodeNameError, rcode)
	require.Empty(t, got)
	require.Equal(t, "non-existent domain", RCodeText(rcode))
}

// TestQuerySRVSkipsNonSRVAnswers exercises the parser branch that
// silently discards records whose Type is not SRV (e.g. CNAME or
// glue A records mixed into the answer section by some resolvers).
func TestQuerySRVSkipsNonSRVAnswers(t *testing.T) {
	t.Parallel()

	conn := dialFake(t, fakeServer{
		rcode: dnsmessage.RCodeSuccess,
		answers: []*net.SRV{
			{Target: "node1.example.com.", Port: 9735, Priority: 10},
		},
		injectCNAME: true,
	})

	got, _, err := QuerySRV(conn, "_nodes._tcp.example.com.")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "node1.example.com.", got[0].Target)
}

// TestQuerySRVInvalidName surfaces a wrapped error for names the
// dnsmessage package rejects (e.g. one too long for the wire
// format). We do not panic and we do not write anything to conn.
func TestQuerySRVInvalidName(t *testing.T) {
	t.Parallel()

	// dnsmessage caps name length at 255 bytes.
	tooLong := make([]byte, 300)
	for i := range tooLong {
		tooLong[i] = 'a'
	}

	_, _, err := QuerySRV(nopConn{}, string(tooLong))
	require.Error(t, err)
	require.Contains(t, err.Error(), "build SRV query")
}

// TestRCodeTextKnownAndUnknown covers both the populated map values
// and the fallback to dnsmessage.RCode.String() for codes that are
// not in the IANA registry table.
func TestRCodeTextKnownAndUnknown(t *testing.T) {
	t.Parallel()

	require.Equal(t, "no error", RCodeText(dnsmessage.RCodeSuccess))
	require.Equal(t, "server failure",
		RCodeText(dnsmessage.RCodeServerFailure))
	require.Equal(t, "non-existent domain",
		RCodeText(dnsmessage.RCodeNameError))
	require.Equal(t, "TSIG signature failure", RCodeText(16))

	// Codes outside the known table fall back to the dnsmessage
	// rendering. We just assert non-empty here so the test does
	// not depend on the upstream formatting style.
	require.NotEmpty(t, RCodeText(dnsmessage.RCode(99)))
}

// fakeServer is the test harness for a single-shot DNS server. It
// accepts one TCP connection, reads one TCP-framed DNS query, and
// writes back one response built from the configured fields.
type fakeServer struct {
	rcode       dnsmessage.RCode
	answers     []*net.SRV
	injectCNAME bool
}

// dialFake starts a fakeServer on 127.0.0.1, dials it, and returns
// the client side of the connection. The server goroutine and the
// listener close themselves when the test exits.
func dialFake(t *testing.T, fs fakeServer) net.Conn {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = fs.serve(c)
	}()

	conn, err := net.DialTimeout(
		"tcp", l.Addr().String(), 2*time.Second,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// serve reads a single DNS query frame and writes a single response
// frame built from fs's configured fields. It does not loop.
func (fs fakeServer) serve(c net.Conn) error {
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))

	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return err
	}
	reqLen := binary.BigEndian.Uint16(hdr[:])
	req := make([]byte, reqLen)
	if _, err := io.ReadFull(c, req); err != nil {
		return err
	}

	resp, err := fs.buildResponse(req)
	if err != nil {
		return err
	}

	var out [2]byte
	binary.BigEndian.PutUint16(out[:], uint16(len(resp)))
	if _, err := c.Write(out[:]); err != nil {
		return err
	}
	_, err = c.Write(resp)
	return err
}

// buildResponse parses the incoming query just enough to echo back
// its ID and question, then appends each configured SRV record (and
// optionally an extra CNAME to exercise the non-SRV skip branch).
func (fs fakeServer) buildResponse(req []byte) ([]byte, error) {
	var p dnsmessage.Parser
	rh, err := p.Start(req)
	if err != nil {
		return nil, err
	}
	q, err := p.Question()
	if err != nil {
		return nil, err
	}

	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:            rh.ID,
		Response:      true,
		Authoritative: true,
		RCode:         fs.rcode,
	})
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(q); err != nil {
		return nil, err
	}
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}

	if fs.injectCNAME {
		err := b.CNAMEResource(dnsmessage.ResourceHeader{
			Name:  q.Name,
			Type:  dnsmessage.TypeCNAME,
			Class: dnsmessage.ClassINET,
			TTL:   60,
		}, dnsmessage.CNAMEResource{
			CNAME: q.Name,
		})
		if err != nil {
			return nil, err
		}
	}

	for _, srv := range fs.answers {
		target, err := dnsmessage.NewName(srv.Target)
		if err != nil {
			return nil, err
		}
		err = b.SRVResource(dnsmessage.ResourceHeader{
			Name:  q.Name,
			Type:  dnsmessage.TypeSRV,
			Class: dnsmessage.ClassINET,
			TTL:   60,
		}, dnsmessage.SRVResource{
			Priority: srv.Priority,
			Weight:   srv.Weight,
			Port:     srv.Port,
			Target:   target,
		})
		if err != nil {
			return nil, err
		}
	}
	return b.Finish()
}

// nopConn is a net.Conn that errors on every operation. Used to
// confirm QuerySRV fails before any wire I/O when name validation
// rejects the input.
type nopConn struct{}

func (nopConn) Read(_ []byte) (int, error)         { return 0, io.EOF }
func (nopConn) Write(_ []byte) (int, error)        { return 0, io.ErrClosedPipe }
func (nopConn) Close() error                       { return nil }
func (nopConn) LocalAddr() net.Addr                { return nil }
func (nopConn) RemoteAddr() net.Addr               { return nil }
func (nopConn) SetDeadline(_ time.Time) error      { return nil }
func (nopConn) SetReadDeadline(_ time.Time) error  { return nil }
func (nopConn) SetWriteDeadline(_ time.Time) error { return nil }
