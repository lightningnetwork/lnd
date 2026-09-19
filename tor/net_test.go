package tor

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"
)

type socksRequest struct {
	command  byte
	host     string
	port     uint16
	username string
	password string
}

type socksServer struct {
	listener    net.Listener
	requireAuth bool
	forward     bool

	requests chan socksRequest
	quit     chan struct{}
	wg       sync.WaitGroup
}

func newSOCKSServer(t *testing.T, requireAuth, forward bool) *socksServer {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server := &socksServer{
		listener:    listener,
		requireAuth: requireAuth,
		forward:     forward,
		requests:    make(chan socksRequest, 20),
		quit:        make(chan struct{}),
	}
	server.wg.Add(1)
	go server.serve()
	t.Cleanup(func() {
		close(server.quit)
		require.NoError(t, server.listener.Close())
		server.wg.Wait()
	})

	return server
}

func (s *socksServer) addr() string {
	return s.listener.Addr().String()
}

func (s *socksServer) serve() {
	defer s.wg.Done()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return
			default:
				continue
			}
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			_ = s.handle(conn)
		}()
	}
}

func (s *socksServer) handle(conn net.Conn) error {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return err
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(reader, methods); err != nil {
		return err
	}

	method := byte(0)
	if s.requireAuth {
		method = 2
	}
	if _, err := conn.Write([]byte{5, method}); err != nil {
		return err
	}

	var request socksRequest
	if s.requireAuth {
		credentials, err := readSOCKSCredentials(reader)
		if err != nil {
			return err
		}
		request.username = credentials.username
		request.password = credentials.password
		if _, err := conn.Write([]byte{1, 0}); err != nil {
			return err
		}
	}

	requestHeader := make([]byte, 4)
	if _, err := io.ReadFull(reader, requestHeader); err != nil {
		return err
	}
	request.command = requestHeader[1]

	host, err := readSOCKSHost(reader, requestHeader[3])
	if err != nil {
		return err
	}
	request.host = host

	port := make([]byte, 2)
	if _, err := io.ReadFull(reader, port); err != nil {
		return err
	}
	request.port = binary.BigEndian.Uint16(port)
	s.requests <- request

	if request.command == 0xf0 {
		_, err := conn.Write([]byte{5, 0, 0, 1, 192, 0, 2, 1})

		return err
	}

	if !s.forward {
		_, err := conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
		if err != nil {
			return err
		}

		_, _ = io.Copy(io.Discard, reader)

		return nil
	}

	upstream, err := net.Dial(
		"tcp", net.JoinHostPort(request.host, fmt.Sprint(request.port)),
	)
	if err != nil {
		_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})

		return err
	}
	defer upstream.Close()

	if _, err := conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {

		return err
	}

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(upstream, reader)
		_ = upstream.(*net.TCPConn).CloseWrite()
		close(done)
	}()
	_, _ = io.Copy(conn, upstream)
	<-done

	return nil
}

func readSOCKSCredentials(reader *bufio.Reader) (socksRequest, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return socksRequest{}, err
	}

	username := make([]byte, int(header[1]))
	if _, err := io.ReadFull(reader, username); err != nil {
		return socksRequest{}, err
	}
	passwordLen, err := reader.ReadByte()
	if err != nil {
		return socksRequest{}, err
	}
	password := make([]byte, int(passwordLen))
	if _, err := io.ReadFull(reader, password); err != nil {
		return socksRequest{}, err
	}

	return socksRequest{
		username: string(username),
		password: string(password),
	}, nil
}

func readSOCKSHost(reader *bufio.Reader, addressType byte) (string, error) {
	switch addressType {
	case 1:
		ip := make([]byte, net.IPv4len)
		_, err := io.ReadFull(reader, ip)

		return net.IP(ip).String(), err

	case 3:
		length, err := reader.ReadByte()
		if err != nil {
			return "", err
		}
		host := make([]byte, int(length))
		_, err = io.ReadFull(reader, host)

		return string(host), err

	case 4:
		ip := make([]byte, net.IPv6len)
		_, err := io.ReadFull(reader, ip)

		return net.IP(ip).String(), err

	default:
		return "", fmt.Errorf("unknown SOCKS address type %d", addressType)
	}
}

func receiveSOCKSRequest(t *testing.T, server *socksServer) socksRequest {
	t.Helper()

	select {
	case request := <-server.requests:
		return request

	case <-time.After(time.Second):
		t.Fatal("SOCKS request not received")

		return socksRequest{}
	}
}

func TestNewClearNetValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  ClearNetConfig
	}{
		{
			name: "missing proxy for credentials",
			cfg:  ClearNetConfig{Username: "alice"},
		},
		{
			name: "missing username",
			cfg: ClearNetConfig{
				SOCKS:    "127.0.0.1:9050",
				Password: "secret",
			},
		},
		{
			name: "missing port",
			cfg:  ClearNetConfig{SOCKS: "127.0.0.1"},
		},
		{
			name: "invalid port",
			cfg:  ClearNetConfig{SOCKS: "127.0.0.1:70000"},
		},
		{
			name: "invalid CIDR",
			cfg: ClearNetConfig{
				NoProxyTargets: []string{"192.0.2.0/99"},
			},
		},
		{
			name: "invalid wildcard",
			cfg: ClearNetConfig{
				NoProxyTargets: []string{"foo.*.example.com"},
			},
		},
		{
			name: "target with port",
			cfg: ClearNetConfig{
				NoProxyTargets: []string{"example.com:443"},
			},
		},
		{
			name: "empty target",
			cfg: ClearNetConfig{
				NoProxyTargets: []string{""},
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewClearNet(test.cfg)
			require.Error(t, err)
		})
	}

	_, err := NewClearNet(ClearNetConfig{
		SOCKS:          "[2001:db8::1]:9050",
		Username:       "alice",
		Password:       "secret",
		NoProxyTargets: []string{"example.com", "*.example.org"},
	})
	require.NoError(t, err)
}

func TestBypassMatcher(t *testing.T) {
	t.Parallel()

	matcher, err := newBypassMatcher([]string{
		"example.com", "*.example.org", "192.0.2.10",
		"198.51.100.0/24", "2001:db8::/32", "example.com",
	})
	require.NoError(t, err)

	tests := []struct {
		host  string
		match bool
	}{
		{host: "localhost", match: true},
		{host: "127.1.2.3", match: true},
		{host: "::1", match: true},
		{host: "example.com", match: true},
		{host: "EXAMPLE.COM.", match: true},
		{host: "sub.example.org", match: true},
		{host: "example.org", match: false},
		{host: "notexample.org", match: false},
		{host: "192.0.2.10", match: true},
		{host: "192.0.2.11", match: false},
		{host: "198.51.100.200", match: true},
		{host: "2001:db8::42", match: true},
		{host: "203.0.113.1", match: false},
	}

	for _, test := range tests {
		test := test
		t.Run(test.host, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, test.match, matcher.Match(test.host))
		})
	}
}

func TestClearNetRoutingAndAuthentication(t *testing.T) {
	proxyServer := newSOCKSServer(t, true, false)
	clearNet, err := NewClearNet(ClearNetConfig{
		SOCKS:    proxyServer.addr(),
		Username: "user:name",
		Password: "p@ssword",
	})
	require.NoError(t, err)

	conn, err := clearNet.Dial("tcp", "198.51.100.1:9735", time.Second)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	request := receiveSOCKSRequest(t, proxyServer)
	require.Equal(t, "198.51.100.1", request.host)
	require.Equal(t, uint16(9735), request.port)
	require.Equal(t, "user:name", request.username)
	require.Equal(t, "p@ssword", request.password)

	conn, err = clearNet.Dial("tcp6", "[2001:db8::1]:443", time.Second)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.Equal(t, "2001:db8::1",
		receiveSOCKSRequest(t, proxyServer).host)
}

func TestClearNetDefaultBypass(t *testing.T) {
	proxyServer := newSOCKSServer(t, false, false)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, listener.Close())
	})

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	clearNet, err := NewClearNet(ClearNetConfig{SOCKS: proxyServer.addr()})
	require.NoError(t, err)
	conn, err := clearNet.Dial("tcp", listener.Addr().String(), time.Second)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	select {
	case serverConn := <-accepted:
		require.NoError(t, serverConn.Close())
	case <-time.After(time.Second):
		t.Fatal("direct loopback connection not received")
	}

	select {
	case request := <-proxyServer.requests:
		t.Fatalf("loopback unexpectedly proxied: %+v", request)
	default:
	}
}

func TestClearNetProxyFailureIsFailClosed(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	proxyAddr := listener.Addr().String()
	require.NoError(t, listener.Close())

	clearNet, err := NewClearNet(ClearNetConfig{SOCKS: proxyAddr})
	require.NoError(t, err)
	conn, err := clearNet.Dial("tcp", "198.51.100.1:9735", 50*time.Millisecond)
	require.Error(t, err)
	require.Nil(t, conn)
}

func TestProxyNetHybridRoutingAndIsolation(t *testing.T) {
	torProxy := newSOCKSServer(t, true, false)
	clearProxy := newSOCKSServer(t, true, false)
	clearNet, err := NewClearNet(ClearNetConfig{
		SOCKS:    clearProxy.addr(),
		Username: "clear-user",
		Password: "clear-password",
	})
	require.NoError(t, err)
	torNet, err := NewProxyNet(ProxyNetConfig{
		SOCKS:                       torProxy.addr(),
		DNS:                         "127.0.0.1:53",
		StreamIsolation:             true,
		SkipProxyForClearNetTargets: true,
		ClearNet:                    clearNet,
	})
	require.NoError(t, err)

	onionHost := strings.Repeat("a", 56) + ".onion"
	conn, err := torNet.Dial(
		"onion", net.JoinHostPort(onionHost, "9735"), time.Second,
	)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	conn, err = torNet.Dial("tcp", "198.51.100.1:9735", time.Second)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	torRequest := receiveSOCKSRequest(t, torProxy)
	clearRequest := receiveSOCKSRequest(t, clearProxy)
	require.Equal(t, onionHost, torRequest.host)
	require.Len(t, torRequest.username, 16)
	require.Len(t, torRequest.password, 16)
	require.Equal(t, "clear-user", clearRequest.username)
	require.Equal(t, "clear-password", clearRequest.password)
	require.NotEqual(t, torRequest.username, clearRequest.username)
}

func TestProxyNetTorBypassTarget(t *testing.T) {
	torProxy := newSOCKSServer(t, false, false)
	clearProxy := newSOCKSServer(t, false, false)
	clearNet, err := NewClearNet(ClearNetConfig{SOCKS: clearProxy.addr()})
	require.NoError(t, err)
	torNet, err := NewProxyNet(ProxyNetConfig{
		SOCKS:          torProxy.addr(),
		DNS:            "127.0.0.1:53",
		ClearNet:       clearNet,
		NoProxyTargets: []string{"198.51.100.0/24"},
	})
	require.NoError(t, err)

	for _, address := range []string{
		"198.51.100.20:9735", "203.0.113.20:9735",
	} {
		conn, err := torNet.Dial("tcp", address, time.Second)
		require.NoError(t, err)
		require.NoError(t, conn.Close())
	}

	require.Equal(t, "198.51.100.20",
		receiveSOCKSRequest(t, clearProxy).host)
	require.Equal(t, "203.0.113.20",
		receiveSOCKSRequest(t, torProxy).host)
}

func TestProxyNetLookupsUseTor(t *testing.T) {
	torProxy := newSOCKSServer(t, false, true)

	dnsListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	dnsServer := &dns.Server{
		Listener: dnsListener,
		Net:      "tcp",
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
			response := new(dns.Msg)
			response.SetReply(request)
			response.Answer = []dns.RR{&dns.SRV{
				Hdr: dns.RR_Header{
					Name:   "_nodes._tcp.example.com.",
					Rrtype: dns.TypeSRV,
					Class:  dns.ClassINET,
					Ttl:    60,
				},
				Port:   9735,
				Target: "node.example.com.",
			}}
			require.NoError(t, w.WriteMsg(response))
		}),
	}
	go func() {
		_ = dnsServer.ActivateAndServe()
	}()
	t.Cleanup(func() {
		require.NoError(t, dnsServer.Shutdown())
	})

	torNet, err := NewProxyNet(ProxyNetConfig{
		SOCKS:                       torProxy.addr(),
		DNS:                         dnsListener.Addr().String(),
		SkipProxyForClearNetTargets: true,
		ClearNet:                    &ClearNet{},
		NoProxyTargets:              []string{"127.0.0.0/8"},
	})
	require.NoError(t, err)

	ips, err := torNet.LookupHost("example.com")
	require.NoError(t, err)
	require.Equal(t, []string{"192.0.2.1"}, ips)
	require.Equal(t, byte(0xf0),
		receiveSOCKSRequest(t, torProxy).command)

	_, records, err := torNet.LookupSRV(
		"nodes", "tcp", "example.com", time.Second,
	)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, uint16(9735), records[0].Port)

	dnsRequest := receiveSOCKSRequest(t, torProxy)
	require.Equal(t, byte(1), dnsRequest.command)
	require.Equal(t, "127.0.0.1", dnsRequest.host)

	// The legacy exported function retains its proxy-skip argument for API
	// compatibility, but SRV resolution must ignore it and still use Tor.
	_, records, err = LookupSRV(
		"nodes", "tcp", "example.com", torProxy.addr(),
		dnsListener.Addr().String(), false, true, time.Second,
	)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "127.0.0.1",
		receiveSOCKSRequest(t, torProxy).host)
}
