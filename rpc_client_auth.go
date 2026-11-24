package lnd

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/lightningnetwork/lnd/internal/rpcclientallowlist"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type rpcClientFingerprints map[[sha256.Size]byte]struct{}

// rpcClientAuth applies the same certificate policy at both public RPC entry
// points. The CA pool is immutable; an entire allowlist is replaced on reload.
type rpcClientAuth struct {
	roots *x509.CertPool
	path  string
	allow atomic.Pointer[rpcClientFingerprints]
}

func newRPCClientAuth(caPath, allowlistPath string) (*rpcClientAuth, error) {
	pem, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read RPC client CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("RPC client CA file has no certificates")
	}

	a := &rpcClientAuth{roots: roots, path: allowlistPath}
	if err := a.reload(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *rpcClientAuth) reload() error {
	f, err := os.Open(a.path)
	if err != nil {
		return fmt.Errorf("open RPC client allowlist: %w", err)
	}
	defer f.Close()

	entries, err := rpcclientallowlist.Parse(f)
	if err != nil {
		return fmt.Errorf("RPC client allowlist: %w", err)
	}
	allow := rpcClientFingerprints(entries)
	a.allow.Store(&allow)
	return nil
}

func (a *rpcClientAuth) watch(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var lastError string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.reload(); err != nil {
				if err.Error() != lastError {
					rpcsLog.Warnf("Keeping previous RPC client "+
						"allowlist: %v", err)
				}
				lastError = err.Error()
			} else {
				lastError = ""
			}
		}
	}
}

func (a *rpcClientAuth) verify(state tls.ConnectionState) error {
	if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
		return errors.New("RPC client certificate is not verified")
	}
	leaf := state.PeerCertificates[0]
	if now := time.Now(); now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return errors.New("RPC client certificate is outside its validity period")
	}
	fingerprint := sha256.Sum256(leaf.Raw)
	if _, ok := (*a.allow.Load())[fingerprint]; !ok {
		return errors.New("RPC client certificate is not in the allowlist")
	}
	return nil
}

func (a *rpcClientAuth) publicTLS(base *tls.Config) *tls.Config {
	cfg := base.Clone()
	cfg.ClientAuth = tls.RequireAndVerifyClientCert
	cfg.ClientCAs = a.roots
	cfg.VerifyConnection = a.verify
	cfg.SessionTicketsDisabled = true
	return cfg
}

// grpcTLS accepts ordinary TLS only for connections from the in-process REST
// gateway. The private address marker is set by the gateway listener, not by
// the network or RPC metadata. Both paths use regular lnd RPC authorization.
func (a *rpcClientAuth) grpcTLS(base *tls.Config) *tls.Config {
	public := a.publicTLS(base)
	private := base.Clone()
	private.NextProtos = []string{"h2"}
	private.SessionTicketsDisabled = true
	public.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if _, ok := hello.Conn.RemoteAddr().(rpcGatewayAddr); ok {
			return private, nil
		}
		return nil, nil
	}
	return public
}

func (a *rpcClientAuth) authorize(ctx context.Context) error {
	p, ok := peer.FromContext(ctx)
	if ok {
		if _, internal := p.Addr.(rpcGatewayAddr); internal {
			return nil
		}
		if info, ok := p.AuthInfo.(credentials.TLSInfo); ok &&
			a.verify(info.State) == nil {

			return nil
		}
	}
	return status.Error(codes.Unauthenticated, "RPC client certificate not authorized")
}

func (a *rpcClientAuth) unary(ctx context.Context, req any,
	_ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {

	if err := a.authorize(ctx); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func (a *rpcClientAuth) stream(srv any, stream grpc.ServerStream,
	_ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {

	if err := a.authorize(stream.Context()); err != nil {
		return err
	}
	return handler(srv, stream)
}

func (a *rpcClientAuth) httpHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || a.verify(*r.TLS) != nil {
			http.Error(w, "RPC client certificate not authorized",
				http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rpcGatewayListener only wraps the private in-memory REST gateway listener.
// A public listener must never return a connection with this marker.
type rpcGatewayListener struct{ net.Listener }

type rpcGatewayAddr struct{ net.Addr }

type rpcGatewayConn struct{ net.Conn }

func (l rpcGatewayListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return rpcGatewayConn{conn}, nil
}

func (c rpcGatewayConn) RemoteAddr() net.Addr {
	return rpcGatewayAddr{c.Conn.RemoteAddr()}
}
