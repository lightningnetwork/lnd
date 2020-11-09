// The code in this file is a heavily modified version of
// https://github.com/tmc/grpc-websocket-proxy/

package lnrpc

import (
	"bufio"
	"io"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/btcsuite/btclog"
	"github.com/gorilla/websocket"
	"golang.org/x/net/context"
)

const (
	// MethodOverrideParam is the GET query parameter that specifies what
	// HTTP request method should be used for the forwarded REST request.
	// This is necessary because the WebSocket API specifies that a
	// handshake request must always be done through a GET request.
	MethodOverrideParam = "method"

	// HeaderWebSocketProtocol is the name of the WebSocket protocol
	// exchange header field that we use to transport additional header
	// fields.
	HeaderWebSocketProtocol = "Sec-Websocket-Protocol"

	// WebSocketProtocolDelimiter is the delimiter we use between the
	// additional header field and its value. We use the plus symbol because
	// the default delimiters aren't allowed in the protocol names.
	WebSocketProtocolDelimiter = "+"
)

var (
	// defaultHeadersToForward is a map of all HTTP header fields that are
	// forwarded by default. The keys must be in the canonical MIME header
	// format.
	defaultHeadersToForward = map[string]bool{
		"Origin":                 true,
		"Referer":                true,
		"Grpc-Metadata-Macaroon": true,
	}

	// defaultProtocolsToAllow are additional header fields that we allow
	// to be transported inside of the Sec-Websocket-Protocol field to be
	// forwarded to the backend.
	defaultProtocolsToAllow = map[string]bool{
		"Grpc-Metadata-Macaroon": true,
	}
)

// NewWebSocketProxy attempts to expose the underlying handler as a response-
// streaming WebSocket stream with newline-delimited JSON as the content
// encoding.
func NewWebSocketProxy(h http.Handler, logger btclog.Logger) http.Handler {
	p := &WebsocketProxy{
		backend: h,
		logger:  logger,
		upgrader: &websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}
	return p
}

// WebsocketProxy provides websocket transport upgrade to compatible endpoints.
type WebsocketProxy struct {
	backend  http.Handler
	logger   btclog.Logger
	upgrader *websocket.Upgrader
}

// ServeHTTP handles the incoming HTTP request. If the request is an
// "upgradeable" WebSocket request (identified by header fields), then the
// WS proxy handles the request. Otherwise the request is passed directly to the
// underlying REST proxy.
func (p *WebsocketProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		p.backend.ServeHTTP(w, r)
		return
	}
	p.upgradeToWebSocketProxy(w, r)
}

// upgradeToWebSocketProxy upgrades the incoming request to a WebSocket, reads
// one incoming message then streams all responses until either the client or
// server quit the connection.
func (p *WebsocketProxy) upgradeToWebSocketProxy(w http.ResponseWriter,
	r *http.Request) {

	conn, err := p.upgrader.Upgrade(w, r, nil)
	if err != nil {
		p.logger.Errorf("error upgrading websocket:", err)
		return
	}
	defer func() {
		err := conn.Close()
		if err != nil && !IsClosedConnError(err) {
			p.logger.Errorf("WS: error closing upgraded conn: %v",
				err)
		}
	}()

	ctx, cancelFn := context.WithCancel(context.Background())
	defer cancelFn()

	requestForwarder := newRequestForwardingReader()
	request, err := http.NewRequestWithContext(
		r.Context(), r.Method, r.URL.String(), requestForwarder,
	)
	if err != nil {
		p.logger.Errorf("WS: error preparing request:", err)
		return
	}

	// Allow certain headers to be forwarded, either from source headers
	// or the special Sec-Websocket-Protocol header field.
	forwardHeaders(r.Header, request.Header)

	// Also allow the target request method to be overwritten, as all
	// WebSocket establishment calls MUST be GET requests.
	if m := r.URL.Query().Get(MethodOverrideParam); m != "" {
		request.Method = m
	}

	responseForwarder := newResponseForwardingWriter()
	go func() {
		<-ctx.Done()
		responseForwarder.Close()
	}()

	go func() {
		defer cancelFn()
		p.backend.ServeHTTP(responseForwarder, request)
	}()

	// Read loop: Take messages from websocket and write to http request.
	go func() {
		defer cancelFn()
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			_, payload, err := conn.ReadMessage()
			if err != nil {
				if IsClosedConnError(err) {
					p.logger.Tracef("WS: socket "+
						"closed: %v", err)
					return
				}
				p.logger.Errorf("error reading message: %v",
					err)
				return
			}
			_, err = requestForwarder.Write(payload)
			if err != nil {
				p.logger.Errorf("WS: error writing message "+
					"to upstream http server: %v", err)
				return
			}
			_, _ = requestForwarder.Write([]byte{'\n'})

			// We currently only support server-streaming messages.
			// Therefore we close the request body after the first
			// incoming message to trigger a response.
			requestForwarder.CloseWriter()
		}
	}()

	// Write loop: Take messages from the response forwarder and write them
	// to the WebSocket.
	for responseForwarder.Scan() {
		if len(responseForwarder.Bytes()) == 0 {
			p.logger.Errorf("WS: empty scan: %v",
				responseForwarder.Err())

			continue
		}

		err = conn.WriteMessage(
			websocket.TextMessage, responseForwarder.Bytes(),
		)
		if err != nil {
			p.logger.Errorf("WS: error writing message: %v", err)
			return
		}
	}
	if err := responseForwarder.Err(); err != nil && !IsClosedConnError(err) {
		p.logger.Errorf("WS: scanner err: %v", err)
	}
}

// forwardHeaders forwards certain allowed header fields from the source request
// to the target request. Because browsers are limited in what header fields
// they can send on the WebSocket setup call, we also allow additional fields to
// be transported in the special Sec-Websocket-Protocol field.
func forwardHeaders(source, target http.Header) {
	// Forward allowed header fields directly.
	for header := range source {
		headerName := textproto.CanonicalMIMEHeaderKey(header)
		forward, ok := defaultHeadersToForward[headerName]
		if ok && forward {
			target.Set(headerName, source.Get(header))
		}
	}

	// Browser aren't allowed to set custom header fields on WebSocket
	// requests. We need to allow them to submit the macaroon as a WS
	// protocol, which is the only allowed header. Set any "protocols" we
	// declare valid as header fields on the forwarded request.
	protocol := source.Get(HeaderWebSocketProtocol)
	for key := range defaultProtocolsToAllow {
		if strings.HasPrefix(protocol, key) {
			// The format is "<protocol name>+<value>". We know the
			// protocol string starts with the name so we only need
			// to set the value.
			values := strings.Split(
				protocol, WebSocketProtocolDelimiter,
			)
			target.Set(key, values[1])
		}
	}
}

// newRequestForwardingReader creates a new request forwarding pipe.
func newRequestForwardingReader() *requestForwardingReader {
	r, w := io.Pipe()
	return &requestForwardingReader{
		Reader: r,
		Writer: w,
		pipeR:  r,
		pipeW:  w,
	}
}

// requestForwardingReader is a wrapper around io.Pipe that embeds both the
// io.Reader and io.Writer interface and can be closed.
type requestForwardingReader struct {
	io.Reader
	io.Writer

	pipeR *io.PipeReader
	pipeW *io.PipeWriter
}

// CloseWriter closes the underlying pipe writer.
func (r *requestForwardingReader) CloseWriter() {
	_ = r.pipeW.CloseWithError(io.EOF)
}

// newResponseForwardingWriter creates a new http.ResponseWriter that intercepts
// what's written to it and presents it through a bufio.Scanner interface.
func newResponseForwardingWriter() *responseForwardingWriter {
	r, w := io.Pipe()
	return &responseForwardingWriter{
		Writer:  w,
		Scanner: bufio.NewScanner(r),
		pipeR:   r,
		pipeW:   w,
		header:  http.Header{},
		closed:  make(chan bool, 1),
	}
}

// responseForwardingWriter is a type that implements the http.ResponseWriter
// interface but internally forwards what's written to the writer through a pipe
// so it can easily be read again through the bufio.Scanner interface.
type responseForwardingWriter struct {
	io.Writer
	*bufio.Scanner

	pipeR *io.PipeReader
	pipeW *io.PipeWriter

	header http.Header
	code   int
	closed chan bool
}

// Write writes the given bytes to the internal pipe.
//
// NOTE: This is part of the http.ResponseWriter interface.
func (w *responseForwardingWriter) Write(b []byte) (int, error) {
	return w.Writer.Write(b)
}

// Header returns the HTTP header fields intercepted so far.
//
// NOTE: This is part of the http.ResponseWriter interface.
func (w *responseForwardingWriter) Header() http.Header {
	return w.header
}

// WriteHeader indicates that the header part of the response is now finished
// and sets the response code.
//
// NOTE: This is part of the http.ResponseWriter interface.
func (w *responseForwardingWriter) WriteHeader(code int) {
	w.code = code
}

// CloseNotify returns a channel that indicates if a connection was closed.
//
// NOTE: This is part of the http.CloseNotifier interface.
func (w *responseForwardingWriter) CloseNotify() <-chan bool {
	return w.closed
}

// Flush empties all buffers. We implement this to indicate to our backend that
// we support flushing our content. There is no actual implementation because
// all writes happen immediately, there is no internal buffering.
//
// NOTE: This is part of the http.Flusher interface.
func (w *responseForwardingWriter) Flush() {}

func (w *responseForwardingWriter) Close() {
	_ = w.pipeR.CloseWithError(io.EOF)
	_ = w.pipeW.CloseWithError(io.EOF)
	w.closed <- true
}

// IsClosedConnError is a helper function that returns true if the given error
// is an error indicating we are using a closed connection.
func IsClosedConnError(err error) bool {
	if err == nil {
		return false
	}
	if err == http.ErrServerClosed {
		return true
	}

	str := err.Error()
	if strings.Contains(str, "use of closed network connection") {
		return true
	}
	if strings.Contains(str, "closed pipe") {
		return true
	}
	if strings.Contains(str, "broken pipe") {
		return true
	}
	return websocket.IsCloseError(
		err, websocket.CloseNormalClosure, websocket.CloseGoingAway,
	)
}
