package discovery

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"
)

// fallbackNet is a tor.Net stub used to drive fallBackSRVLookup. LookupHost
// returns shimAddrs and Dial serves a single DNS response, written by
// serveResp, over an in-memory pipe so the fallback path can be exercised
// without a real DNS server.
type fallbackNet struct {
	shimAddrs []string
	serveResp func(question *dns.Msg) *dns.Msg
}

// Dial returns one end of an in-memory pipe and spins up a goroutine acting as
// the DNS server on the other end, which reads the SRV query and writes back
// the response produced by serveResp.
func (n *fallbackNet) Dial(_, _ string,
	_ time.Duration) (net.Conn, error) {

	client, server := net.Pipe()

	// Act as the DNS server on the far end of the pipe: read the SRV
	// query, then write back the crafted response.
	go func() {
		srvConn := &dns.Conn{Conn: server}
		defer srvConn.Close()

		query, err := srvConn.ReadMsg()
		if err != nil {
			return
		}

		_ = srvConn.WriteMsg(n.serveResp(query))
	}()

	return client, nil
}

// LookupHost returns the configured shim addresses used to reach the fallback
// DNS server.
func (n *fallbackNet) LookupHost(_ string) ([]string, error) {
	return n.shimAddrs, nil
}

// LookupSRV is unsupported by this stub; the fallback path under test issues
// the SRV query manually over the Dial connection instead.
func (n *fallbackNet) LookupSRV(_, _, _ string,
	_ time.Duration) (string, []*net.SRV, error) {

	return "", nil, fmt.Errorf("unsupported")
}

// ResolveTCPAddr is unsupported by this stub as it is not exercised by the
// fallback SRV lookup path.
func (n *fallbackNet) ResolveTCPAddr(_, _ string) (*net.TCPAddr, error) {
	return nil, fmt.Errorf("unsupported")
}

// TestFallBackSRVLookupSkipsNonSRV ensures a DNS response whose Answer section
// contains non-SRV records (which an on-path attacker or malicious seed can
// inject, since the response is unauthenticated) is filtered rather than
// triggering a type-assertion panic that would crash the daemon.
func TestFallBackSRVLookupSkipsNonSRV(t *testing.T) {
	t.Parallel()

	const target = "nodes.lightning.directory"

	srvTarget := "ln1qexample._nodes._tcp." + target + "."

	netStub := &fallbackNet{
		shimAddrs: []string{"127.0.0.1"},
		serveResp: func(q *dns.Msg) *dns.Msg {
			resp := new(dns.Msg)
			resp.SetReply(q)
			resp.Rcode = dns.RcodeSuccess

			// A hostile/malformed Answer section: an A record and a
			// CNAME interleaved with a single valid SRV record.
			resp.Answer = []dns.RR{
				&dns.A{
					Hdr: dns.RR_Header{
						Name:   q.Question[0].Name,
						Rrtype: dns.TypeA,
					},
					A: net.ParseIP("1.2.3.4"),
				},
				&dns.CNAME{
					Hdr: dns.RR_Header{
						Name:   q.Question[0].Name,
						Rrtype: dns.TypeCNAME,
					},
					Target: "evil.example.",
				},
				&dns.SRV{
					Hdr: dns.RR_Header{
						Name:   q.Question[0].Name,
						Rrtype: dns.TypeSRV,
					},
					Target: srvTarget,
					Port:   9735,
				},
			}

			return resp
		},
	}

	bs := NewDNSSeedBootstrapper(
		[][2]string{{target, "soa.lightning.directory"}},
		netStub, time.Second,
	)
	d, ok := bs.(*DNSSeedBootstrapper)
	require.True(t, ok)

	// The non-SRV records must be skipped, leaving only the valid SRV
	// record. Crucially, this must not panic.
	srvs, err := d.fallBackSRVLookup("soa.lightning.directory", target)
	require.NoError(t, err)
	require.Len(t, srvs, 1)
	require.Equal(t, srvTarget, srvs[0].Target)
}

// TestFallBackSRVLookupNoSRVRecords ensures a successful DNS response whose
// Answer section holds no SRV records (only CNAME/A entries, or is empty)
// returns an error rather than (nil, nil), so the caller does not mistake "no
// usable records" for a successful query.
func TestFallBackSRVLookupNoSRVRecords(t *testing.T) {
	t.Parallel()

	const target = "nodes.lightning.directory"

	netStub := &fallbackNet{
		shimAddrs: []string{"127.0.0.1"},
		serveResp: func(q *dns.Msg) *dns.Msg {
			resp := new(dns.Msg)
			resp.SetReply(q)
			resp.Rcode = dns.RcodeSuccess

			// Only a non-SRV record is present in the Answer
			// section, leaving zero usable SRV targets.
			resp.Answer = []dns.RR{
				&dns.A{
					Hdr: dns.RR_Header{
						Name:   q.Question[0].Name,
						Rrtype: dns.TypeA,
					},
					A: net.ParseIP("1.2.3.4"),
				},
			}

			return resp
		},
	}

	bs := NewDNSSeedBootstrapper(
		[][2]string{{target, "soa.lightning.directory"}},
		netStub, time.Second,
	)
	d, ok := bs.(*DNSSeedBootstrapper)
	require.True(t, ok)

	srvs, err := d.fallBackSRVLookup("soa.lightning.directory", target)
	require.Error(t, err)
	require.Empty(t, srvs)
}

// TestFallBackSRVLookupEmptyAnswer ensures a successful DNS response with an
// entirely empty Answer section returns an error rather than (nil, nil), so the
// caller does not mistake an empty response for a successful query.
func TestFallBackSRVLookupEmptyAnswer(t *testing.T) {
	t.Parallel()

	const target = "nodes.lightning.directory"

	netStub := &fallbackNet{
		shimAddrs: []string{"127.0.0.1"},
		serveResp: func(q *dns.Msg) *dns.Msg {
			resp := new(dns.Msg)
			resp.SetReply(q)
			resp.Rcode = dns.RcodeSuccess

			// Leave the Answer section empty.
			resp.Answer = nil

			return resp
		},
	}

	bs := NewDNSSeedBootstrapper(
		[][2]string{{target, "soa.lightning.directory"}},
		netStub, time.Second,
	)
	d, ok := bs.(*DNSSeedBootstrapper)
	require.True(t, ok)

	srvs, err := d.fallBackSRVLookup("soa.lightning.directory", target)
	require.Error(t, err)
	require.Empty(t, srvs)
}

// TestFallBackSRVLookupNoShimAddrs ensures an empty LookupHost result for the
// fallback shim returns an error instead of panicking on an out-of-bounds
// index.
func TestFallBackSRVLookupNoShimAddrs(t *testing.T) {
	t.Parallel()

	netStub := &fallbackNet{shimAddrs: nil}

	bs := NewDNSSeedBootstrapper(nil, netStub, time.Second)
	d, ok := bs.(*DNSSeedBootstrapper)
	require.True(t, ok)

	_, err := d.fallBackSRVLookup(
		"soa.lightning.directory", "nodes.lightning.directory",
	)
	require.Error(t, err)
}
