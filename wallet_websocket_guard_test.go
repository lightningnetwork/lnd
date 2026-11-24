package lnd

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/stretchr/testify/require"
)

// TestWalletRESTStack exercises the actual WebSocket proxy and both guards.
func TestWalletRESTStack(t *testing.T) {
	for _, local := range []bool{false, true} {
		name := "remote"
		if local {
			name = "local"
		}
		t.Run(name, func(t *testing.T) {
			gotPeer := make(chan net.Addr, 2)
			mux := runtime.NewServeMux()
			serveSeed := func(w http.ResponseWriter,
				r *http.Request, _ map[string]string) {

				value := r.Context().Value(walletPeerKey{})
				addr, _ := value.(net.Addr)
				gotPeer <- addr
				_, _ = w.Write([]byte("{}"))
			}
			err := mux.HandlePath(
				http.MethodGet, "/v1/genseed", serveSeed,
			)
			require.NoError(t, err)
			handler := localWalletUnlockerHTTP(
				newWalletRESTHandler(mux, &Config{}, nil),
			)
			server := httptest.NewUnstartedServer(handler)
			addr := &net.TCPAddr{
				IP: net.ParseIP("192.0.2.7"), Port: 1234,
			}
			if local {
				addr.IP = net.ParseIP("127.0.0.1")
			}
			server.Config.ConnContext = func(ctx context.Context,
				_ net.Conn) context.Context {

				return context.WithValue(
					ctx, walletPeerKey{}, addr,
				)
			}
			server.Start()
			defer server.Close()
			response, err := server.Client().Get(
				server.URL + "/v1/genseed",
			)
			require.NoError(t, err)
			response.Body.Close()
			want := http.StatusForbidden
			if local {
				want = http.StatusOK
			}
			if response.StatusCode != want {
				require.Equal(t, want, response.StatusCode)
			}
			if local {
				if got := <-gotPeer; got != addr {
					t.Fatalf("HTTP peer %v", got)
				}
			}
			wsURL := "ws" + strings.TrimPrefix(server.URL, "http") +
				"/v1/genseed"
			conn, response, err := websocket.DefaultDialer.Dial(
				wsURL, nil,
			)
			if !local {
				if err == nil {
					conn.Close()
					t.Fatal("remote WebSocket upgraded")
				}
				require.NotNil(t, response)
				code := response.StatusCode
				require.Equal(t, http.StatusForbidden, code)
				response.Body.Close()

				return
			}
			require.NoError(t, err)
			defer conn.Close()
			err = conn.WriteMessage(
				websocket.TextMessage, []byte("{}"),
			)
			require.NoError(t, err)
			select {
			case got := <-gotPeer:
				if got != addr {
					require.Equal(t, addr, got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("gateway did not receive request")
			}
		})
	}
}

// TestWalletUnlockerOptOut checks the policy shared by gRPC and REST.
func TestWalletUnlockerOptOut(t *testing.T) {
	cfg := &Config{AllowRemoteWalletUnlocker: true}
	if cfg.restrictWalletUnlocker(nil) ||
		cfg.restrictWalletUnlocker(&rpcClientAuth{}) {

		t.Fatal("opt-out must not install the locality check")
	}
	backend := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}
	handler := newWalletRESTHandler(http.HandlerFunc(backend), cfg, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/genseed", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusNoContent {
		t.Fatalf("opt-out response %d, want %d",
			response.Code, http.StatusNoContent)
	}
}

// TestWalletRESTDispatchGuard covers the inner guard independently of the
// pre-upgrade guard, using the same gateway handler as the production stack.
func TestWalletRESTDispatchGuard(t *testing.T) {
	called := false
	backend := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	})
	handler := newWalletRESTHandler(backend, &Config{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/genseed", nil)
	req = req.WithContext(context.WithValue(
		req.Context(), walletPeerKey{},
		&net.TCPAddr{IP: net.ParseIP("192.0.2.7")},
	))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if called || response.Code != http.StatusForbidden {
		t.Fatalf("called=%v status=%d", called, response.Code)
	}
}
