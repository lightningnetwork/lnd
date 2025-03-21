package rpcwallet

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnutils"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	// ErrRequestTimeout is the error that's returned if we time out while
	// waiting for a response from the remote signer.
	ErrRequestTimeout = errors.New("remote signer response timeout reached")

	// ErrConnectTimeout is the error that's returned if we time out while
	// waiting for the remote signer to connect.
	ErrConnectTimeout = errors.New("timed out when waiting for remote " +
		"signer to connect")

	// ErrMultipleConnections is the error that's returned if another
	// remote signer attempts to connect while we already have one
	// connected.
	ErrMultipleConnections = errors.New("only one remote signer can be " +
		"connected")

	// ErrNotConnected is the error that's returned if the remote signer
	// closes the stream or we encounter an error when receiving over the
	// stream.
	ErrNotConnected = errors.New("the remote signer is no longer connected")

	// ErrUnexpectedResponse is the error that's returned if the response
	// with the expected request ID from the remote signer is of an
	// unexpected type.
	ErrUnexpectedResponse = errors.New("unexpected response type")
)

// SignCoordinator is an implementation of the signrpc.SignerClient and the
// walletrpc.WalletKitClient interfaces that passes on all requests to a remote
// signer. It is used by the watch-only wallet to delegate any signing or ECDH
// operations to a remote node over a
// walletrpc.WalletKit_SignCoordinatorStreamsServer stream. The stream is set up
// by the remote signer when it connects to the watch-only wallet, which should
// execute the Run method.
type SignCoordinator struct {
	// nextRequestID keeps track of the next request ID that should
	// be used when sending a request to the remote signer.
	nextRequestID atomic.Uint64

	// stream is a bi-directional stream between us and the remote signer.
	stream StreamServer

	// responses is a map of request IDs to response channels. This map
	// should be populated with a response channel for each request that has
	// been sent to the remote signer. The response channel should be
	// inserted into the map before the request is sent.
	// Any response received over the stream that does not have an
	// associated response channel in this map is ignored.
	// The response channel should be removed from the map when the response
	// has been received and processed.
	responses *lnutils.SyncMap[uint64, chan *signerResponse]

	// receiveErrChan is used to signal that the stream with the remote
	// signer has errored, and we can no longer process responses.
	receiveErrChan chan error

	// disconnected is closed when either party terminates and signals to
	// any pending requests that we'll no longer process the response for
	// that request.
	disconnected chan struct{}

	// quit is closed when lnd is shutting down.
	quit chan struct{}

	// clientReady is closed and sent over when the remote signer is
	// connected and ready to accept requests (after the initial handshake).
	clientReady chan struct{}

	// clientConnected is true if a remote signer is currently connected.
	clientConnected bool

	// requestTimeout is the maximum time we will wait for a response from
	// the remote signer.
	requestTimeout time.Duration

	// connectionTimeout is the maximum time we will wait for the remote
	// signer to connect.
	connectionTimeout time.Duration

	mu sync.Mutex

	wg sync.WaitGroup
}

// A compile time assertion to ensure SignCoordinator meets the
// RemoteSignerRequests interface.
var _ RemoteSignerRequests = (*SignCoordinator)(nil)

// NewSignCoordinator creates a new instance of the SignCoordinator.
func NewSignCoordinator(requestTimeout time.Duration,
	connectionTimeout time.Duration) *SignCoordinator {

	respsMap := &lnutils.SyncMap[uint64, chan *signerResponse]{}

	s := &SignCoordinator{
		responses:         respsMap,
		receiveErrChan:    make(chan error, 1),
		clientReady:       make(chan struct{}),
		clientConnected:   false,
		quit:              make(chan struct{}),
		requestTimeout:    requestTimeout,
		connectionTimeout: connectionTimeout,
		// Note that the disconnected channel is not initialized here,
		// as no code listens to it until the Run method has been called
		// and set the field.
	}

	// We initialize the atomic nextRequestID to the handshakeRequestID, as
	// requestID 1 is reserved for the initial handshake by the remote
	// signer.
	s.nextRequestID.Store(handshakeRequestID)

	return s
}

// Run starts the SignCoordinator and blocks until the remote signer either
// disconnects, the SignCoordinator is shut down, or an error occurs.
func (s *SignCoordinator) Run(stream StreamServer) error {
	s.mu.Lock()

	select {
	case <-s.quit:
		s.mu.Unlock()
		return ErrShuttingDown

	default:
	}

	if s.clientConnected {
		// If we already have a stream, we error out as we can only have
		// one connection at a time.
		return ErrMultipleConnections
	}

	s.wg.Add(1)
	defer s.wg.Done()

	s.clientConnected = true
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()

		// When `Run` returns, we set the clientConnected field to false
		// to allow a new remote signer connection to be set up.
		s.clientConnected = false
	}()

	s.stream = stream

	s.disconnected = make(chan struct{})
	defer close(s.disconnected)

	s.mu.Unlock()

	// The handshake must be completed before we can start sending requests
	// to the remote signer.
	err := s.handshake(stream)
	if err != nil {
		return err
	}

	log.Infof("Remote signer connected and ready")

	close(s.clientReady)
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()

		// We create a new clientReady channel, once this function
		// has exited, to ensure that a new remote signer connection can
		// be set up.
		s.clientReady = make(chan struct{})
	}()

	// Now let's start the main receiving loop, which will receive all
	// responses to our requests from the remote signer!
	// We start the receiving loop in a goroutine to ensure that this
	// function exits if the SignCoordinator is shut down (i.e. the s.quit
	// channel is closed). Returning from this function will cause the
	// stream to be closed, which in turn will cause the receiving loop to
	// exit.
	s.wg.Add(1)
	go s.StartReceiving()

	select {
	case err := <-s.receiveErrChan:
		return err

	case <-s.quit:
		return ErrShuttingDown
	}
}

// Stop shuts down the SignCoordinator and waits until the main receiving loop
// has exited and all pending requests have been terminated.
func (s *SignCoordinator) Stop() {
	log.Infof("Stopping Sign Coordinator")
	defer log.Debugf("Sign coordinator stopped")

	// We lock the mutex before closing the quit channel to ensure that we
	// can't get a concurrent request into the SignCoordinator while we're
	// stopping it. That will ensure that the s.wg.Wait() call below will
	// always wait for any ongoing requests to finish before we return.
	s.mu.Lock()

	close(s.quit)

	s.mu.Unlock()

	s.wg.Wait()
}

// handshake performs the initial handshake with the remote signer. This must
// be done before any other requests are sent to the remote signer.
func (s *SignCoordinator) handshake(stream StreamServer) error {
	var (
		registerChan     = make(chan *walletrpc.SignerRegistration)
		registerDoneChan = make(chan struct{})
		errChan          = make(chan error)
	)

	// Create a context with a timeout using the context from the stream as
	// the parent context. This ensures that we'll exit if either the stream
	// is closed by the remote signer or if we time out.
	ctxt, cancel := context.WithTimeout(
		stream.Context(), s.requestTimeout,
	)
	defer cancel()

	// Read the first message in a goroutine because the Recv method blocks
	// until the message arrives.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		msg, err := stream.Recv()
		if err != nil {
			select {
			case errChan <- err:
			case <-ctxt.Done():
			}

			return
		}

		if msg.GetRefRequestId() != handshakeRequestID {
			err = fmt.Errorf("initial request ID must be %d, "+
				"but is: %d", handshakeRequestID,
				msg.GetRefRequestId())

			select {
			case errChan <- err:
			case <-ctxt.Done():
			}

			return
		}

		switch req := msg.GetSignResponseType().(type) {
		case *walletrpc.SignCoordinatorResponse_SignerRegistration:
			select {
			case registerChan <- req.SignerRegistration:
			case <-ctxt.Done():
			}

			return

		default:
			err := fmt.Errorf("expected registration message, "+
				"but got: %T", req)

			select {
			case errChan <- err:
			case <-ctxt.Done():
			}

			return
		}
	}()

	// Wait for the initial message to arrive or time out if it takes too
	// long. The initial message must be a registration message from the
	// remote signer.
	select {
	case signerRegistration := <-registerChan:
		// TODO(viktor): This could be extended to validate the version
		// of the remote signer in the future.
		if signerRegistration.GetRegistrationInfo() == "" {
			return errors.New("invalid remote signer " +
				"registration info")
		}

		// Todo(viktor): The RegistrationChallenge in the
		// signerRegistration should likely also be signed here.

	case err := <-errChan:
		return fmt.Errorf("error receiving initial remote signer "+
			"registration message: %v", err)

	case <-s.quit:
		return ErrShuttingDown

	case <-ctxt.Done():
		return ctxt.Err()
	}

	complete := &walletrpc.RegistrationResponse_RegistrationComplete{
		// TODO(viktor): The signature should be generated by signing
		// the RegistrationChallenge contained in the SignerRegistration
		// message in the future.
		// The RegistrationInfo could also be extended to include info
		// about the watch-only node in the future.
		RegistrationComplete: &walletrpc.RegistrationComplete{
			Signature:        "",
			RegistrationInfo: "watch-only registration info",
		},
	}
	// Send a message to the client to indicate that the registration has
	// successfully completed.
	req := &walletrpc.SignCoordinatorRequest_RegistrationResponse{
		RegistrationResponse: &walletrpc.RegistrationResponse{
			RegistrationResponseType: complete,
		},
	}

	regCompleteMsg := &walletrpc.SignCoordinatorRequest{
		RequestId:       handshakeRequestID,
		SignRequestType: req,
	}

	// Send the message in a goroutine because the Send method blocks until
	// the message is read by the client.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		err := stream.Send(regCompleteMsg)
		if err != nil {
			select {
			case errChan <- err:
			case <-ctxt.Done():
			}

			return
		}

		close(registerDoneChan)
	}()

	select {
	case err := <-errChan:
		return fmt.Errorf("error sending registration complete "+
			" message to remote signer: %v", err)

	case <-ctxt.Done():
		return ctxt.Err()

	case <-s.quit:
		return ErrShuttingDown

	case <-registerDoneChan:
	}

	return nil
}

// StartReceiving is the main receive loop that receives responses from the
// remote signer. Responses must have a RequestID that corresponds to requests
// which are waiting for a response; otherwise, the response is ignored.
func (s *SignCoordinator) StartReceiving() {
	defer s.wg.Done()

	for {
		resp, err := s.stream.Recv()
		if err != nil {
			select {
			// If we've already shut down, the main Run method will
			// not be able to receive any error sent over the error
			// channel. So we just return.
			case <-s.quit:

			// Send the error over the error channel, so that the
			// main Run method can return the error.
			case s.receiveErrChan <- err:
			}

			return
		}

		respChan, ok := s.responses.Load(resp.GetRefRequestId())

		if ok {
			select {
			// We should always be able to send over the response
			// channel, as the channel allows for a buffer of 1, and
			// we shouldn't have multiple requests and responses for
			// the same request ID.
			case respChan <- resp:

			case <-s.quit:
				return

			// The timeout case be unreachable, as we should always
			// be able to send 1 response over the response channel.
			// We keep this case just to avoid a scenario where the
			// receive loop would be blocked if we receive multiple
			// responses for the same request ID.
			case <-time.After(s.requestTimeout):
			}
		}

		// If there's no response channel, the thread waiting for the
		// response has most likely timed out. We therefore ignore the
		// response. The other scenario where we don't have a response
		// channel would be if we received a response for a request that
		// we didn't send. This should never happen, but if it does, we
		// ignore the response.

		select {
		case <-s.quit:
			return
		default:
		}
	}
}

// WaitUntilConnected waits until the remote signer has connected. If the remote
// signer does not connect within the configured connection timeout, or if the
// passed context is canceled, an error is returned.
func (s *SignCoordinator) WaitUntilConnected(ctx context.Context) error {
	// As the Run method will redefine the clientReady channel once it
	// returns, we need copy the pointer to the current clientReady channel
	// to ensure that we're waiting for the correct channel, and to avoid
	// a data race.
	s.mu.Lock()
	currentClientReady := s.clientReady
	s.mu.Unlock()

	select {
	case <-currentClientReady:
		return nil

	case <-s.quit:
		return ErrShuttingDown

	case <-ctx.Done():
		return ctx.Err()

	case <-time.After(s.connectionTimeout):
		return ErrConnectTimeout
	}
}

// createResponseChannel creates a response channel for the given request ID and
// inserts it into the responses map. The function returns a cleanup function
// which removes the channel from the responses map, and the caller must ensure
// that this cleanup function is executed once the thread that's waiting for
// the response is done.
func (s *SignCoordinator) createResponseChannel(requestID uint64) func() {
	// Create a new response channel.
	respChan := make(chan *signerResponse, 1)

	// Insert the response channel into the map.
	s.responses.Store(requestID, respChan)

	// Create a cleanup function that will delete the response channel.
	return func() {
		select {
		// If we have timed out, there could be a very unlikely
		// scenario where we did receive a response before we managed to
		// grab the lock in the cleanup func. In that case, we'll just
		// ignore the response. We should still clean up the response
		// channel though.
		case <-respChan:
		default:
		}

		s.responses.Delete(requestID)
	}
}

// getResponse waits for a response with the given request ID and returns the
// response if it is received. If the corresponding response from the remote
// signer is a SignerError, the error message is returned. If the response is
// not received within the given timeout, an error is returned.
//
// Note: Before calling this function, the caller must have created a response
// channel for the request ID.
func (s *SignCoordinator) getResponse(ctx context.Context,
	requestID uint64) (*signerResponse, error) {

	respChan, ok := s.responses.Load(requestID)

	// Verify that we have a response channel for the request ID.
	if !ok {
		// It should be impossible to reach this case, as we create the
		// response channel before sending the request.
		return nil, fmt.Errorf("no response channel found for "+
			"request ID %d", requestID)
	}

	// Wait for the response to arrive.
	select {
	case resp, ok := <-respChan:
		if !ok {
			// If the response channel was closed, we return an
			// error as the receiving thread must have timed out
			// before we managed to grab the response.
			return nil, ErrRequestTimeout
		}

		// a temp type alias to limit the length of the line below.
		type sErr = walletrpc.SignCoordinatorResponse_SignerError

		// If the response is an error, we return the error message.
		if errorResp, ok := resp.GetSignResponseType().(*sErr); ok {
			errStr := errorResp.SignerError.GetError()

			log.Debugf("Received an error response from remote "+
				"signer for request ID %d. Error: %v",
				requestID, errStr)

			return nil, errors.New(errStr)
		}

		log.Debugf("Received remote signer %T response for request "+
			"ID %d", resp.GetSignResponseType(), requestID)

		log.Tracef("Remote signer response content: %v",
			formatSignCoordinatorMsg(resp))

		return resp, nil

	case <-s.disconnected:
		log.Debugf("Stopped waiting for remote signer response for "+
			"request ID %d as the stream has been closed",
			requestID)

		return nil, ErrNotConnected

	case <-s.quit:
		log.Debugf("Stopped waiting for remote signer response for "+
			"request ID %d as we're shutting down", requestID)

		return nil, ErrShuttingDown

	case <-ctx.Done():
		log.Debugf("Context cancelled while waiting for remote signer "+
			"response for request ID %d", requestID)

		return nil, ctx.Err()

	case <-time.After(s.requestTimeout):
		log.Debugf("Remote signer response timed out for request ID %d",
			requestID)

		return nil, ErrRequestTimeout
	}
}

// registerRequest registers a new request with the SignCoordinator, ensuring it
// awaits the handling of the request before shutting down. The function returns
// a Done function that must be executed once the request has been handled.
func (s *SignCoordinator) registerRequest() (func(), error) {
	// We lock the mutex to ensure that we can't have a race where we'd
	// register a request while shutting down.
	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case <-s.quit:
		return nil, ErrShuttingDown
	default:
	}

	s.wg.Add(1)

	return func() {
		s.wg.Done()
	}, nil
}

// Ping sends a ping request to the remote signer and waits for a pong response.
func (s *SignCoordinator) Ping(ctx context.Context,
	timeout time.Duration) (bool, error) {

	req := &walletrpc.SignCoordinatorRequest_Ping{
		Ping: true,
	}

	// As we're pinging, we will time out the request if we don't receive a
	// response within the timeout.
	ctxt, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return processRequest(
		ctxt, s,
		func(reqId uint64) walletrpc.SignCoordinatorRequest {
			return walletrpc.SignCoordinatorRequest{
				RequestId:       reqId,
				SignRequestType: req,
			}
		},
		func(resp *signerResponse) bool {
			return resp.GetPong()
		},
	)
}

// DeriveSharedKey sends a SharedKeyRequest to the remote signer and waits for
// the corresponding response.
//
// NOTE: This is part of the RemoteSignerRequests interface.
func (s *SignCoordinator) DeriveSharedKey(ctx context.Context,
	in *signrpc.SharedKeyRequest,
	_ ...grpc.CallOption) (*signrpc.SharedKeyResponse, error) {

	req := &walletrpc.SignCoordinatorRequest_SharedKeyRequest{
		SharedKeyRequest: in,
	}

	return processRequest(
		ctx, s,
		func(reqId uint64) walletrpc.SignCoordinatorRequest {
			return walletrpc.SignCoordinatorRequest{
				RequestId:       reqId,
				SignRequestType: req,
			}
		},
		func(resp *signerResponse) *signrpc.SharedKeyResponse {
			return resp.GetSharedKeyResponse()
		},
	)
}

// MuSig2Cleanup sends a MuSig2CleanupRequest to the remote signer and waits for
// the corresponding response.
//
// NOTE: This is part of the RemoteSignerRequests interface.
func (s *SignCoordinator) MuSig2Cleanup(ctx context.Context,
	in *signrpc.MuSig2CleanupRequest,
	_ ...grpc.CallOption) (*signrpc.MuSig2CleanupResponse, error) {

	req := &walletrpc.SignCoordinatorRequest_MuSig2CleanupRequest{
		MuSig2CleanupRequest: in,
	}

	return processRequest(
		ctx, s,
		func(reqId uint64) walletrpc.SignCoordinatorRequest {
			return walletrpc.SignCoordinatorRequest{
				RequestId:       reqId,
				SignRequestType: req,
			}
		},
		func(resp *signerResponse) *signrpc.MuSig2CleanupResponse {
			return resp.GetMuSig2CleanupResponse()
		},
	)
}

// MuSig2CombineSig sends a MuSig2CombineSigRequest to the remote signer and
// waits for the corresponding response.
//
// NOTE: This is part of the RemoteSignerRequests interface.
func (s *SignCoordinator) MuSig2CombineSig(ctx context.Context,
	in *signrpc.MuSig2CombineSigRequest,
	_ ...grpc.CallOption) (*signrpc.MuSig2CombineSigResponse, error) {

	req := &walletrpc.SignCoordinatorRequest_MuSig2CombineSigRequest{
		MuSig2CombineSigRequest: in,
	}

	return processRequest(
		ctx, s,
		func(reqId uint64) walletrpc.SignCoordinatorRequest {
			return walletrpc.SignCoordinatorRequest{
				RequestId:       reqId,
				SignRequestType: req,
			}
		},
		func(resp *signerResponse) *signrpc.MuSig2CombineSigResponse {
			return resp.GetMuSig2CombineSigResponse()
		},
	)
}

// MuSig2CreateSession sends a MuSig2SessionRequest to the remote signer and
// waits for the corresponding response.
//
// NOTE: This is part of the RemoteSignerRequests interface.
func (s *SignCoordinator) MuSig2CreateSession(ctx context.Context,
	in *signrpc.MuSig2SessionRequest,
	_ ...grpc.CallOption) (*signrpc.MuSig2SessionResponse, error) {

	req := &walletrpc.SignCoordinatorRequest_MuSig2SessionRequest{
		MuSig2SessionRequest: in,
	}

	return processRequest(
		ctx, s,
		func(reqId uint64) walletrpc.SignCoordinatorRequest {
			return walletrpc.SignCoordinatorRequest{
				RequestId:       reqId,
				SignRequestType: req,
			}
		},
		func(resp *signerResponse) *signrpc.MuSig2SessionResponse {
			return resp.GetMuSig2SessionResponse()
		},
	)
}

// MuSig2RegisterNonces sends a MuSig2RegisterNoncesRequest to the remote signer
// and waits for the corresponding response.
//
// NOTE: This is part of the RemoteSignerRequests interface.
func (s *SignCoordinator) MuSig2RegisterNonces(ctx context.Context,
	in *signrpc.MuSig2RegisterNoncesRequest,
	_ ...grpc.CallOption) (*signrpc.MuSig2RegisterNoncesResponse,
	error) {

	req := &walletrpc.SignCoordinatorRequest_MuSig2RegisterNoncesRequest{
		MuSig2RegisterNoncesRequest: in,
	}

	type muSig2RegisterNoncesResp = *signrpc.MuSig2RegisterNoncesResponse

	return processRequest(
		ctx, s,
		func(reqId uint64) walletrpc.SignCoordinatorRequest {
			return walletrpc.SignCoordinatorRequest{
				RequestId:       reqId,
				SignRequestType: req,
			}
		},
		func(resp *signerResponse) muSig2RegisterNoncesResp {
			return resp.GetMuSig2RegisterNoncesResponse()
		},
	)
}

// MuSig2RegisterCombinedNonce sends a MuSig2RegisterCombinedNonce to
// the remote signer and waits for the corresponding response.
func (s *SignCoordinator) MuSig2RegisterCombinedNonce(ctx context.Context,
	in *signrpc.MuSig2RegisterCombinedNonceRequest,
	_ ...grpc.CallOption) (*signrpc.MuSig2RegisterCombinedNonceResponse,
	error) {

	req := &walletrpc.SignCoordinatorRequest_MuSig2CombinedNoncesReq{
		MuSig2CombinedNoncesReq: in,
	}

	type muSig2RegCombNResp = *signrpc.MuSig2RegisterCombinedNonceResponse

	return processRequest(
		ctx, s,
		func(reqId uint64) walletrpc.SignCoordinatorRequest {
			return walletrpc.SignCoordinatorRequest{
				RequestId:       reqId,
				SignRequestType: req,
			}
		},
		func(resp *signerResponse) muSig2RegCombNResp {
			return resp.GetMuSig2CombNoncesResp()
		},
	)
}

// MuSig2GetCombinedNonce sends a MuSig2GetCombinedNonceRequest to the
// remote signer and waits for the corresponding response.
func (s *SignCoordinator) MuSig2GetCombinedNonce(ctx context.Context,
	in *signrpc.MuSig2GetCombinedNonceRequest,
	_ ...grpc.CallOption) (*signrpc.MuSig2GetCombinedNonceResponse, error) {

	req := &walletrpc.SignCoordinatorRequest_MuSig2GetCombinedNoncesReq{
		MuSig2GetCombinedNoncesReq: in,
	}

	type muSig2GetCombNonceResp = *signrpc.MuSig2GetCombinedNonceResponse

	return processRequest(
		ctx, s,
		func(reqId uint64) walletrpc.SignCoordinatorRequest {
			return walletrpc.SignCoordinatorRequest{
				RequestId:       reqId,
				SignRequestType: req,
			}
		},
		func(resp *signerResponse) muSig2GetCombNonceResp {
			return resp.GetMuSig2GetCombNoncesResp()
		},
	)
}

// MuSig2Sign sends a MuSig2SignRequest to the remote signer and waits for the
// corresponding response.
//
// NOTE: This is part of the RemoteSignerRequests interface.
func (s *SignCoordinator) MuSig2Sign(ctx context.Context,
	in *signrpc.MuSig2SignRequest,
	_ ...grpc.CallOption) (*signrpc.MuSig2SignResponse, error) {

	req := &walletrpc.SignCoordinatorRequest_MuSig2SignRequest{
		MuSig2SignRequest: in,
	}

	return processRequest(
		ctx, s,
		func(reqId uint64) walletrpc.SignCoordinatorRequest {
			return walletrpc.SignCoordinatorRequest{
				RequestId:       reqId,
				SignRequestType: req,
			}
		},
		func(resp *signerResponse) *signrpc.MuSig2SignResponse {
			return resp.GetMuSig2SignResponse()
		},
	)
}

// SignMessage sends a SignMessageReq to the remote signer and waits for the
// corresponding response.
//
// NOTE: This is part of the RemoteSignerRequests interface.
func (s *SignCoordinator) SignMessage(ctx context.Context,
	in *signrpc.SignMessageReq,
	_ ...grpc.CallOption) (*signrpc.SignMessageResp, error) {

	req := &walletrpc.SignCoordinatorRequest_SignMessageReq{
		SignMessageReq: in,
	}

	return processRequest(
		ctx, s,
		func(reqId uint64) walletrpc.SignCoordinatorRequest {
			return walletrpc.SignCoordinatorRequest{
				RequestId:       reqId,
				SignRequestType: req,
			}
		},
		func(resp *signerResponse) *signrpc.SignMessageResp {
			return resp.GetSignMessageResp()
		},
	)
}

// SignPsbt sends a SignPsbtRequest to the remote signer and waits for the
// corresponding response.
//
// NOTE: This is part of the RemoteSignerRequests interface.
func (s *SignCoordinator) SignPsbt(ctx context.Context,
	in *walletrpc.SignPsbtRequest,
	_ ...grpc.CallOption) (*walletrpc.SignPsbtResponse, error) {

	req := &walletrpc.SignCoordinatorRequest_SignPsbtRequest{
		SignPsbtRequest: in,
	}

	return processRequest(
		ctx, s,
		func(reqId uint64) walletrpc.SignCoordinatorRequest {
			return walletrpc.SignCoordinatorRequest{
				RequestId:       reqId,
				SignRequestType: req,
			}
		},
		func(resp *signerResponse) *walletrpc.SignPsbtResponse {
			return resp.GetSignPsbtResponse()
		},
	)
}

// processRequest is a generic function that sends a request to the remote
// signer and waits for the corresponding response. If a timeout is set, the
// function will limit the execution time of the entire function to the
// specified timeout. If it is not set, configured timeouts will be used for
// the individual operations within the function.
func processRequest[R comparable](ctx context.Context, s *SignCoordinator,
	generateRequest func(uint64) walletrpc.SignCoordinatorRequest,
	extractResponse func(*signerResponse) R) (R, error) {

	var zero R

	done, err := s.registerRequest()
	if err != nil {
		return zero, err
	}
	defer done()

	// Wait for the remote signer to connect. If the remote signer doesn't
	// connect within the configured connection timeout, or before the ctx
	// times out, we will return an error.
	err = s.WaitUntilConnected(ctx)

	if err != nil {
		return zero, err
	}

	reqID := s.nextRequestID.Add(1)
	req := generateRequest(reqID)

	cleanUpChannel := s.createResponseChannel(reqID)
	defer cleanUpChannel()

	log.Debugf("Sending a %T to the remote signer with request ID %d",
		req.SignRequestType, reqID)

	log.Tracef("Request content: %v", formatSignCoordinatorMsg(&req))

	// reprocessOnDisconnect is a helper function that will be used to
	// resend the request if the remote signer disconnects, through which
	// we will wait for it to reconnect within the configured timeout, and
	// then resend the request.
	reprocessOnDisconnect := func() (R, error) {
		log.Debugf("Remote signer disconnected while waiting for "+
			"response for request ID %d. Retrying request...",
			reqID)

		return processRequest[R](
			ctx, s, generateRequest, extractResponse,
		)
	}

	err = s.stream.Send(&req)
	if err != nil {
		st, isStatusError := status.FromError(err)
		if isStatusError && st.Code() == codes.Unavailable {
			// If the stream was closed due to the remote signer
			// disconnecting, we will retry to process the request
			// if the remote signer reconnects.
			return reprocessOnDisconnect()
		}

		return zero, err
	}

	var resp *walletrpc.SignCoordinatorResponse

	// Wait for the remote signer response for the given request. We will
	// wait for the configured request timeout, or until the context is
	// cancelled/timed out.
	resp, err = s.getResponse(ctx, reqID)

	if errors.Is(err, ErrNotConnected) {
		// If the remote signer disconnected while we were waiting for
		// the response, we will retry to process the request if the
		// remote signer reconnects.
		return reprocessOnDisconnect()
	} else if err != nil {
		return zero, err
	}

	rpcResp := extractResponse(resp)
	if rpcResp == zero {
		return zero, ErrUnexpectedResponse
	}

	return rpcResp, nil
}
