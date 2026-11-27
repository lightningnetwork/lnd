package lnd

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/test/bufconn"
)

// TestWalletGatewayTransport verifies that only the built-in wallet registrar
// changes transport when mTLS is disabled.
func TestWalletGatewayTransport(t *testing.T) {
	tests := []struct {
		name string
		mtls bool
	}{
		{name: "ordinary", mtls: false},
		{name: "mtls", mtls: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mtls := test.mtls
			ctx, cancel := context.WithTimeout(
				t.Context(), 10*time.Second,
			)
			defer cancel()
			peers := make(chan net.Addr, 4)
			interceptor := func(ctx context.Context, req any,
				_ *grpc.UnaryServerInfo,
				handler grpc.UnaryHandler) (any, error) {

				p, _ := peer.FromContext(ctx)
				peers <- p.Addr
				return handler(ctx, req)
			}
			server := grpc.NewServer(grpc.ChainUnaryInterceptor(
				interceptor, localWalletUnlockerUnary,
			))
			defer server.Stop()
			lnrpc.RegisterWalletUnlockerServer(
				server,
				lnrpc.UnimplementedWalletUnlockerServer{},
			)
			lnrpc.RegisterStateServer(
				server, &rpcClientAuthTestState{},
			)
			tcp, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer tcp.Close()
			private := bufconn.Listen(65536)
			defer private.Close()
			go func() { _ = server.Serve(tcp) }()
			go func() {
				_ = server.Serve(rpcGatewayListener{private})
			}()
			baseOpts := []grpc.DialOption{
				grpc.WithTransportCredentials(
					insecure.NewCredentials(),
				),
			}
			dest, opts, wallet := restGatewayOptions(
				"passthrough:///"+tcp.Addr().String(), baseOpts,
				func(ctx context.Context,
					_ string) (net.Conn, error) {

					return private.DialContext(ctx)
				}, mtls,
			)
			if wallet != nil {
				ctx = context.WithValue(
					ctx, walletRESTGatewayKey{}, wallet,
				)
			}
			mux := runtime.NewServeMux()
			err = lnrpc.RegisterStateHandlerFromEndpoint(
				ctx, mux, dest, opts,
			)
			require.NoError(t, err)
			err = (&DefaultWalletImpl{}).RegisterRestSubserver(
				ctx, mux, dest, opts,
			)
			require.NoError(t, err)
			call := func(path string, want int) net.Addr {
				t.Helper()
				r := httptest.NewRequest(
					http.MethodGet, path, nil,
				).WithContext(ctx)
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)
				if w.Code != want {
					t.Fatalf("%s: status %d, body %s",
						path, w.Code, w.Body.String())
				}
				select {
				case addr := <-peers:
					return addr
				case <-ctx.Done():
					t.Fatal(ctx.Err())
					return nil
				}
			}
			statePeer := call("/v1/state", http.StatusOK)
			if _, ok := statePeer.(rpcGatewayAddr); ok != mtls {
				t.Fatalf("state peer %T, mtls=%v",
					statePeer, mtls)
			}
			if !mtls {
				if _, ok := statePeer.(*net.TCPAddr); !ok {
					t.Fatalf("state peer = %T, want TCP",
						statePeer)
				}
			}
			walletPeer := call(
				"/v1/genseed", http.StatusNotImplemented,
			)
			if _, ok := walletPeer.(rpcGatewayAddr); !ok {
				t.Fatalf("wallet peer = %T, want gateway",
					walletPeer)
			}
		})
	}
}
