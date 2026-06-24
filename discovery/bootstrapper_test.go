package discovery

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"
)

// stubNode is a minimal autopilot.Node implementation used to drive
// ChannelGraphBootstrapper from a unit test.
type stubNode struct {
	pub   *btcec.PublicKey
	addrs []net.Addr
}

func (n *stubNode) PubKey() [33]byte {
	var out [33]byte
	copy(out[:], n.pub.SerializeCompressed())
	return out
}

func (n *stubNode) Addrs() []net.Addr { return n.addrs }

// stubChannelGraph yields a fixed list of nodes from ForEachNode and is
// otherwise unused by SampleNodeAddrs.
type stubChannelGraph struct {
	nodes []autopilot.Node
}

// ForEachNode invokes the callback for each node in the stub graph,
// stopping early if the callback returns a non-nil error.
func (s *stubChannelGraph) ForEachNode(ctx context.Context,
	cb func(context.Context, autopilot.Node) error, _ func()) error {

	for _, n := range s.nodes {
		if err := cb(ctx, n); err != nil {
			return err
		}
	}

	return nil
}

// ForEachNodesChannels is a no-op stub; SampleNodeAddrs does not exercise
// the channel iteration path.
func (s *stubChannelGraph) ForEachNodesChannels(_ context.Context,
	_ func(context.Context, autopilot.NodeID,
		[]*autopilot.ChannelEdge) error, _ func()) error {

	return nil
}

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

// TestGraphBootstrapperSkipsV2Onion ensures SampleNodeAddrs strips Tor v2
// .onion entries from the returned candidate set so the connection manager
// never attempts to dial an obsolete v2 hidden service surfaced through the
// channel graph. A node whose remaining addresses include a v3 .onion or
// plain TCP entry is still returned for those addresses, and a node whose
// only address is v2 contributes nothing.
func TestGraphBootstrapperSkipsV2Onion(t *testing.T) {
	t.Parallel()

	v2 := &tor.OnionAddr{
		OnionService: "3g2upl4pq6kufc4m.onion",
		Port:         9735,
	}
	v3 := &tor.OnionAddr{
		OnionService: "4acth47i6kxnvkewtm6q7ib2s3ufpo5sqbsnz" +
			"jpbi7utijcltosqemad.onion",
		Port: 9735,
	}
	tcp := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9735}

	mkPub := func(t *testing.T) *btcec.PublicKey {
		t.Helper()
		priv, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		return priv.PubKey()
	}

	mixedPub := mkPub(t)
	v2OnlyPub := mkPub(t)

	graph := &stubChannelGraph{
		nodes: []autopilot.Node{
			&stubNode{
				pub:   mixedPub,
				addrs: []net.Addr{v2, v3, tcp},
			},
			&stubNode{
				pub:   v2OnlyPub,
				addrs: []net.Addr{v2},
			},
		},
	}

	// Use deterministic sampling so the hash accumulator never skips a
	// node — every eligible address must surface on each call.
	bs, err := NewGraphBootstrapper(graph, true)
	require.NoError(t, err)

	// Ask for more addresses than the graph holds so the bootstrapper
	// drains it in a single pass.
	got, err := bs.SampleNodeAddrs(
		t.Context(), 10, map[autopilot.NodeID]struct{}{},
	)
	require.NoError(t, err)

	// Collect (pubkey, address) pairs we expect — the v2-only node must
	// not contribute any NetAddress, and the mixed node contributes only
	// v3 and tcp.
	type seenAddr struct {
		pub  [33]byte
		addr string
	}
	seen := make(map[seenAddr]struct{}, len(got))
	for _, na := range got {
		var p [33]byte
		copy(p[:], na.IdentityKey.SerializeCompressed())
		seen[seenAddr{pub: p, addr: na.Address.String()}] = struct{}{}
	}

	var mixedKey, v2OnlyKey [33]byte
	copy(mixedKey[:], mixedPub.SerializeCompressed())
	copy(v2OnlyKey[:], v2OnlyPub.SerializeCompressed())

	// Mixed node yields v3 and tcp entries.
	require.Contains(t, seen, seenAddr{
		pub: mixedKey, addr: v3.String(),
	})
	require.Contains(t, seen, seenAddr{
		pub: mixedKey, addr: tcp.String(),
	})

	// No v2 entry surfaces, for either node.
	for s := range seen {
		require.NotEqual(t, v2.String(), s.addr,
			"v2 onion %v leaked into candidate set", s.addr)
		require.NotEqual(t, v2OnlyKey, s.pub,
			"v2-only node %x should contribute nothing", s.pub)
	}
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
