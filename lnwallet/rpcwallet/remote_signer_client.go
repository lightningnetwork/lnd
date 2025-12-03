package rpcwallet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/reflect/protoreflect"
	"gopkg.in/macaroon.v2"
)

type (
	signerResponse = walletrpc.SignCoordinatorResponse

	// registrationResp is a type alias for the registration response type,
	// created to keep line length within 80 characters.
	registrationResp = walletrpc.SignCoordinatorRequest_RegistrationResponse

	// completeType is a type alias for the registration complete type,
	// created to keep line length within 80 characters.
	completeType = walletrpc.RegistrationResponse_RegistrationComplete
)

// registrationMsg is the message that we send to the watch-only node to
// initiate the handshake process.
// TODO(viktor): This could be extended to include info about the
// version of the remote signer in the future.
// The RegistrationChallenge should also be set to a randomized string.
var registrationMsg = &walletrpc.SignCoordinatorResponse{
	RefRequestId: handshakeRequestID,
	SignResponseType: &walletrpc.SignCoordinatorResponse_SignerRegistration{
		SignerRegistration: &walletrpc.SignerRegistration{
			RegistrationChallenge: "registrationChallenge",
			RegistrationInfo:      "outboundSigner",
		},
	},
}

var (
	// ErrShuttingDown indicates that the server is in the process of
	// gracefully exiting.
	ErrShuttingDown = errors.New("lnd is shutting down")

	// ErrRequestType is returned when the request type by the watch-only
	// node has not been implemented by remote signer.
	ErrRequestType = errors.New("unimplemented request by watch-only node")
)

const (
	// defaultRetryTimeout is the default timeout used when retrying to
	// connect to the watch-only node.
	defaultRetryTimeout = time.Second * 1

	// retryMultiplier is the multiplier used to increase the retry timeout
	// for every retry.
	retryMultiplier = 1.5

	// defaultMaxRetryTimeout is the default max value for the
	// maxRetryTimeout, which defines the maximum backoff period before
	// attempting to reconnect to the watch-only node.
	defaultMaxRetryTimeout = time.Minute * 1

	// handshakeRequestID is the request ID that is reversed for the
	// handshake with the watch-only node.
	handshakeRequestID = uint64(1)
)

// Stream represents the stream to the watch-only node with a Close function
// that closes the connection.
type Stream struct {
	StreamClient

	// Close closes the connection to the watch-only node.
	Close func() error
}

// NewStream creates a new Stream instance.
func NewStream(client StreamClient, closeConn func() error) *Stream {
	return &Stream{
		StreamClient: client,
		Close:        closeConn,
	}
}

// SignCoordinatorStreamFeeder is an interface that returns a newly created
// stream to the watch-only node. The stream is used to send and receive
// messages between the remote signer client and the watch-only node.
type SignCoordinatorStreamFeeder interface {
	// GetStream returns a new stream to the watch-only node. The function
	// also returns a cleanup function that should be called when the stream
	// is no longer needed.
	GetStream(ctx context.Context) (*Stream, error)

	// Stop stops the stream feeder.
	Stop()
}

// RemoteSignerClient is an interface that defines the methods that a remote
// signer client should implement.
type RemoteSignerClient interface {
	// Start starts the remote signer client.
	Start(ctx context.Context) error

	// Stop stops the remote signer client.
	Stop() error

	// MustImplementRemoteSignerClient is a no-op method that makes it
	// easier to filter structs that implement the RemoteSignerClient
	// interface.
	MustImplementRemoteSignerClient()
}

// StreamFeeder is an implementation of the SignCoordinatorStreamFeeder
// interface that creates a new stream to the watch-only node, by making an
// outbound gRPC connection to the watch-only node.
type StreamFeeder struct {
	wg sync.WaitGroup

	cfg lncfg.ConnectionCfg

	cg *fn.ContextGuard
}

// NewStreamFeeder creates a new StreamFeeder instance.
func NewStreamFeeder(cfg lncfg.ConnectionCfg) *StreamFeeder {
	return &StreamFeeder{
		cfg: cfg,
		cg:  fn.NewContextGuard(),
	}
}

// Stop stops the StreamFeeder and disables the StreamFeeder from creating any
// new connections.
//
// NOTE: This is part of the SignCoordinatorStreamFeeder interface.
func (s *StreamFeeder) Stop() {
	s.cg.Quit()

	s.wg.Wait()
}

// GetStream returns a new stream to the watch-only node, by making an
// outbound gRPC connection to the watch-only node. The function also returns a
// cleanup function that closes the connection, which should be called when the
// stream is no longer needed.
//
// NOTE: This is part of the SignCoordinatorStreamFeeder interface.
func (s *StreamFeeder) GetStream(ctx context.Context) (*Stream, error) {
	select {
	// Don't run if the StreamFeeder has already been shutdown.
	case <-s.cg.Done():
		return nil, ErrShuttingDown
	default:
	}

	// Create a new outbound gRPC connection to the watch-only node.
	conn, err := s.getClientConn(ctx)
	if err != nil {
		return nil, err
	}

	// Wrap the connection in a WalletKitClient.
	walletKitClient := walletrpc.NewWalletKitClient(conn)

	// Create a new stream to the watch-only node.
	streamClient, err := walletKitClient.SignCoordinatorStreams(ctx)
	if err != nil {
		connErr := conn.Close()
		if connErr != nil {
			log.ErrorS(ctx, "Unable to close watch-only node "+
				"connection: %v", connErr)
		}

		return nil, err
	}

	return NewStream(streamClient, conn.Close), nil
}

// getClientConn creates a new outbound gRPC connection to the watch-only node.
func (s *StreamFeeder) getClientConn(
	ctx context.Context) (*grpc.ClientConn, error) {

	// Ensure that our top level ctx is derived from the context guard.
	// That way we know that we only need to select on the context guard's
	// Done channel if the remote signer client is shutting down.
	// If we fail to connect to the watch-only node within the
	// configured timeout we should return an error.
	ctx, cancel := s.cg.Create(ctx, fn.WithCustomTimeoutCG(s.cfg.Timeout))
	defer cancel()

	// Load the specified macaroon file for the watch-only node.
	macBytes, err := os.ReadFile(s.cfg.MacaroonPath)
	if err != nil {
		return nil, fmt.Errorf("could not read macaroon file: %w", err)
	}

	mac := &macaroon.Macaroon{}

	err = mac.UnmarshalBinary(macBytes)
	if err != nil {
		return nil, fmt.Errorf("could not unmarshal macaroon: %w", err)
	}

	macCred, err := macaroons.NewMacaroonCredential(mac)
	if err != nil {
		return nil, fmt.Errorf(
			"could not create macaroon credential: %w", err)
	}

	// Load the specified TLS cert for the watch-only node.
	tlsCreds, err := credentials.NewClientTLSFromFile(s.cfg.TLSCertPath, "")
	if err != nil {
		return nil, fmt.Errorf("could not load TLS cert: %w", err)
	}

	opts := []grpc.DialOption{
		grpc.WithBlock(),
		grpc.WithTransportCredentials(tlsCreds),
		grpc.WithPerRPCCredentials(macCred),
	}

	log.InfoS(ctx, "Attempting to connect to the watch-only node on: %s",
		s.cfg.RPCHost)

	// Connect to the watch-only node using the new context.
	return grpc.DialContext(ctx, s.cfg.RPCHost, opts...)
}

// A compile time assertion to ensure StreamFeeder meets the
// SignCoordinatorStreamFeeder interface.
var _ SignCoordinatorStreamFeeder = (*StreamFeeder)(nil)

// NoOpClient is a remote signer client that is a no op, and is used when the
// configuration doesn't enable the use of a remote signer client.
type NoOpClient struct{}

// Start is a no-op.
//
// NOTE: Part of the RemoteSignerClient interface.
func (n *NoOpClient) Start(ctx context.Context) error {
	return nil
}

// Stop is a no-op.
//
// NOTE: Part of the RemoteSignerClient interface.
func (n *NoOpClient) Stop() error {
	return nil
}

// MustImplementRemoteSignerClient is a no-op.
//
// NOTE: Part of the RemoteSignerClient interface.
func (n *NoOpClient) MustImplementRemoteSignerClient() {}

// A compile time assertion to ensure NoOpClient meets the
// RemoteSignerClient interface.
var _ RemoteSignerClient = (*NoOpClient)(nil)

// OutboundClient is a remote signer client which will process and respond to
// sign requests from the watch-only node, which are sent over a stream between
// the node and a watch-only node.
type OutboundClient struct {
	stopped atomic.Bool

	log btclog.Logger

	// walletServer is the WalletKitServer that the remote signer client
	// will use to process walletrpc requests.
	walletServer walletrpc.WalletKitServer

	// signerServer is the SignerServer that the remote signer client will
	// use to process signrpc requests.
	signerServer signrpc.SignerServer

	// streamFeeder is the stream feeder that will set up a stream to the
	// watch-only node when requested to do so by the remote signer client.
	streamFeeder SignCoordinatorStreamFeeder

	// requestTimeout is the timeout used when sending responses to the
	// watch-only node.
	requestTimeout time.Duration

	// retryTimeout is the backoff timeout used when retrying to set up a
	// connection to the watch-only node, if the previous connection/attempt
	// failed.
	retryTimeout time.Duration

	// maxRetryTimeout is the max value for the retryTimeout, defining
	// the maximum backoff period before attempting to reconnect to the
	// watch-only node.
	maxRetryTimeout time.Duration

	cg       *fn.ContextGuard
	gManager *fn.GoroutineManager
}

// NewOutboundClient creates a new instance of the remote signer client.
// The passed subServers need to include a walletrpc.WalletKitServer and a
// signrpc.SignerServer, or the OutboundClient will be disabled.
// Note that the client will only fully start if the configuration
// enables an outbound remote signer.
func NewOutboundClient(walletServer walletrpc.WalletKitServer,
	signerServer signrpc.SignerServer,
	streamFeeder SignCoordinatorStreamFeeder,
	requestTimeout time.Duration) (*OutboundClient, error) {

	if walletServer == nil || signerServer == nil {
		return nil, errors.New("sub-servers cannot be nil when using " +
			"an outbound remote signer")
	}

	if streamFeeder == nil {
		return nil, errors.New("streamFeeder cannot be nil")
	}

	return &OutboundClient{
		log:             log.WithPrefix("Remote signer client: "),
		walletServer:    walletServer,
		signerServer:    signerServer,
		streamFeeder:    streamFeeder,
		requestTimeout:  requestTimeout,
		retryTimeout:    defaultRetryTimeout,
		maxRetryTimeout: defaultMaxRetryTimeout,
		cg:              fn.NewContextGuard(),
		gManager:        fn.NewGoroutineManager(),
	}, nil
}

// Start starts the remote signer client. The function will continuously try to
// set up a connection to the configured watch-only node, and retry to connect
// if the connection fails until we Stop the remote signer client.
//
// NOTE: Part of the RemoteSignerClient interface.
func (r *OutboundClient) Start(ctx context.Context) error {
	// Ensure that our top level ctx is derived from the context guard.
	// That way we know that we only need to select on the context guard's
	// Done channel if the remote signer client is shutting down.
	ctx, _ = r.cg.Create(ctx)

	success := r.gManager.Go(ctx, r.runForever)
	if !success {
		return errors.New("failed to start remote signer client")
	}

	return nil
}

// runForever continuously tries to set up a connection to the watch-only node,
// and retry to connect if the connection fails until we Stop the remote
// signer client.
func (r *OutboundClient) runForever(ctx context.Context) {
	for {
		// Check if we are shutting down.
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := r.runOnce(ctx)
		if err != nil {
			r.log.ErrorS(ctx, "runOnce error", err)
		}

		r.log.InfoS(
			ctx,
			"Connection retry to watch-only node scheduled",
			"retry_after", r.retryTimeout,
		)

		// Backoff before retrying to connect to the watch-only node.
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.retryTimeout):
		}

		r.log.InfoS(ctx, "Retrying to connect to watch-only node")

		// Increase the retry timeout by 50% for every retry.
		r.retryTimeout = time.Duration(
			float64(r.retryTimeout) * retryMultiplier,
		)

		// But cap the retryTimeout at r.maxRetryTimeout
		if r.retryTimeout > r.maxRetryTimeout {
			r.retryTimeout = r.maxRetryTimeout
		}
	}
}

// Stop stops the remote signer client.
//
// NOTE: Part of the RemoteSignerClient interface.
func (r *OutboundClient) Stop() error {
	if r.stopped.Swap(true) {
		return errors.New("remote signer client is already shut down")
	}

	r.log.Info("Shutting down")

	r.cg.Quit()

	r.streamFeeder.Stop()

	r.gManager.Stop()

	r.log.Debugf("Shutdown complete")

	return nil
}

// MustImplementRemoteSignerClient is a no-op.
//
// NOTE: Part of the RemoteSignerClient interface.
func (r *OutboundClient) MustImplementRemoteSignerClient() {}

// runOnce creates a new stream to the watch-only node, and starts processing
// and responding to the sign requests that are sent over the stream. The
// function will continuously run until the remote signer client is either
// stopped or the stream errors.
func (r *OutboundClient) runOnce(ctx context.Context) error {
	// Derive a context for the lifetime of the stream.
	ctx, cancel := r.cg.Create(ctx)

	// Cancel the stream context whenever we return from this function.
	defer cancel()

	log.InfoS(ctx, "Attempting to setup the watch-only node connection")

	// Try to get a new stream to the watch-only node.
	stream, err := r.streamFeeder.GetStream(ctx)
	if err != nil {
		return err
	}
	defer func() {
		err := stream.Close()
		if err != nil {
			log.ErrorS(ctx, "Unable to close watch-only node "+
				"connection", err)
		}
	}()

	// Once the stream has been created, we'll need to perform the handshake
	// process with the watch-only node, before it will start sending us
	// requests.
	err = r.handshake(ctx, stream)
	if err != nil {
		return err
	}

	log.InfoS(ctx, "Completed setup connection to watch-only node")

	// Reset the retry timeout after a successful connection.
	r.retryTimeout = defaultRetryTimeout

	return r.processSignRequestsForever(ctx, stream)
}

// handshake performs the handshake process with the watch-only node. As we are
// the initiator of the stream, we need to send the first message over the
// stream. The watch-only node will only proceed to sending us requests after
// the handshake has been completed.
func (r *OutboundClient) handshake(ctx context.Context, stream *Stream) error {
	// Derive a context that times out the handshake process, if it takes
	// longer than the request timeout.
	ctxt, cancel := context.WithTimeout(ctx, r.requestTimeout)
	defer cancel()

	var (
		msg     *walletrpc.SignCoordinatorRequest
		errChan = make(chan error, 1)
	)

	ok := r.gManager.Go(ctxt, func(_ context.Context) {
		// Send the registration message to the watch-only node.
		err := stream.Send(registrationMsg)
		if err != nil {
			errChan <- err
			return
		}

		// After the registration message has been sent, the signer node
		// will respond with a message indicating that it has accepted
		// the signer registration request if the registration was
		// successful.
		msg, err = stream.Recv()
		errChan <- err
	})
	if !ok {
		return fmt.Errorf("error sending registration message")
	}

	// Wait for the response.
	select {
	case <-ctxt.Done():
		return ctxt.Err()
	case err := <-errChan:
		if err != nil {
			return fmt.Errorf("handshake error: %w", err)
		}
	}

	// Verify that the request ID of the response is the same as the
	// request ID of the registration message.
	if msg.GetRequestId() != handshakeRequestID {
		return fmt.Errorf("initial response request id must "+
			"be %d, but is: %d", handshakeRequestID,
			msg.GetRequestId())
	}

	// Check the type of the response message.
	resp, ok := msg.GetSignRequestType().(*registrationResp)
	if !ok {
		return fmt.Errorf("expected registration response, but got: %T",
			msg.GetSignRequestType())
	}

	switch rType := resp.RegistrationResponse.
		GetRegistrationResponseType().(type) {
	// The registration was successful.
	case *completeType:
		// TODO(viktor): This should verify that the signature in the
		// complete message is valid.
		return nil

	// An error occurred during the registration process.
	case *walletrpc.RegistrationResponse_RegistrationError:
		return fmt.Errorf("registration error: %s",
			rType.RegistrationError)

	default:
		return fmt.Errorf("unknown registration response type: %T",
			resp.RegistrationResponse.GetRegistrationResponseType())
	}
}

// processSignRequestsForever processes and responds to the sign requests tha
// are sent over the stream. The function will continuously run until the
// remote signer client is either stopped or the stream errors.
func (r *OutboundClient) processSignRequestsForever(ctx context.Context,
	stream *Stream) error {

	for {
		err := r.processSingleSignReq(ctx, stream)
		if err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ErrShuttingDown
		default:
		}
	}
}

// processSingleSignReq waits for and processes a single request from the
// watch-only node, and sends the corresponding response back.
func (r *OutboundClient) processSingleSignReq(ctx context.Context,
	stream *Stream) error {

	// Wait for a request from the watch-only node.
	req, err := r.waitForRequest(ctx, stream)
	if err != nil {
		return err
	}

	// Process the received request.
	resp := r.formResponse(ctx, req)

	// Send the response back to the watch-only node.
	return r.sendResponse(ctx, resp, stream)
}

// waitForRequest waits for a request from the watch-only node.
func (r *OutboundClient) waitForRequest(ctx context.Context, stream *Stream) (
	*walletrpc.SignCoordinatorRequest, error) {

	var (
		req     *walletrpc.SignCoordinatorRequest
		err     error
		errChan = make(chan error, 1)
	)

	// We run the stream.Recv() in a goroutine to ensure we can stop if the
	// remote signer client is shutting down (i.e. the quit channel is
	// closed). Shutting down the remote signer client will cancel the ctx,
	// which will cancel the stream context, which in turn will stop the
	// goroutine.
	ok := r.gManager.Go(ctx, func(_ context.Context) {
		req, err = stream.Recv()
		errChan <- err
	})
	if !ok {
		return nil, fmt.Errorf("error receiving request")
	}

	// Wait for the response and then handle it.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-errChan:
		if err != nil {
			return nil, fmt.Errorf("error receiving request: %w",
				err)
		}
	}

	return req, nil
}

// formResponse processes the received request from the watch-only node, and
// sends the corresponding response back.
func (r *OutboundClient) formResponse(ctx context.Context,
	req *walletrpc.SignCoordinatorRequest) *signerResponse {

	resp, err := r.process(ctx, req)
	if err != nil {
		r.log.ErrorS(ctx, "could not process request", err)

		// If we fail to process the request, we will send a SignerError
		// back to the watch-only node, indicating the nature of the
		// error.
		eType := &walletrpc.SignCoordinatorResponse_SignerError{
			SignerError: &walletrpc.SignerError{
				Error: "error processing the request in the " +
					"remote signer: " + err.Error(),
			},
		}

		resp = &signerResponse{
			RefRequestId:     req.GetRequestId(),
			SignResponseType: eType,
		}
	}

	return resp
}

// process sends the passed request on to the appropriate server for processing
// it, and returns the response.
func (r *OutboundClient) process(ctx context.Context,
	req *walletrpc.SignCoordinatorRequest) (*signerResponse, error) {

	r.log.DebugS(ctx, "Processing a request from watch-only",
		btclog.Fmt("request_type", "%T", req.GetSignRequestType()))

	r.log.TraceS(ctx, "Request content",
		"content", formatSignCoordinatorMsg(req))

	var (
		requestID = req.GetRequestId()
		signResp  = &signerResponse{
			RefRequestId: requestID,
		}
	)

	//nolint:ll
	switch reqType := req.GetSignRequestType().(type) {
	case *walletrpc.SignCoordinatorRequest_SharedKeyRequest:
		resp, err := r.signerServer.DeriveSharedKey(
			ctx, reqType.SharedKeyRequest,
		)
		if err != nil {
			return nil, err
		}

		rType := &walletrpc.SignCoordinatorResponse_SharedKeyResponse{
			SharedKeyResponse: resp,
		}

		signResp.SignResponseType = rType

		return signResp, nil

	case *walletrpc.SignCoordinatorRequest_SignMessageReq:
		resp, err := r.signerServer.SignMessage(
			ctx, reqType.SignMessageReq,
		)
		if err != nil {
			return nil, err
		}

		rType := &walletrpc.SignCoordinatorResponse_SignMessageResp{
			SignMessageResp: resp,
		}

		signResp.SignResponseType = rType

		return signResp, nil

	case *walletrpc.SignCoordinatorRequest_MuSig2SessionRequest:
		resp, err := r.signerServer.MuSig2CreateSession(
			ctx, reqType.MuSig2SessionRequest,
		)
		if err != nil {
			return nil, err
		}

		rType := &walletrpc.SignCoordinatorResponse_MuSig2SessionResponse{
			MuSig2SessionResponse: resp,
		}

		signResp.SignResponseType = rType

		return signResp, nil

	case *walletrpc.SignCoordinatorRequest_MuSig2RegisterNoncesRequest:
		resp, err := r.signerServer.MuSig2RegisterNonces(
			ctx, reqType.MuSig2RegisterNoncesRequest,
		)
		if err != nil {
			return nil, err
		}

		rType := &walletrpc.SignCoordinatorResponse_MuSig2RegisterNoncesResponse{
			MuSig2RegisterNoncesResponse: resp,
		}

		signResp.SignResponseType = rType

		return signResp, nil

	case *walletrpc.SignCoordinatorRequest_MuSig2SignRequest:
		resp, err := r.signerServer.MuSig2Sign(
			ctx, reqType.MuSig2SignRequest,
		)
		if err != nil {
			return nil, err
		}

		rType := &walletrpc.SignCoordinatorResponse_MuSig2SignResponse{
			MuSig2SignResponse: resp,
		}

		signResp.SignResponseType = rType

		return signResp, nil

	case *walletrpc.SignCoordinatorRequest_MuSig2CombineSigRequest:
		resp, err := r.signerServer.MuSig2CombineSig(
			ctx, reqType.MuSig2CombineSigRequest,
		)
		if err != nil {
			return nil, err
		}

		rType := &walletrpc.SignCoordinatorResponse_MuSig2CombineSigResponse{
			MuSig2CombineSigResponse: resp,
		}

		signResp.SignResponseType = rType

		return signResp, nil

	case *walletrpc.SignCoordinatorRequest_MuSig2CleanupRequest:
		resp, err := r.signerServer.MuSig2Cleanup(
			ctx, reqType.MuSig2CleanupRequest,
		)
		if err != nil {
			return nil, err
		}

		rType := &walletrpc.SignCoordinatorResponse_MuSig2CleanupResponse{
			MuSig2CleanupResponse: resp,
		}

		signResp.SignResponseType = rType

		return signResp, nil

	case *walletrpc.SignCoordinatorRequest_SignPsbtRequest:
		resp, err := r.walletServer.SignPsbt(
			ctx, reqType.SignPsbtRequest,
		)
		if err != nil {
			return nil, err
		}

		rType := &walletrpc.SignCoordinatorResponse_SignPsbtResponse{
			SignPsbtResponse: resp,
		}

		signResp.SignResponseType = rType

		return signResp, nil

	case *walletrpc.SignCoordinatorRequest_Ping:
		// If the received request is a ping, we don't need to pass the
		// request on to a server, but can respond with a pong directly.
		rType := &walletrpc.SignCoordinatorResponse_Pong{
			Pong: true,
		}

		signResp.SignResponseType = rType

		return signResp, nil

	default:
		return nil, ErrRequestType
	}
}

// sendResponse sends the passed response back to the watch-only node over the
// stream.
func (r *OutboundClient) sendResponse(ctx context.Context, resp *signerResponse,
	stream *Stream) error {

	// Timeout sending the response if it takes too long.
	ctxt, cancel := r.cg.Create(
		ctx, fn.WithCustomTimeoutCG(r.requestTimeout),
	)
	defer cancel()

	var errChan = make(chan error, 1)

	// We send the response in a goroutine to ensure we can return an error
	// if the send times out or we shut down. This is done to ensure that
	// this function won't block indefinitely.
	ok := r.gManager.Go(ctxt, func(ctxt context.Context) {
		errChan <- stream.Send(resp)
	})
	if !ok {
		return fmt.Errorf("error sending response")
	}

	select {
	case <-ctxt.Done():
		return ctxt.Err()

	case err := <-errChan:
		if err != nil {
			return fmt.Errorf("error sending response: %w", err)
		}
	}

	r.log.TraceS(ctxt, "Sent response to watch-only node",
		btclog.ClosureAttr("response", formatSignCoordinatorMsg(resp)))

	return nil
}

// A compile time assertion to ensure OutboundClient meets the
// RemoteSignerClient interface.
var _ RemoteSignerClient = (*OutboundClient)(nil)

// formatSignCoordinatorMsg formats the passed proto message into a JSON string.
func formatSignCoordinatorMsg(msg protoreflect.ProtoMessage) btclog.Closure {
	return func() string {
		jsonBytes, err := lnrpc.ProtoJSONMarshalOpts.Marshal(msg)
		if err != nil {
			return fmt.Sprintf("<err: %v>", err.Error())
		}

		return string(jsonBytes)
	}
}
