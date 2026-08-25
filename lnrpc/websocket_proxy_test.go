package lnrpc

import (
	"net/http"
	"testing"

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
