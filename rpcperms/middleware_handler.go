package rpcperms

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"gopkg.in/macaroon.v2"
)

var (
	// ErrShuttingDown is the error that's returned when the server is
	// shutting down and a request cannot be served anymore.
	ErrShuttingDown = errors.New("server shutting down")

	// ErrTimeoutReached is the error that's returned if any of the
	// middleware's tasks is not completed in the given time.
	ErrTimeoutReached = errors.New("intercept timeout reached")

	// errClientQuit is the error that's returned if the client closes the
	// middleware communication stream before a request was fully handled.
	errClientQuit = errors.New("interceptor RPC client quit")
)

// MiddlewareHandler is a type that communicates with a middleware over the
// established bi-directional RPC stream. It sends messages to the middleware
// whenever the custom business logic implemented there should give feedback to
// a request or response that's happening on the main gRPC server.
type MiddlewareHandler struct {
	// lastMsgID is the ID of the last intercept message that was forwarded
	// to the middleware.
	//
	// NOTE: Must be used atomically!
	lastMsgID uint64

	middlewareName string

	readOnly bool

	customCaveatName string

	receive func() (*lnrpc.RPCMiddlewareResponse, error)

	send func(request *lnrpc.RPCMiddlewareRequest) error

	interceptRequests chan *interceptRequest

	timeout time.Duration

	// params are our current chain params.
	params *chaincfg.Params

	// done is closed when the rpc client terminates.
	done chan struct{}

	// quit is closed when lnd is shutting down.
	quit chan struct{}

	wg sync.WaitGroup
}

// NewMiddlewareHandler creates a new handler for the middleware with the given
// name and custom caveat name.
func NewMiddlewareHandler(name, customCaveatName string, readOnly bool,
	receive func() (*lnrpc.RPCMiddlewareResponse, error),
	send func(request *lnrpc.RPCMiddlewareRequest) error,
	timeout time.Duration, params *chaincfg.Params,
	quit chan struct{}) *MiddlewareHandler {

	// We explicitly want to log this as a warning since intercepting any
	// gRPC messages can also be used for malicious purposes and the user
	// should be made aware of the risks.
	log.Warnf("A new gRPC middleware with the name '%s' was registered "+
		" with custom_macaroon_caveat='%s', read_only=%v. Make sure "+
		"you trust the middleware author since that code will be able "+
		"to intercept and possibly modify any gRPC messages sent/"+
		"received to/from a client that has a macaroon with that "+
		"custom caveat.", name, customCaveatName, readOnly)

	return &MiddlewareHandler{
		middlewareName:    name,
		customCaveatName:  customCaveatName,
		readOnly:          readOnly,
		receive:           receive,
		send:              send,
		interceptRequests: make(chan *interceptRequest),
		timeout:           timeout,
		params:            params,
		done:              make(chan struct{}),
		quit:              quit,
	}
}

// intercept handles the full interception lifecycle of a single middleware
// event (stream authentication, request interception or response interception).
// The lifecycle consists of sending a message to the middleware, receiving a
// feedback on it and sending the feedback to the appropriate channel. All steps
// are guarded by the configured timeout to make sure a middleware cannot slow
// down requests too much.
func (h *MiddlewareHandler) intercept(requestID uint64,
	req *InterceptionRequest) (*interceptResponse, error) {

	respChan := make(chan *interceptResponse, 1)

	newRequest := &interceptRequest{
		requestID: requestID,
		request:   req,
		response:  respChan,
	}

	// timeout is the time after which intercept requests expire.
	timeout := time.After(h.timeout)

	// Send the request to the interceptRequests channel for the main
	// goroutine to be picked up.
	select {
	case h.interceptRequests <- newRequest:

	case <-timeout:
		log.Errorf("MiddlewareHandler returned error - reached "+
			"timeout of %v for request interception", h.timeout)

		return nil, ErrTimeoutReached

	case <-h.done:
		return nil, errClientQuit

	case <-h.quit:
		return nil, ErrShuttingDown
	}

	// Receive the response and return it. If no response has been received
	// in AcceptorTimeout, then return false.
	select {
	case resp := <-respChan:
		return resp, nil

	case <-timeout:
		log.Errorf("MiddlewareHandler returned error - reached "+
			"timeout of %v for response interception", h.timeout)
		return nil, ErrTimeoutReached

	case <-h.done:
		return nil, errClientQuit

	case <-h.quit:
		return nil, ErrShuttingDown
	}
}

// Run is the main loop for the middleware handler. This function will block
// until it receives the signal that lnd is shutting down, or the rpc stream is
// cancelled by the client.
func (h *MiddlewareHandler) Run() error {
	// Wait for our goroutines to exit before we return.
	defer h.wg.Wait()
	defer log.Debugf("Exiting middleware run loop for %s", h.middlewareName)

	// Create a channel that responses from middlewares are sent into.
	responses := make(chan *lnrpc.RPCMiddlewareResponse)

	// errChan is used by the receive loop to signal any errors that occur
	// during reading from the stream. This is primarily used to shutdown
	// the send loop in the case of an RPC client disconnecting.
	errChan := make(chan error, 1)

	// Start a goroutine to receive responses from the interceptor. We
	// expect the receive function to block, so it must be run in a
	// goroutine (otherwise we could not send more than one intercept
	// request to the client).
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()

		h.receiveResponses(errChan, responses)
	}()

	return h.sendInterceptRequests(errChan, responses)
}

// receiveResponses receives responses for our intercept requests and dispatches
// them into the responses channel provided, sending any errors that occur into
// the error channel provided.
func (h *MiddlewareHandler) receiveResponses(errChan chan error,
	responses chan *lnrpc.RPCMiddlewareResponse) {

	for {
		resp, err := h.receive()
		if err != nil {
			errChan <- err
			return
		}

		select {
		case responses <- resp:

		case <-h.done:
			return

		case <-h.quit:
			return
		}
	}
}

// sendInterceptRequests handles intercept requests sent to us by our Accept()
// function, dispatching them to our acceptor stream and coordinating return of
// responses to their callers.
func (h *MiddlewareHandler) sendInterceptRequests(errChan chan error,
	responses chan *lnrpc.RPCMiddlewareResponse) error {

	// Close the done channel to indicate that the interceptor is no longer
	// listening and any in-progress requests should be terminated.
	defer close(h.done)

	interceptRequests := make(map[uint64]*interceptRequest)

	for {
		select {
		// Consume requests passed to us from our Accept() function and
		// send them into our stream.
		case newRequest := <-h.interceptRequests:
			msgID := atomic.AddUint64(&h.lastMsgID, 1)

			req := newRequest.request
			interceptRequests[msgID] = newRequest

			interceptReq, err := req.ToRPC(
				newRequest.requestID, msgID,
			)
			if err != nil {
				return err
			}

			if err := h.send(interceptReq); err != nil {
				return err
			}

		// Process newly received responses from our interceptor,
		// looking the original request up in our map of requests and
		// dispatching the response.
		case resp := <-responses:
			requestInfo, ok := interceptRequests[resp.RefMsgId]
			if !ok {
				continue
			}

			response := &interceptResponse{}
			switch msg := resp.GetMiddlewareMessage().(type) {
			case *lnrpc.RPCMiddlewareResponse_Feedback:
				t := msg.Feedback
				if t.Error != "" {
					response.err = fmt.Errorf("%s", t.Error)
					break
				}

				// If there's nothing to replace, we're done,
				// this request was just accepted.
				if !t.ReplaceResponse {
					break
				}

				// We are replacing the response, the question
				// now just is: was it an error or a proper
				// proto message?
				response.replace = true
				if requestInfo.request.IsError {
					response.replacement = errors.New(
						string(t.ReplacementSerialized),
					)

					break
				}

				// Not an error but a proper proto message that
				// needs to be replaced. For that we need to
				// parse it from the raw bytes into the full RPC
				// message.
				protoMsg, err := parseProto(
					requestInfo.request.ProtoTypeName,
					t.ReplacementSerialized,
				)

				if err != nil {
					response.err = err

					break
				}

				response.replacement = protoMsg

			default:
				return fmt.Errorf("unknown middleware "+
					"message: %v", msg)
			}

			select {
			case requestInfo.response <- response:
			case <-h.quit:
			}

			delete(interceptRequests, resp.RefMsgId)

		// If we failed to receive from our middleware, we exit.
		case err := <-errChan:
			log.Errorf("Received an error: %v, shutting down", err)
			return err

		// Exit if we are shutting down.
		case <-h.quit:
			return ErrShuttingDown
		}
	}
}

// InterceptType defines the different types of intercept messages a middleware
// can receive.
type InterceptType uint8

const (
	// TypeStreamAuth is the type of intercept message that is sent when a
	// client or streaming RPC is initialized. A message with this type will
	// be sent out during stream initialization so a middleware can
	// accept/deny the whole stream instead of only single messages on the
	// stream.
	TypeStreamAuth InterceptType = 1

	// TypeRequest is the type of intercept message that is sent when an RPC
	// request message is sent to lnd. For client-streaming RPCs a new
	// message of this type is sent for each individual RPC request sent to
	// the stream. Middleware has the option to modify a request message
	// before it is delivered to lnd.
	TypeRequest InterceptType = 2

	// TypeResponse is the type of intercept message that is sent when an
	// RPC response message is sent from lnd to a client. For
	// server-streaming RPCs a new message of this type is sent for each
	// individual RPC response sent to the stream. Middleware has the option
	// to modify a response message before it is sent out to the client.
	TypeResponse InterceptType = 3
)

// InterceptionRequest is a struct holding all information that is sent to a
// middleware whenever there is something to intercept (auth, request,
// response).
type InterceptionRequest struct {
	// Type is the type of the interception message.
	Type InterceptType

	// StreamRPC is set to true if the invoked RPC method is client or
	// server streaming.
	StreamRPC bool

	// Macaroon holds the macaroon that the client sent to lnd.
	Macaroon *macaroon.Macaroon

	// RawMacaroon holds the raw binary serialized macaroon that the client
	// sent to lnd.
	RawMacaroon []byte

	// CustomCaveatName is the name of the custom caveat that the middleware
	// was intercepting for.
	CustomCaveatName string

	// CustomCaveatCondition is the condition of the custom caveat that the
	// middleware was intercepting for. This can be empty for custom caveats
	// that only have a name (marker caveats).
	CustomCaveatCondition string

	// FullURI is the full RPC method URI that was invoked.
	FullURI string

	// ProtoSerialized is the full request or response object in the
	// protobuf binary serialization format.
	ProtoSerialized []byte

	// ProtoTypeName is the fully qualified name of the protobuf type of the
	// request or response message that is serialized in the field above.
	ProtoTypeName string

	// IsError indicates that the message contained within this request is
	// an error. Will only ever be true for response messages.
	IsError bool

	// CtxMetadataPairs contains the metadata pairs that were sent along
	// with the RPC request via the context.
	CtxMetadataPairs metadata.MD
}

// NewMessageInterceptionRequest creates a new interception request for either
// a request or response message.
func NewMessageInterceptionRequest(ctx context.Context,
	authType InterceptType, isStream bool, fullMethod string,
	m interface{}) (*InterceptionRequest, error) {

	mac, rawMacaroon, err := macaroonFromContext(ctx)
	if err != nil {
		return nil, err
	}

	md, _ := metadata.FromIncomingContext(ctx)

	req := &InterceptionRequest{
		Type:             authType,
		StreamRPC:        isStream,
		Macaroon:         mac,
		RawMacaroon:      rawMacaroon,
		FullURI:          fullMethod,
		CtxMetadataPairs: md,
	}

	// The message is either a proto message or an error, we don't support
	// any other types being intercepted.
	switch t := m.(type) {
	case proto.Message:
		req.ProtoSerialized, err = proto.Marshal(t)
		if err != nil {
			return nil, fmt.Errorf("cannot marshal proto msg: %w",
				err)
		}
		req.ProtoTypeName = string(proto.MessageName(t))

	case error:
		req.ProtoSerialized = []byte(t.Error())
		req.ProtoTypeName = "error"
		req.IsError = true

	default:
		return nil, fmt.Errorf("unsupported type for interception "+
			"request: %v", m)
	}

	return req, nil
}

// NewStreamAuthInterceptionRequest creates a new interception request for a
// stream authentication message.
func NewStreamAuthInterceptionRequest(ctx context.Context,
	fullMethod string) (*InterceptionRequest, error) {

	mac, rawMacaroon, err := macaroonFromContext(ctx)
	if err != nil {
		return nil, err
	}

	return &InterceptionRequest{
		Type:        TypeStreamAuth,
		StreamRPC:   true,
		Macaroon:    mac,
		RawMacaroon: rawMacaroon,
		FullURI:     fullMethod,
	}, nil
}

// macaroonFromContext tries to extract the macaroon from the incoming context.
// If there is no macaroon, a nil error is returned since some RPCs might not
// require a macaroon. But in case there is something in the macaroon header
// field that cannot be parsed, a non-nil error is returned.
func macaroonFromContext(ctx context.Context) (*macaroon.Macaroon, []byte,
	error) {

	macHex, err := macaroons.RawMacaroonFromContext(ctx)
	if err != nil {
		// If there is no macaroon, we continue anyway as it might be an
		// RPC that doesn't require a macaroon.
		return nil, nil, nil
	}

	macBytes, err := hex.DecodeString(macHex)
	if err != nil {
		return nil, nil, err
	}

	mac := &macaroon.Macaroon{}
	if err := mac.UnmarshalBinary(macBytes); err != nil {
		return nil, nil, err
	}

	return mac, macBytes, nil
}

// ToRPC converts the interception request to its RPC counterpart.
func (r *InterceptionRequest) ToRPC(requestID,
	msgID uint64) (*lnrpc.RPCMiddlewareRequest, error) {

	mdPairs := make(
		map[string]*lnrpc.MetadataValues, len(r.CtxMetadataPairs),
	)
	for key, values := range r.CtxMetadataPairs {
		mdPairs[key] = &lnrpc.MetadataValues{
			Values: values,
		}
	}

	rpcRequest := &lnrpc.RPCMiddlewareRequest{
		RequestId:             requestID,
		MsgId:                 msgID,
		RawMacaroon:           r.RawMacaroon,
		CustomCaveatCondition: r.CustomCaveatCondition,
		MetadataPairs:         mdPairs,
	}

	switch r.Type {
	case TypeStreamAuth:
		rpcRequest.InterceptType = &lnrpc.RPCMiddlewareRequest_StreamAuth{
			StreamAuth: &lnrpc.StreamAuth{
				MethodFullUri: r.FullURI,
			},
		}

	case TypeRequest:
		rpcRequest.InterceptType = &lnrpc.RPCMiddlewareRequest_Request{
			Request: &lnrpc.RPCMessage{
				MethodFullUri: r.FullURI,
				StreamRpc:     r.StreamRPC,
				TypeName:      r.ProtoTypeName,
				Serialized:    r.ProtoSerialized,
			},
		}

	case TypeResponse:
		rpcRequest.InterceptType = &lnrpc.RPCMiddlewareRequest_Response{
			Response: &lnrpc.RPCMessage{
				MethodFullUri: r.FullURI,
				StreamRpc:     r.StreamRPC,
				TypeName:      r.ProtoTypeName,
				Serialized:    r.ProtoSerialized,
				IsError:       r.IsError,
			},
		}

	default:
		return nil, fmt.Errorf("unknown intercept type %v", r.Type)
	}

	return rpcRequest, nil
}

// interceptRequest is a struct that keeps track of an interception request sent
// out to a middleware and the response that is eventually sent back by the
// middleware.
type interceptRequest struct {
	requestID uint64
	request   *InterceptionRequest
	response  chan *interceptResponse
}

// interceptResponse is the response a middleware sends back for each
// intercepted message.
type interceptResponse struct {
	err         error
	replace     bool
	replacement interface{}
}

// parseProto parses a proto serialized message of the given type into its
// native version.
func parseProto(typeName string, serialized []byte) (proto.Message, error) {
	messageType, err := protoregistry.GlobalTypes.FindMessageByName(
		protoreflect.FullName(typeName),
	)
	if err != nil {
		return nil, err
	}
	msg := messageType.New()
	err = proto.Unmarshal(serialized, msg.Interface())
	if err != nil {
		return nil, err
	}

	return msg.Interface(), nil
}

// replaceProtoMsg replaces the given target message with the content of the
// replacement message.
func replaceProtoMsg(target interface{}, replacement interface{}) error {
	targetMsg, ok := target.(proto.Message)
	if !ok {
		return fmt.Errorf("target is not a proto message: %v", target)
	}

	replacementMsg, ok := replacement.(proto.Message)
	if !ok {
		return fmt.Errorf("replacement is not a proto message: %v",
			replacement)
	}

	if targetMsg.ProtoReflect().Type() !=
		replacementMsg.ProtoReflect().Type() {

		return fmt.Errorf("replacement message is of wrong type")
	}

	replacementBytes, err := proto.Marshal(replacementMsg)
	if err != nil {
		return fmt.Errorf("error marshaling replacement: %w", err)
	}
	err = proto.Unmarshal(replacementBytes, targetMsg)
	if err != nil {
		return fmt.Errorf("error unmarshaling replacement: %w", err)
	}

	return nil
}
