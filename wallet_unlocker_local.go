package lnd

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// restGatewayOptions retains the ordinary gateway transport without mTLS, so
// existing peer-dependent macaroon caveats keep their previous behavior.
func restGatewayOptions(dest string, opts []grpc.DialOption,
	dial func(context.Context, string) (net.Conn, error), mtls bool) (string, []grpc.DialOption, *walletRESTGateway) {

	if dial == nil {
		return dest, opts, nil
	}
	privateOpts := append([]grpc.DialOption(nil), opts...)
	privateOpts = append(privateOpts, grpc.WithContextDialer(dial))
	privateDest := "passthrough:///lnd-rest-gateway"
	if mtls {
		return privateDest, privateOpts, nil
	}
	return dest, opts, &walletRESTGateway{dest: privateDest, opts: privateOpts}
}

// walletPeerKey holds the actual HTTP connection peer, not forwarding headers.
type walletPeerKey struct{}

func localWalletPeer(addr net.Addr) bool {
	switch a := addr.(type) {
	case rpcGatewayAddr:
		return true
	case *net.UnixAddr:
		return true
	case *net.TCPAddr:
		return a.IP.IsLoopback()
	default:
		return false
	}
}

func localWalletUnlockerAuthorize(ctx context.Context, method string) error {
	if !strings.HasPrefix(method, "/lnrpc.WalletUnlocker/") {
		return nil
	}
	p, ok := peer.FromContext(ctx)
	if !ok || !localWalletPeer(p.Addr) {
		return status.Error(codes.PermissionDenied, "WalletUnlocker requires a local connection")
	}
	return nil
}

func localWalletUnlockerUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := localWalletUnlockerAuthorize(ctx, info.FullMethod); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func localWalletUnlockerStream(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := localWalletUnlockerAuthorize(stream.Context(), info.FullMethod); err != nil {
		return err
	}
	return handler(srv, stream)
}

// localWalletUnlockerHTTP checks the original peer both before WebSocket
// upgrade and after request reconstruction. These static paths match the
// grpc-gateway's decoded URL.Path in legacy mode.
func localWalletUnlockerHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/genseed", "/v1/initwallet", "/v1/unlockwallet", "/v1/changepassword":
			addr, _ := r.Context().Value(walletPeerKey{}).(net.Addr)
			if !localWalletPeer(addr) {
				http.Error(w, "WalletUnlocker requires a local connection", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// restrictWalletUnlocker reports whether local access is required. The opt-out
// never disables RPC client authentication on an mTLS listener.
func (cfg *Config) restrictWalletUnlocker(auth *rpcClientAuth) bool {
	return auth == nil && !cfg.AllowRemoteWalletUnlocker
}

// newWalletRESTHandler applies the second check immediately before gateway
// dispatch, including to requests reconstructed by the WebSocket proxy.
func newWalletRESTHandler(mux http.Handler, cfg *Config, auth *rpcClientAuth) http.Handler {
	if cfg.restrictWalletUnlocker(auth) {
		mux = localWalletUnlockerHTTP(mux)
	}
	return lnrpc.NewWebSocketProxy(
		mux, rpcsLog, cfg.WSPingInterval, cfg.WSPongWait,
		lnrpc.LndClientStreamingURIs,
	)
}
