package rpcwallet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
)

type signerResponse = walletrpc.SignCoordinatorResponse

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

	// handshakeRequestID is the request ID that is reversed for the
	// handshake with the watch-only node.
	handshakeRequestID = uint64(1)
)

// SignCoordinatorStreamFeeder is an interface that returns a newly created
// stream to the watch-only node. The stream is used to send and receive
// messages between the remote signer client and the watch-only node.
type SignCoordinatorStreamFeeder interface {
	// GetStream returns a new stream to the watch-only node. The function
	// also returns a cleanup function that should be called when the stream
	// is no longer needed.
	GetStream(streamCtx context.Context) (StreamClient, func(), error)

	// Stop stops the stream feeder.
	Stop()
}

// RemoteSignerClient is an interface that defines the methods that a remote
// signer client should implement.
type RemoteSignerClient interface {
	// Start starts the remote signer client.
	Start() error

	// Stop stops the remote signer client.
	Stop() error
}

// StreamFeeder is an implementation of the SignCoordinatorStreamFeeder
// interface that creates a new stream to the watch-only node, by making an
// outbound gRPC connection to the watch-only node.
type StreamFeeder struct {
	wg sync.WaitGroup

	rpcHost, macaroonPath, tlsCertPath string

	timeout time.Duration

	quit chan struct{}
}

// NewStreamFeeder creates a new StreamFeeder instance.
func NewStreamFeeder(rpcHost, macaroonPath, tlsCertPath string,
	timeout time.Duration) *StreamFeeder {

	return &StreamFeeder{
		quit:         make(chan struct{}),
		rpcHost:      rpcHost,
		macaroonPath: macaroonPath,
		tlsCertPath:  tlsCertPath,
		timeout:      timeout,
	}
}

// Stop stops the StreamFeeder and disables the StreamFeeder from creating any
// new connections.
//
// NOTE: This is part of the SignCoordinatorStreamFeeder interface.
func (s *StreamFeeder) Stop() {
	close(s.quit)

	s.wg.Wait()
}

// GetStream returns a new stream to the watch-only node, by making an
// outbound gRPC connection to the watch-only node. The function also returns a
// cleanup function that closes the connection, which should be called when the
// stream is no longer needed.
//
// NOTE: This is part of the SignCoordinatorStreamFeeder interface.
func (s *StreamFeeder) GetStream(streamCtx context.Context) (
	StreamClient, func(), error) {

	// Create a new outbound gRPC connection to the watch-only node.
	conn, err := s.getClientConn()
	if err != nil {
		return nil, nil, err
	}

	cleanUp := func() {
		conn.Close()
	}

	// Wrap the connection in a WalletKitClient.
	walletKitClient := walletrpc.NewWalletKitClient(conn)

	// Create a new stream to the watch-only node.
	stream, err := walletKitClient.SignCoordinatorStreams(streamCtx)
	if err != nil {
		cleanUp()

		return nil, nil, err
	}

	return stream, cleanUp, nil
}

// getClientConn creates a new outbound gRPC connection to the watch-only node.
func (s *StreamFeeder) getClientConn() (*grpc.ClientConn, error) {
	// If we fail to connect to the watch-only node within the
	// configured timeout we should return an error.
	ctxt, cancel := context.WithTimeout(
		context.Background(), s.timeout,
	)
	defer cancel()

	// Load the specified macaroon file for the watch-only node.
	macBytes, err := os.ReadFile(s.macaroonPath)
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
	tlsCreds, err := credentials.NewClientTLSFromFile(s.tlsCertPath, "")
	if err != nil {
		return nil, fmt.Errorf("could not load TLS cert: %w", err)
	}

	opts := []grpc.DialOption{
		grpc.WithBlock(),
		grpc.WithTransportCredentials(tlsCreds),
		grpc.WithPerRPCCredentials(macCred),
	}

	var (
		// A channel to signal when has successfully been created.
		connDoneChan = make(chan *grpc.ClientConn)
		errChan      = make(chan error)
	)

	// Now let's try to connect to the watch-only node. We'll do this in a
	// goroutine to ensure we can exit if the quit channel is closed. If the
	// quit channel is closed, the context will also be canceled, hence
	// stopping the goroutine.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		conn, err := grpc.DialContext(ctxt, s.rpcHost, opts...)
		if err != nil {
			select {
			case errChan <- fmt.Errorf("could not connect to "+
				"watch-only node: %v", err):

			case <-ctxt.Done():
			}
		}

		// Only send the connection if the getClientConn function hasn't
		// returned yet.
		select {
		case <-ctxt.Done():
			return

		case connDoneChan <- conn:
		}
	}()

	select {
	case conn := <-connDoneChan:
		return conn, nil

	case err := <-errChan:
		return nil, err

	case <-s.quit:
		return nil, ErrShuttingDown

	case <-ctxt.Done():
		return nil, ctxt.Err()
	}
}

// A compile time assertion to ensure StreamFeeder meets the
// SignCoordinatorStreamFeeder interface.
var _ SignCoordinatorStreamFeeder = (*StreamFeeder)(nil)

// NoOpClient is a remote signer client that is a no op, and is used when the
// configuration doesn't enable the use of a remote signer client.
type NoOpClient struct{}

// Start implements RemoteSignerClient, and is a no op.
func (n *NoOpClient) Start() error {
	return nil
}

// Stop implements RemoteSignerClient, and is a no op.
func (n *NoOpClient) Stop() error {
	return nil
}

// A compile time assertion to ensure NoOpClient meets the
// RemoteSignerClient interface.
var _ RemoteSignerClient = (*NoOpClient)(nil)

// OutboundClient is a remote signer client which will process and respond to
// sign requests from the watch-only node, which are sent over a stream between
// the node and a watch-only node.
type OutboundClient struct {
	// walletServer is the WalletKitServer that the remote signer client
	// will use to process walletrpc requests.
	walletServer walletrpc.WalletKitServer

	// signerServer is the SignerServer that the remote signer client will
	// use to process signrpc requests.
	signerServer signrpc.SignerServer

	// streamFeeder is the stream feeder that will set up a stream to the
	// watch-only node when requested to do so by the remote signer client.
	streamFeeder SignCoordinatorStreamFeeder

	// stream is the stream between the node and the watch-only node.
	stream StreamClient

	// requestTimeout is the timeout used when sending responses to the
	// watch-only node.
	requestTimeout time.Duration

	// retryTimeout is the backoff timeout used when retrying to set up a
	// connection to the watch-only node, if the previous connection/attempt
	// failed.
	retryTimeout time.Duration

	stopped int32 // To be used atomically.

	quit chan struct{}

	wg sync.WaitGroup

	// wgMu ensures that we can't spawn a new Run goroutine after we've
	// stopped the remote signer client.
	wgMu sync.Mutex
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
		walletServer:   walletServer,
		signerServer:   signerServer,
		streamFeeder:   streamFeeder,
		requestTimeout: requestTimeout,
		quit:           make(chan struct{}),
		retryTimeout:   defaultRetryTimeout,
	}, nil
}

// Start starts the remote signer client. The function will continuously try to
// setup a connection to the configured watch-only node, and retry to connect if
// the connection fails until we Stop the remote signer client.
func (r *OutboundClient) Start() error {
	r.wg.Add(1)

	// We'll continuously try setup a connection to the watch-only node, and
	// retry to connect if the connection fails until we Stop the remote
	// signer client.
	go func() {
		defer r.wg.Done()

		for {
			err := r.run()
			log.Errorf("Remote signer client error: %v", err)

			select {
			case <-r.quit:
				return
			default:
				log.Infof("Will retry to connect to "+
					"watch-only node in: %v",
					r.retryTimeout)

				// Backoff before retrying to connect to the
				// watch-only node.
				select {
				case <-r.quit:
					return
				case <-time.After(r.retryTimeout):
				}
			}

			log.Infof("Retrying to connect to watch-only node")

			// Increase the retry timeout by 50% for every retry.
			r.retryTimeout = time.Duration(float64(r.retryTimeout) *
				retryMultiplier)
		}
	}()

	return nil
}

// Stop stops the remote signer client.
func (r *OutboundClient) Stop() error {
	if !atomic.CompareAndSwapInt32(&r.stopped, 0, 1) {
		return errors.New("remote signer client is already shut down")
	}

	log.Info("Remote signer client shutting down")

	// Ensure that no new Run goroutines can start when we've initiated
	// the stopping of the remote signer client.
	r.wgMu.Lock()

	if r.streamFeeder != nil {
		r.streamFeeder.Stop()
	}

	close(r.quit)

	r.wgMu.Unlock()

	r.wg.Wait()

	log.Debugf("Remote signer client shut down")

	return nil
}

// run creates a new stream to the watch-only node, and starts processing and
// responding to the sign requests that are sent over the stream. The function
// will continuously run until the remote signer client is either stopped or
// the stream errors.
func (r *OutboundClient) run() error {
	r.wgMu.Lock()

	select {
	case <-r.quit:
		r.wgMu.Unlock()
		return ErrShuttingDown
	default:
	}

	r.wgMu.Unlock()

	streamCtx, cancel := context.WithCancel(context.Background())

	// Cancel the stream context whenever we return from this function.
	defer cancel()

	log.Infof("Attempting to setup the watch-only node connection")

	// Try to get a new stream to the watch-only node.
	stream, streamCleanUp, err := r.streamFeeder.GetStream(streamCtx)
	if err != nil {
		return err
	}

	r.stream = stream
	defer streamCleanUp()

	// Once the stream has been created, we'll need to perform the handshake
	// process with the watch-only node, before it will start sending us
	// requests.
	err = r.handshake(streamCtx)
	if err != nil {
		return err
	}

	log.Infof("Completed setup connection to watch-only node")

	// Reset the retry timeout after a successful connection.
	r.retryTimeout = defaultRetryTimeout

	return r.processSignRequests(streamCtx)
}

// handshake performs the handshake process with the watch-only node. As we are
// the initiator of the stream, we need to send the first message over the
// stream. The watch-only node will only proceed to sending us requests after
// the handshake has been completed.
func (r *OutboundClient) handshake(streamCtx context.Context) error {
	var (
		regSentChan = make(chan struct{})
		regDoneChan = make(chan *walletrpc.SignCoordinatorRequest)
		errChan     = make(chan error)

		// The returnedChan is used to signal that this function has
		// already returned, so that the goroutines don't remain blocked
		// indefinitely when trying to send to the above channels.
		returnedChan = make(chan struct{})
	)
	defer close(returnedChan)

	// TODO(viktor): This could be extended to include info about the
	// version of the remote signer in the future.
	regType := &walletrpc.SignCoordinatorResponse_SignerRegistration{
		SignerRegistration: "outboundSigner",
	}

	registrationMsg := &walletrpc.SignCoordinatorResponse{
		RefRequestId:     handshakeRequestID,
		SignResponseType: regType,
	}

	// Send the registration message to the watch-only node.
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()

		err := r.stream.Send(registrationMsg)
		if err != nil {
			select {
			case errChan <- err:
			case <-returnedChan:
			}

			return
		}

		close(regSentChan)
	}()

	select {
	case err := <-errChan:
		return fmt.Errorf("error sending registration complete "+
			"message to remote signer: %w", err)

	case <-streamCtx.Done():
		return streamCtx.Err()

	case <-r.quit:
		return ErrShuttingDown

	case <-time.After(r.requestTimeout):
		return errors.New("watch-only node handshake timeout")

	case <-regSentChan:
	}

	// After the registration message has been sent, the signer node will
	// respond with a message indicating that it has accepted the signer
	// registration request if the registration was successful.
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()

		msg, err := r.stream.Recv()
		if err != nil {
			select {
			case errChan <- err:
			case <-returnedChan:
			}

			return
		}

		select {
		case regDoneChan <- msg:
		case <-returnedChan:
		}
	}()

	// Wait for the watch-only node to respond that it has accepted the
	// signer has registered.
	var regComplete *walletrpc.SignCoordinatorRequest
	select {
	case regComplete = <-regDoneChan:
		if regComplete.GetRequestId() != handshakeRequestID {
			return fmt.Errorf("initial response request id must "+
				"be 1, but is: %d", regComplete.GetRequestId())
		}

		complete := regComplete.GetRegistrationComplete()
		if !complete {
			return errors.New("invalid initial watch-only node " +
				"registration complete message")
		}

	case err := <-errChan:
		return fmt.Errorf("watch-only node handshake error: %w", err)

	case <-r.quit:
		return ErrShuttingDown

	case <-streamCtx.Done():
		return streamCtx.Err()

	case <-time.After(r.requestTimeout):
		return errors.New("watch-only node handshake timeout")
	}

	return nil
}

// processSignRequests processes and responds to the sign requests that are
// sent over the stream. The function will continuously run until the remote
// signer client is either stopped or the stream errors.
func (r *OutboundClient) processSignRequests(streamCtx context.Context) error {
	var (
		reqChan = make(chan *walletrpc.SignCoordinatorRequest)
		errChan = make(chan error)
	)

	// We run the receive loop in a goroutine to ensure we can stop if the
	// remote signer client is shutting down (i.e. the quit channel is
	// closed). Closing the quit channel will make the processSignRequests
	// function return, which will cancel the stream context, which in turn
	// will stop the receive goroutine.
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()

		for {
			req, err := r.stream.Recv()
			if err != nil {
				wrappedErr := fmt.Errorf("error receiving "+
					"request from watch-only node: %w", err)

				// Send the error to the error channel, given
				// that we're still listening on the channel.
				select {
				case errChan <- wrappedErr:
				case <-streamCtx.Done():
				case <-r.quit:
				}

				return
			}

			select {
			case <-streamCtx.Done():
				return

			case <-r.quit:
				return

			case reqChan <- req:
			}
		}
	}()

	for {
		log.Tracef("Waiting for a request from the watch-only node")

		select {
		case req := <-reqChan:
			// Process the received request.
			err := r.handleRequest(streamCtx, req)
			if err != nil {
				return err
			}

		case <-r.quit:
			return ErrShuttingDown

		case <-streamCtx.Done():
			return streamCtx.Err()

		case err := <-errChan:
			return err
		}
	}
}

// handleRequest processes the received request from the watch-only node, and
// sends the corresponding response back.
func (r *OutboundClient) handleRequest(streamCtx context.Context,
	req *walletrpc.SignCoordinatorRequest) error {

	log.Debugf("Processing a request from watch-only node of type: %T",
		req.GetSignRequestType())

	// Process the request.
	resp, err := r.process(streamCtx, req)
	if err != nil {
		errStr := "error processing the request in the remote " +
			"signer: " + err.Error()

		log.Errorf(errStr)

		// If we fail to process the request, we will send a SignerError
		// back to the watch-only node, indicating the nature of the
		// error.
		eType := &walletrpc.SignCoordinatorResponse_SignerError{
			SignerError: &walletrpc.SignerError{
				Error: errStr,
			},
		}

		resp = &signerResponse{
			RefRequestId:     req.GetRequestId(),
			SignResponseType: eType,
		}
	}

	// Send the response back to the watch-only node.
	err = r.sendResponse(streamCtx, resp)
	if err != nil {
		return fmt.Errorf("error sending response to watch-only "+
			"node: %w", err)
	}

	log.Tracef("Sent the following response to watch-only node: %v", resp)

	return nil
}

// process sends the passed request on to the appropriate server for processing
// it, and returns the response.
func (r *OutboundClient) process(ctx context.Context,
	req *walletrpc.SignCoordinatorRequest) (*signerResponse, error) {

	var (
		requestID = req.GetRequestId()
		signResp  = &signerResponse{
			RefRequestId: requestID,
		}
	)

	//nolint:lll
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
func (r *OutboundClient) sendResponse(ctx context.Context,
	resp *signerResponse) error {

	// We send the response in a goroutine to ensure we can return an error
	// if the send times out or the context is canceled. This is done to
	// ensure that this function won't block indefinitely.
	var (
		sendDone = make(chan struct{})
		errChan  = make(chan error)

		// The returnedChan is used to signal that this function has
		// already returned, so that the goroutines don't remain blocked
		// indefinitely when trying to send to the above channels.
		returnedChan = make(chan struct{})
	)
	defer close(returnedChan)

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()

		err := r.stream.Send(resp)
		if err != nil {
			select {
			case errChan <- err:
			case <-returnedChan:
			}

			return
		}

		close(sendDone)
	}()

	select {
	case err := <-errChan:
		return fmt.Errorf("send response to watch-only node error: %w",
			err)

	case <-time.After(r.requestTimeout):
		return errors.New("send response to watch-only node timeout")

	case <-r.quit:
		return ErrShuttingDown

	case <-ctx.Done():
		return ctx.Err()

	case <-sendDone:
		return nil
	}
}

// A compile time assertion to ensure OutboundClient meets the
// RemoteSignerClient interface.
var _ RemoteSignerClient = (*OutboundClient)(nil)
