package lnrpc

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// TestForwardHeaders makes sure the headers of an incoming WebSocket request
// are forwarded to the upstream HTTP request correctly. That includes the
// special Sec-Websocket-Protocol field, which browsers use to transport header
// fields they aren't allowed to set on a WebSocket request directly.
func TestForwardHeaders(t *testing.T) {
	t.Parallel()

	const macaroon = "0201036c6e6402eb01030a10"

	testCases := []struct {
		name     string
		source   http.Header
		expected http.Header
	}{{
		name:     "no headers",
		source:   http.Header{},
		expected: http.Header{},
	}, {
		name: "allowed header is forwarded",
		source: http.Header{
			"Grpc-Metadata-Macaroon": []string{macaroon},
		},
		expected: http.Header{
			"Grpc-Metadata-Macaroon": []string{macaroon},
		},
	}, {
		name: "disallowed header is dropped",
		source: http.Header{
			"Authorization": []string{"Bearer foo"},
		},
		expected: http.Header{},
	}, {
		name: "macaroon in protocol field is forwarded",
		source: http.Header{
			HeaderWebSocketProtocol: []string{
				"Grpc-Metadata-Macaroon+" + macaroon,
			},
		},
		expected: http.Header{
			"Grpc-Metadata-Macaroon": []string{macaroon},
		},
	}, {
		// A client is free to send the protocol name without the
		// delimiter and value. There is nothing to forward in that
		// case, and we must not attempt to read a value that isn't
		// there.
		name: "protocol field without delimiter is ignored",
		source: http.Header{
			HeaderWebSocketProtocol: []string{
				"Grpc-Metadata-Macaroon",
			},
		},
		expected: http.Header{},
	}, {
		name: "unknown protocol field is ignored",
		source: http.Header{
			HeaderWebSocketProtocol: []string{"some-protocol"},
		},
		expected: http.Header{},
	}, {
		// The protocol field is a comma separated list, so a bare
		// allowed protocol name must not be able to borrow the value
		// of a different sub protocol in the same list.
		name: "bare allowed protocol followed by valued protocol",
		source: http.Header{
			HeaderWebSocketProtocol: []string{
				"Grpc-Metadata-Macaroon,other+value",
			},
		},
		expected: http.Header{},
	}, {
		// An allowed protocol is forwarded no matter where in the
		// list it appears.
		name: "allowed protocol in list is forwarded",
		source: http.Header{
			HeaderWebSocketProtocol: []string{
				"other+value, Grpc-Metadata-Macaroon+" +
					macaroon,
			},
		},
		expected: http.Header{
			"Grpc-Metadata-Macaroon": []string{macaroon},
		},
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			target := http.Header{}
			forwardHeaders(tc.source, target)

			require.Equal(t, tc.expected, target)
		})
	}
}

// TestWebSocketProxyReadLimit makes sure the proxy refuses incoming WebSocket
// messages that are larger than MaxWsMsgSize instead of reading them into
// memory in full.
func TestWebSocketProxyReadLimit(t *testing.T) {
	t.Parallel()

	// The backend just blocks until the request is cancelled. The
	// oversized message should never make it this far.
	backend := http.HandlerFunc(
		func(_ http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		},
	)

	server := httptest.NewServer(NewWebSocketProxy(
		backend, btclog.Disabled, 0, 0, nil,
	))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, resp.Body.Close())
		require.NoError(t, conn.Close())
	}()

	// A message within the limit is accepted and forwarded.
	err = conn.WriteMessage(websocket.TextMessage, make([]byte, 1024))
	require.NoError(t, err)

	// One byte over the limit must not be. We don't assert on the write
	// error, since the proxy may tear the connection down while we're
	// still writing.
	_ = conn.WriteMessage(
		websocket.TextMessage, make([]byte, MaxWsMsgSize+1),
	)

	// The deadline is what keeps a regression from turning into a hang:
	// without a read limit the proxy buffers the oversized payload and
	// forwards it, so nothing ever closes the connection and this read
	// would block forever.
	err = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	require.NoError(t, err)

	// We don't assert on the exact close code. The server writes a
	// "message too big" close frame, but it then closes a connection with
	// several megabytes still unread, so the client may see a reset
	// instead. Either is a rejection; a timeout is not.
	// Note that gorilla replaces a timeout with an error type of its own
	// that doesn't wrap the original, so this has to go through net.Error
	// rather than os.ErrDeadlineExceeded.
	_, _, err = conn.ReadMessage()
	require.Error(t, err)

	var netErr net.Error
	timedOut := errors.As(err, &netErr) && netErr.Timeout()
	require.False(
		t, timedOut,
		"connection stayed open, oversized message was accepted",
	)
}
