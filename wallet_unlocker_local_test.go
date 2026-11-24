package lnd

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestLocalWalletUnlockerGRPC(t *testing.T) {
	methods := []string{
		"GenSeed", "InitWallet", "UnlockWallet", "ChangePassword",
	}
	peers := []struct {
		name    string
		addr    net.Addr
		allowed bool
	}{
		{
			name:    "loopback",
			addr:    &net.TCPAddr{IP: net.ParseIP("127.0.0.1")},
			allowed: true,
		},
		{
			name:    "ipv6",
			addr:    &net.TCPAddr{IP: net.ParseIP("::1")},
			allowed: true,
		},
		{
			name:    "unix",
			addr:    &net.UnixAddr{Name: "socket", Net: "unix"},
			allowed: true,
		},
		{name: "gateway", addr: rpcGatewayAddr{}, allowed: true},
		{
			name:    "remote",
			addr:    &net.TCPAddr{IP: net.ParseIP("192.0.2.1")},
			allowed: false,
		},
		{
			name:    "wildcard",
			addr:    &net.TCPAddr{IP: net.IPv4zero},
			allowed: false,
		},
		{name: "missing", addr: nil, allowed: false},
	}
	for _, p := range peers {
		for _, method := range methods {
			t.Run(p.name+"/"+method, func(t *testing.T) {
				ctx := peer.NewContext(
					t.Context(), &peer.Peer{Addr: p.addr},
				)
				called := false
				fullMethod := "/lnrpc.WalletUnlocker/" + method
				info := &grpc.UnaryServerInfo{
					FullMethod: fullMethod,
				}
				handler := func(
					context.Context, any) (any, error) {

					called = true
					return nil, nil
				}
				_, err := localWalletUnlockerUnary(
					ctx, nil, info, handler,
				)
				code := status.Code(err)
				denied := code == codes.PermissionDenied
				if called != p.allowed || denied == p.allowed {
					t.Fatalf("called=%v error=%v",
						called, err)
				}
			})
		}
	}
	err := localWalletUnlockerAuthorize(
		t.Context(), "/lnrpc.State/GetState",
	)
	require.NoError(t, err)
}

func TestLocalWalletUnlockerHTTP(t *testing.T) {
	paths := []string{
		"/v1/genseed", "/v1/initwallet", "/v1/unlockwallet",
		"/v1/changepassword", "/v1/%67enseed",
	}
	for _, path := range paths {
		for _, local := range []bool{false, true} {
			for _, method := range []string{"GET", "POST"} {
				req := httptest.NewRequest(
					method, path+"?method=POST", nil,
				)
				req.Header.Set("X-Forwarded-For", "127.0.0.1")
				req.Header.Set("X-HTTP-Method-Override", "POST")
				req.Header.Set("Upgrade", "websocket")
				req.Header.Set("Connection", "Upgrade")
				addr := &net.TCPAddr{
					IP: net.ParseIP("192.0.2.1"),
				}
				if local {
					addr.IP = net.ParseIP("127.0.0.1")
				}
				req = req.WithContext(context.WithValue(
					req.Context(), walletPeerKey{}, addr,
				))
				recorder := httptest.NewRecorder()
				localWalletUnlockerHTTP(http.HandlerFunc(
					func(w http.ResponseWriter,
						_ *http.Request) {

						w.WriteHeader(
							http.StatusNoContent,
						)
					},
				)).ServeHTTP(recorder, req)
				want := http.StatusForbidden
				if local {
					want = http.StatusNoContent
				}
				if recorder.Code != want {
					t.Fatalf("%s %s local=%v got %d",
						method, path, local,
						recorder.Code)
				}
			}
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/state", nil)
	recorder := httptest.NewRecorder()
	localWalletUnlockerHTTP(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
	)).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent {
		t.Fatal(recorder.Code)
	}
}

// TestLocalWalletUnlockerGateway exercises the actual gateway-to-gRPC hop.
func TestLocalWalletUnlockerGateway(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	private := bufconn.Listen(65536)
	defer private.Close()
	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(localWalletUnlockerUnary),
	)
	lnrpc.RegisterWalletUnlockerServer(
		server, lnrpc.UnimplementedWalletUnlockerServer{},
	)
	defer server.Stop()
	go func() { _ = server.Serve(rpcGatewayListener{private}) }()
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context,
			_ string) (net.Conn, error) {

			return private.DialContext(ctx)
		}),
	}
	mux := runtime.NewServeMux()
	err := lnrpc.RegisterWalletUnlockerHandlerFromEndpoint(
		ctx, mux, "passthrough:///gateway", opts,
	)
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodGet, "/v1/genseed", nil)
	request = request.WithContext(context.WithValue(
		request.Context(), walletPeerKey{},
		&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)},
	))
	response := httptest.NewRecorder()
	localWalletUnlockerHTTP(mux).ServeHTTP(response, request)
	// Unimplemented proves the request reached the test RPC implementation.
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("gateway status %d: %s",
			response.Code, response.Body.String())
	}
}
