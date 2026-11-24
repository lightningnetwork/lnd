package lnd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// TestRPCClientAuthentication exercises both public transports and the private
// REST gateway hop against one gRPC server. In particular a plain bufconn is
// treated as public; the server requires the private marker to waive mTLS.
func TestRPCClientAuthentication(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	ca := makeRPCTestCertificate(t, 1, nil, 0)
	serverCert := makeRPCTestCertificate(t, 2, &ca, x509.ExtKeyUsageServerAuth)
	client := makeRPCTestCertificate(t, 3, &ca, x509.ExtKeyUsageClientAuth)
	replacement := makeRPCTestCertificate(t, 4, &ca, x509.ExtKeyUsageClientAuth)
	untrustedCA := makeRPCTestCertificate(t, 5, nil, 0)
	untrusted := makeRPCTestCertificate(t, 6, &untrustedCA, x509.ExtKeyUsageClientAuth)
	serverOnly := makeRPCTestCertificate(t, 7, &ca, x509.ExtKeyUsageServerAuth)

	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	allowPath := filepath.Join(dir, "allowlist")
	require.NoError(t, os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: ca.Certificate[0],
	}), 0600))
	writeAllowlist := func(certs ...tls.Certificate) {
		t.Helper()
		var contents string
		for _, cert := range certs {
			contents += fmt.Sprintf("%x # client\n", sha256.Sum256(cert.Certificate[0]))
		}
		require.NoError(t, os.WriteFile(allowPath+".tmp", []byte(contents), 0600))
		require.NoError(t, os.Rename(allowPath+".tmp", allowPath))
	}

	writeAllowlist(client, untrusted, serverOnly)
	policy, err := newRPCClientAuth(caPath, allowPath)
	require.NoError(t, err)
	go policy.watch(ctx)

	baseTLS := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		MinVersion:   tls.VersionTLS12,
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Leaf)
	clientTLS := func(cert *tls.Certificate) *tls.Config {
		cfg := &tls.Config{RootCAs: roots, ServerName: "localhost"}
		if cert != nil {
			cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				return cert, nil
			}
		}
		return cfg
	}
	public, private := bufconn.Listen(65536), bufconn.Listen(65536)
	t.Cleanup(func() { _ = public.Close(); _ = private.Close() })
	grpcServer := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(policy.grpcTLS(baseTLS))),
		grpc.ChainUnaryInterceptor(policy.unary),
		grpc.ChainStreamInterceptor(policy.stream),
	)
	t.Cleanup(grpcServer.Stop)
	state := &rpcClientAuthTestState{}
	lnrpc.RegisterStateServer(grpcServer, state)
	go func() { _ = grpcServer.Serve(public) }()
	go func() { _ = grpcServer.Serve(rpcGatewayListener{private}) }()

	dialOptions := func(listener *bufconn.Listener, cert *tls.Certificate) []grpc.DialOption {
		return []grpc.DialOption{
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return listener.DialContext(ctx)
			}),
			grpc.WithTransportCredentials(credentials.NewTLS(clientTLS(cert))),
		}
	}
	grpcClient := func(cert *tls.Certificate) lnrpc.StateClient {
		t.Helper()
		conn, err := grpc.NewClient("passthrough:///test", dialOptions(public, cert)...)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		return lnrpc.NewStateClient(conn)
	}

	mux := runtime.NewServeMux()
	restCtx, stopREST := context.WithCancel(ctx)
	defer stopREST()
	require.NoError(t, lnrpc.RegisterStateHandlerFromEndpoint(
		restCtx, mux, "passthrough:///gateway", dialOptions(private, nil),
	))
	var lastRemoteAddr atomic.Value
	httpServer := httptest.NewUnstartedServer(policy.httpHandler(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			lastRemoteAddr.Store(r.RemoteAddr)
			mux.ServeHTTP(w, r)
		},
	)))
	httpServer.TLS = policy.publicTLS(baseTLS)
	httpServer.StartTLS()
	t.Cleanup(httpServer.Close)
	restClient := func(cert *tls.Certificate) *http.Client {
		transport := &http.Transport{TLSClientConfig: clientTLS(cert)}
		t.Cleanup(transport.CloseIdleConnections)
		return &http.Client{Transport: transport, Timeout: time.Second}
	}
	getREST := func(c *http.Client) (int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/v1/state", nil)
		if err != nil {
			return 0, err
		}
		resp, err := c.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		// Drain the response so the next call can reuse the connection.
		_, err = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, err
	}

	for _, cert := range []*tls.Certificate{nil, &replacement, &untrusted, &serverOnly} {
		_, err := grpcClient(cert).GetState(ctx, &lnrpc.GetStateRequest{})
		require.Equal(t, codes.Unavailable, status.Code(err))
		_, err = getREST(restClient(cert))
		require.Error(t, err)
	}
	require.Zero(t, state.calls.Load())

	firstRPC, firstREST := grpcClient(&client), restClient(&client)
	_, err = firstRPC.GetState(ctx, &lnrpc.GetStateRequest{})
	require.NoError(t, err)
	code, err := getREST(firstREST)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code)
	firstRESTAddr := lastRemoteAddr.Load()

	// Reload registers a replacement without invalidating the old client.
	writeAllowlist(client, replacement)
	secondRPC, secondREST := grpcClient(&replacement), restClient(&replacement)
	require.Eventually(t, func() bool {
		code, err := getREST(secondREST)
		return err == nil && code == http.StatusOK
	}, 4*time.Second, 100*time.Millisecond)
	_, err = secondRPC.GetState(ctx, &lnrpc.GetStateRequest{})
	require.NoError(t, err)
	_, err = firstRPC.GetState(ctx, &lnrpc.GetStateRequest{})
	require.NoError(t, err)
	code, err = getREST(firstREST)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, firstRESTAddr, lastRemoteAddr.Load())

	// A malformed replacement must not partially apply. An empty but
	// valid file, by contrast, removes all entries.
	require.NoError(t, os.WriteFile(allowPath+".tmp", []byte("bad fingerprint\n"), 0600))
	require.NoError(t, os.Rename(allowPath+".tmp", allowPath))
	// Inspect the malformed file with a separate policy, so the watch
	// goroutine remains the only writer to the live policy.
	_, err = newRPCClientAuth(caPath, allowPath)
	require.ErrorContains(t, err, "line 1")
	_, err = firstRPC.GetState(ctx, &lnrpc.GetStateRequest{})
	require.NoError(t, err)

	// The same live connections must no longer admit calls after revocation.
	writeAllowlist(replacement)
	require.Eventually(t, func() bool {
		_, err := firstRPC.GetState(ctx, &lnrpc.GetStateRequest{})
		return status.Code(err) == codes.Unauthenticated
	}, 4*time.Second, 100*time.Millisecond)
	code, err = getREST(firstREST)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, code)
	stream, err := firstRPC.SubscribeState(ctx, &lnrpc.SubscribeStateRequest{})
	require.NoError(t, err)
	_, err = stream.Recv()
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	_, err = secondRPC.GetState(ctx, &lnrpc.GetStateRequest{})
	require.NoError(t, err)
	writeAllowlist()
	require.Eventually(t, func() bool {
		_, err := secondRPC.GetState(ctx, &lnrpc.GetStateRequest{})
		return status.Code(err) == codes.Unauthenticated
	}, 4*time.Second, 100*time.Millisecond)
}

type rpcClientAuthTestState struct {
	lnrpc.UnimplementedStateServer
	calls atomic.Uint32
}

func (s *rpcClientAuthTestState) GetState(context.Context,
	*lnrpc.GetStateRequest) (*lnrpc.GetStateResponse, error) {

	s.calls.Add(1)
	return &lnrpc.GetStateResponse{State: lnrpc.WalletState_LOCKED}, nil
}

func makeRPCTestCertificate(t *testing.T, serial int64, ca *tls.Certificate,
	usage x509.ExtKeyUsage) tls.Certificate {

	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature,
	}
	parent, signer := template, key
	if ca == nil {
		template.IsCA = true
		template.KeyUsage |= x509.KeyUsageCertSign
	} else {
		template.ExtKeyUsage = []x509.ExtKeyUsage{usage}
		template.DNSNames = []string{"localhost"}
		parent, signer = ca.Leaf, ca.PrivateKey.(*ecdsa.PrivateKey)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, signer)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}
