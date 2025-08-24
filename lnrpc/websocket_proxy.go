// The code in this file is a heavily modified version of
// https://github.com/tmc/grpc-websocket-proxy/

package lnrpc

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/textproto"
	"regexp"
	"strings"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/gorilla/websocket"
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

	// PingContent is the content of the ping message we send out. This is
	// an arbitrary non-empty message that has no deeper meaning but should
	// be sent back by the client in the pong message.
	PingContent = "are you there?"

	// MaxWsMsgSize is the largest websockets message we'll attempt to
	// decode in the gRPC <-> WS proxy. gRPC has a similar setting used
	// elsewhere.
	MaxWsMsgSize = 4 * 1024 * 1024
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

	// DefaultPingInterval is the default number of seconds to wait between
	// sending ping requests.
	DefaultPingInterval = time.Second * 30

	// DefaultPongWait is the maximum duration we wait for a pong response
	// to a ping we sent before we assume the connection died.
	DefaultPongWait = time.Second * 5
)

// NewWebSocketProxy attempts to expose the underlying handler as a response-
// streaming WebSocket stream with newline-delimited JSON as the content
// encoding. If pingInterval is a non-zero duration, a ping message will be
// sent out periodically and a pong response message is expected from the
// client. The clientStreamingURIs parameter can hold a list of all patterns
// for URIs that are mapped to client-streaming RPC methods. We need to keep
// track of those to make sure we initialize the request body correctly for the
// underlying grpc-gateway library.
func NewWebSocketProxy(h http.Handler, logger btclog.Logger,
	pingInterval, pongWait time.Duration,
	clientStreamingURIs []*regexp.Regexp) http.Handler {

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
		clientStreamingURIs: clientStreamingURIs,
	}

	if pingInterval > 0 && pongWait > 0 {
		p.pingInterval = pingInterval
		p.pongWait = pongWait
	}

	return p
}

// WebsocketProxy provides websocket transport upgrade to compatible endpoints.
type WebsocketProxy struct {
	backend  http.Handler
	logger   btclog.Logger
	upgrader *websocket.Upgrader

	// clientStreamingURIs holds a list of all patterns for URIs that are
	// mapped to client-streaming RPC methods. We need to keep track of
	// those to make sure we initialize the request body correctly for the
	// underlying grpc-gateway library.
	clientStreamingURIs []*regexp.Regexp

	pingInterval time.Duration
	pongWait     time.Duration
}

// pingPongEnabled returns true if a ping interval is set to enable sending and
// expecting regular ping/pong messages.
func (p *WebsocketProxy) pingPongEnabled() bool {
	return p.pingInterval > 0 && p.pongWait > 0
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

	ctx, cancelFn := context.WithCancel(r.Context())
	defer cancelFn()

	requestForwarder := newRequestForwardingReader()
	request, err := http.NewRequestWithContext(
		ctx, r.Method, r.URL.String(), requestForwarder,
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

	// Is this a call to a client-streaming RPC method?
	clientStreaming := false
	for _, pattern := range p.clientStreamingURIs {
		if pattern.MatchString(r.URL.Path) {
			clientStreaming = true
		}
	}

	responseForwarder := newResponseForwardingWriter()
	go func() {
		<-ctx.Done()
		responseForwarder.Close()
		requestForwarder.CloseWriter()
	}()

	go func() {
		defer cancelFn()
		p.backend.ServeHTTP(responseForwarder, request)
	}()

	// Read loop: Take messages from websocket and write them to the payload
	// channel. This needs to be its own goroutine because for non-client
	// streaming RPCs, the requestForwarder.Write() in the second goroutine
	// will block until the request has fully completed. But for the ping/
	// pong handler to work, we need to have an active call to
	// conn.ReadMessage() going on. So we make sure we have such an active
	// call by starting a second read as soon as the first one has
	// completed.
	payloadChannel := make(chan []byte, 1)
	go func() {
		defer cancelFn()
		defer close(payloadChannel)

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

			select {
			case payloadChannel <- payload:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Forward loop: Take messages from the incoming payload channel and
	// write them to the http request.
	go func() {
		defer cancelFn()
		for {
			var payload []byte
			select {
			case <-ctx.Done():
				return
			case newPayload, more := <-payloadChannel:
				if !more {
					p.logger.Infof("WS: incoming payload " +
						"chan closed")
					return
				}

				payload = newPayload
			}

			_, err := requestForwarder.Write(payload)
			if err != nil {
				p.logger.Errorf("WS: error writing message "+
					"to upstream http server: %v", err)
				return
			}
			_, _ = requestForwarder.Write([]byte{'\n'})

			// The grpc-gateway library uses a different request
			// reader depending on whether it is a client streaming
			// RPC or not. For a non-streaming request we need to
			// close with EOF to signal the request was completed.
			if !clientStreaming {
				requestForwarder.CloseWriter()
			}
		}
	}()

	// Ping write loop: Send a ping message regularly if ping/pong is
	// enabled.
	if p.pingPongEnabled() {
		// We'll send out our first ping in pingInterval. So the initial
		// deadline is that interval plus the time we allow for a
		// response to be sent.
		initialDeadline := time.Now().Add(p.pingInterval + p.pongWait)
		_ = conn.SetReadDeadline(initialDeadline)

		// Whenever a pong message comes in, we extend the deadline
		// until the next read is expected by the interval plus pong
		// wait time. Since we can never _reach_ any of the deadlines,
		// we also have to advance the deadline for the next expected
		// write to happen, in case the next thing we actually write is
		// the next ping.
		conn.SetPongHandler(func(appData string) error {
			nextDeadline := time.Now().Add(
				p.pingInterval + p.pongWait,
			)
			_ = conn.SetReadDeadline(nextDeadline)
			_ = conn.SetWriteDeadline(nextDeadline)

			return nil
		})
		go func() {
			ticker := time.NewTicker(p.pingInterval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					p.logger.Debug("WS: ping loop done")
					return

				case <-ticker.C:
					// Writing the ping shouldn't take any
					// longer than we'll wait for a response
					// in the first place.
					writeDeadline := time.Now().Add(
						p.pongWait,
					)
					err := conn.WriteControl(
						websocket.PingMessage,
						[]byte(PingContent),
						writeDeadline,
					)
					if err != nil {
						p.logger.Warnf("WS: could not "+
							"send ping message: %v",
							err)
						return
					}
				}
			}
		}()
	}

	// Write loop: Take messages from the response forwarder and write them
	// to the WebSocket.
	for responseForwarder.Scan() {
		if len(responseForwarder.Bytes()) == 0 {
			p.logger.Errorf("WS: empty scan: %v",
				responseForwarder.Err())

			continue
		}

		err := conn.WriteMessage(
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

	scanner := bufio.NewScanner(r)

	// We pass in a custom buffer for the bufio scanner to use. We'll keep
	// with a normal 64KB buffer, but allow a larger max message size,
	// which may cause buffer expansion when needed.
	buf := make([]byte, 0, bufio.MaxScanTokenSize)
	scanner.Buffer(buf, MaxWsMsgSize)

	return &responseForwardingWriter{
		Writer:  w,
		Scanner: scanner,
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
	if strings.Contains(str, "connection reset by peer") {
		return true
	}
	return websocket.IsCloseError(
		err, websocket.CloseNormalClosure, websocket.CloseGoingAway,
	)
}
