package rpcwallet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/cert"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnrpc/watchonlyrpc"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

var (
	// ErrStreamCanceled is returned when the mock stream is canceled by the
	// remote signer client.
	ErrStreamCanceled = errors.New("stream canceled")

	// ErrMockResponseErr is a mock error that is returned by the mock
	// signer server.
	ErrMockResponseErr = errors.New("mock response error")

	// ErrStreamError is returned when the mock stream creation fails.
	ErrStreamError = errors.New("stream creation error")
)

// muSig2SignRequest names the existing wire variant used by ordered stream
// fixtures without repeating its long qualified name in nested literals.
type muSig2SignRequest = watchonlyrpc.SignCoordinatorRequest_MuSig2SignRequest

// mockStreamFeeder arranges each connection attempt before the client starts.
// streamCreated publishes the current stream to the owning test goroutine.
type mockStreamFeeder struct {
	mock.Mock
	stream        *mockStream
	streamCreated chan error
	quit          chan struct{}
}

// newMockStreamFeeder gives the test one expectation check and one Stop call.
func newMockStreamFeeder(t *testing.T) *mockStreamFeeder {
	t.Helper()
	feeder := &mockStreamFeeder{
		streamCreated: make(chan error),
		quit:          make(chan struct{}),
	}
	feeder.On("Stop").Run(func(mock.Arguments) {
		close(feeder.quit)
	}).Return().Once()
	t.Cleanup(func() {
		feeder.AssertExpectations(t)
	})

	return feeder
}

// expectStream arranges one successful connection and its bounded wire calls.
// Preparing the stream here keeps expectations in Arrange, including
// reconnects. The returned mock allows additional expectations before Start.
func (msf *mockStreamFeeder) expectStream(t *testing.T,
	sends, requests int) *mockStream {

	t.Helper()
	_, tlsCtx := remoteSignerTLSContexts(t)
	ctx, cancel := context.WithCancel(tlsCtx)
	t.Cleanup(cancel)
	stream := newMockStream(t, ctx, sends, requests)
	connection := NewStream(stream, func() error {
		cancel()

		return nil
	})
	msf.On("GetStream", mock.Anything).Run(func(args mock.Arguments) {
		caller, _ := args.Get(0).(context.Context)
		// The rendezvous publishes this generation before the test uses
		// its channels. Client cancellation always releases the
		// callback.
		msf.stream = stream
		select {
		case msf.streamCreated <- nil:

		case <-caller.Done():

		case <-msf.quit:
		}
	}).Return(connection, nil).Once()

	return stream
}

// expectStreamFailures arranges the exact failed attempts in the backoff test.
func (msf *mockStreamFeeder) expectStreamFailures(attempts int) {
	msf.On("GetStream", mock.Anything).Run(func(args mock.Arguments) {
		ctx, _ := args.Get(0).(context.Context)
		select {
		case msf.streamCreated <- ErrStreamError:
		case <-ctx.Done():
		case <-msf.quit:
		}
	}).Return((*Stream)(nil), ErrStreamError).Times(attempts)
}

// GetStream returns the generation selected by the test's Arrange expectations.
func (msf *mockStreamFeeder) GetStream(ctx context.Context) (*Stream, error) {
	args := msf.Called(ctx)
	stream, _ := args.Get(0).(*Stream)

	return stream, args.Error(1)
}

// Stop records the single client shutdown before releasing fixture waiters.
func (msf *mockStreamFeeder) Stop() {
	msf.Called()
}

// Compile-time checks keep the fixtures on the actual transport interfaces.
var _ SignCoordinatorStreamFeeder = (*mockStreamFeeder)(nil)
var _ StreamClient = (*mockStream)(nil)

// mockStream keeps channel rendezvous in Arrange callbacks, with one reader and
// one writer returning only their mock expectation results.
type mockStream struct {
	mock.Mock
	sendChan    chan *watchonlyrpc.SignCoordinatorResponse
	recvChan    chan *watchonlyrpc.SignCoordinatorRequest
	recvErrChan chan error
	ctx         context.Context //nolint:containedctx
}

// newMockStream arranges exactly the traffic the test observes. One final Recv
// may be waiting during cancellation; an unfinished replacement handshake may
// likewise stop before either IO call enters.
func newMockStream(t *testing.T, ctx context.Context,
	sends, requests int) *mockStream {

	t.Helper()
	stream := &mockStream{
		sendChan:    make(chan *watchonlyrpc.SignCoordinatorResponse),
		recvChan:    make(chan *watchonlyrpc.SignCoordinatorRequest),
		recvErrChan: make(chan error),
		ctx:         ctx,
	}
	t.Cleanup(func() {
		stream.AssertExpectations(t)
	})
	// Context reads are immutable transport access, not a business-call
	// count. Send/Recv expectations bound the actual protocol traffic.
	stream.On("Context").Return(ctx).Maybe()
	send := stream.On("Send", mock.Anything).Return(nil)
	switch sends {
	case 0:
		send.Maybe()

	case 1:
		send.Once()

	default:
		send.Times(sends)
	}
	send.Run(func(args mock.Arguments) {
		response, _ := args.Get(0).(*watchonlyrpc.
			SignCoordinatorResponse)
		var err error
		select {
		case <-ctx.Done():
			err = ErrStreamCanceled

		case stream.sendChan <- response:
		}
		// Writes are serialized, so Called returns this invocation's
		// result through the existing mock.Call return mechanism.
		send.Return(err)
	})
	for index, count := range []int{requests, 1} {
		if count == 0 {
			continue
		}
		recv := stream.On("Recv").Return(nil, nil)
		switch {
		case index == 1:
			recv.Maybe()

		case count == 1:
			recv.Once()

		default:
			recv.Times(count)
		}
		recv.Run(func(mock.Arguments) {
			var request *watchonlyrpc.SignCoordinatorRequest
			var err error
			select {
			case request = <-stream.recvChan:

			case err = <-stream.recvErrChan:

			case <-ctx.Done():
				err = ErrStreamCanceled
			}
			// One stream reader owns these dynamic channel results;
			// they remain mock return values rather than fake
			// methods.
			recv.Return(request, err)
		})
	}

	return stream
}

// Send returns the arranged result of writing this response to the peer.
func (ms *mockStream) Send(
	response *watchonlyrpc.SignCoordinatorResponse) error {

	args := ms.Called(response)

	return args.Error(0)
}

// Recv returns the arranged request or stream error from the peer.
func (ms *mockStream) Recv() (*watchonlyrpc.SignCoordinatorRequest, error) {
	args := ms.Called()
	request, _ := args.Get(0).(*watchonlyrpc.SignCoordinatorRequest)

	return request, args.Error(1)
}

// recvRequest delivers peer traffic without bypassing the arranged Recv call.
func (ms *mockStream) recvRequest(
	req *watchonlyrpc.SignCoordinatorRequest) error {

	select {
	case ms.recvChan <- req:
		return nil
	case <-ms.ctx.Done():
		return ErrStreamCanceled
	}
}

// recvErr fails the waiting transport read without calling the client directly.
func (ms *mockStream) recvErr(err error) error {
	select {
	case ms.recvErrChan <- err:
		return nil
	case <-ms.ctx.Done():
		return ErrStreamCanceled
	}
}

// handleHandshake simulates the handshake procedure between the remote signer
// client and the watch-only node.
func (ms *mockStream) handleHandshake(t *testing.T) error {
	var resp *watchonlyrpc.SignCoordinatorResponse

	// Wait for the handshake init from the remote signer client.
	select {
	case <-ms.ctx.Done():
		// If the context is canceled, we return an error to indicate
		// that the stream has been canceled.
		return ErrStreamCanceled
	case resp = <-ms.sendChan:
	}

	require.Equal(t, handshakeRequestID, resp.GetRefRequestId())
	require.NotEmpty(t, resp.GetSignerRegistration())

	proof, err := watchonlyrpc.RegistrationProof(
		ms.ctx, resp.GetSignerRegistration().GetRegistrationChallenge(),
		watchonlyrpc.RegistrationVersion,
	)
	require.NoError(t, err)
	complete := &watchonlyrpc.RegistrationResponse_RegistrationComplete{
		RegistrationComplete: &watchonlyrpc.RegistrationComplete{
			Signature:        proof,
			RegistrationInfo: watchonlyrpc.RegistrationVersion,
		},
	}

	rType := &watchonlyrpc.SignCoordinatorRequest_RegistrationResponse{
		RegistrationResponse: &watchonlyrpc.RegistrationResponse{
			RegistrationResponseType: complete,
		},
	}

	// Send a message to the client to simulate that the watch-only node has
	// accepted the registration and that it's completed.
	regCompleteMsg := &watchonlyrpc.SignCoordinatorRequest{
		RequestId:       handshakeRequestID,
		SignRequestType: rType,
	}

	return ms.recvRequest(regCompleteMsg)
}

// Header requires an explicit expectation for gRPC metadata access.
func (ms *mockStream) Header() (metadata.MD, error) {
	args := ms.Called()
	header, _ := args.Get(0).(metadata.MD)

	return header, args.Error(1)
}

// SendMsg rejects an unexpected generic gRPC write.
func (ms *mockStream) SendMsg(msg any) error {
	args := ms.Called(msg)

	return args.Error(0)
}

// Trailer returns metadata only when the test has arranged it.
func (ms *mockStream) Trailer() metadata.MD {
	args := ms.Called()
	trailer, _ := args.Get(0).(metadata.MD)

	return trailer
}

// CloseSend requires an explicit expectation for a half-close.
func (ms *mockStream) CloseSend() error {
	args := ms.Called()

	return args.Error(0)
}

// RecvMsg rejects an unexpected generic gRPC read.
func (ms *mockStream) RecvMsg(msg any) error {
	args := ms.Called(msg)

	return args.Error(0)
}

// Context returns the immutable transport context prepared in Arrange.
func (ms *mockStream) Context() context.Context {
	args := ms.Called()
	ctx, _ := args.Get(0).(context.Context)

	return ctx
}

// newTestRemoteSignerClient creates a new outbound remote signer client
// instance for testing purposes, and inserts the passed streamFeeder together
// with a mock sub-servers into the created instance.
func newTestRemoteSignerClient(t *testing.T,
	streamFeeder *mockStreamFeeder, handshake bool) *OutboundClient {

	wallet := &mockWalletKitServer{}
	signer := &mockSignerServer{}
	// The fixture owns verification so callers never duplicate
	// expectations.
	t.Cleanup(func() {
		signer.AssertExpectations(t)
	})
	client, err := NewOutboundClient(
		wallet, signer, streamFeeder,
		1*time.Second,
	)
	require.NoError(t, err)
	require.NoError(t, client.Start(t.Context()))

	// We expect the remote signer client attempt to create a stream during
	// the start up. So if the stream feeder is configured to succeed, we
	// need to handle the handshake procedure to finalize the stream set up.
	if handshake {
		// Wait for the stream to be created.
		err := <-streamFeeder.streamCreated
		require.NoError(t, err)

		err = streamFeeder.stream.handleHandshake(t)
		require.NoError(t, err)
	}

	return client
}

// TestPingResponse tests that we can send a ping request to the remote signer
// client, and that it will respond with a pong.
func TestPingResponse(t *testing.T) {
	t.Parallel()

	// Arrange the exact connection and wire traffic before starting the
	// client; fixture cleanup verifies each mock once after shutdown.

	mockFeeder := newMockStreamFeeder(t)
	mockFeeder.expectStream(t, 2, 2)

	client := newTestRemoteSignerClient(t, mockFeeder, true)
	defer func() {
		// Ensure that the remote signer client is stopped successfully
		// after the test.
		require.NoError(t, client.Stop())
	}()

	// create the ping request.
	pingReq := &watchonlyrpc.SignCoordinatorRequest_Ping{
		Ping: true,
	}

	requestID := uint64(2)

	req := &watchonlyrpc.SignCoordinatorRequest{
		RequestId:       requestID,
		SignRequestType: pingReq,
	}

	// Act by delivering one Ping through the arranged transport reader.
	err := mockFeeder.stream.recvRequest(req)
	require.NoError(t, err)

	// Wait for the response from the remote signer client.
	resp := <-mockFeeder.stream.sendChan

	// Assert the serialized writer returns a Pong for that request ID.
	require.Equal(t, requestID, resp.GetRefRequestId())
	require.True(t, resp.GetPong())
}

// TestMultiplePingResponses tests that we can send multiple ping requests to
// the remote signer client, and that it will respond with a pong for each
// request.
func TestMultiplePingResponses(t *testing.T) {
	t.Parallel()

	// Arrange the exact connection and wire traffic before starting the
	// client; fixture cleanup verifies each mock once after shutdown.

	mockFeeder := newMockStreamFeeder(t)
	mockFeeder.expectStream(t, 3, 3)

	client := newTestRemoteSignerClient(t, mockFeeder, true)
	defer func() {
		// Ensure that the remote signer client is stopped successfully
		// after the test.
		require.NoError(t, client.Stop())
	}()

	// Create the first ping request.
	pingReq := &watchonlyrpc.SignCoordinatorRequest_Ping{
		Ping: true,
	}

	requestID1 := uint64(2)

	req1 := &watchonlyrpc.SignCoordinatorRequest{
		RequestId:       requestID1,
		SignRequestType: pingReq,
	}

	// Act by delivering Pings sequentially through the transport reader.
	err := mockFeeder.stream.recvRequest(req1)
	require.NoError(t, err)

	// Wait for the first response from the remote signer client.
	resp1 := <-mockFeeder.stream.sendChan

	// Ensure that the response contains the correct request ID and that
	// it's a pong response.
	require.Equal(t, requestID1, resp1.GetRefRequestId())
	require.True(t, resp1.GetPong())

	// Create the second ping request.
	requestID2 := uint64(3)

	req2 := &watchonlyrpc.SignCoordinatorRequest{
		RequestId:       requestID2,
		SignRequestType: pingReq,
	}

	// Send the second request to the remote signer client.
	err = mockFeeder.stream.recvRequest(req2)
	require.NoError(t, err)

	// Wait for the second response from the remote signer client.
	resp2 := <-mockFeeder.stream.sendChan

	// Assert the second Pong preserves its distinct ID, proving the
	// response correlation survives repeated stream requests.
	require.Equal(t, requestID2, resp2.GetRefRequestId())
	require.True(t, resp2.GetPong())
}

// TestStreamRecvErrorHandling tests that the remote signer client will cancel
// the stream if an error is received over the stream.Recv() method.
// The remote signer client should then proceed to retry to create a new stream.
func TestStreamRecvErrorHandling(t *testing.T) {
	t.Parallel()

	// Arrange the exact connection and wire traffic before starting the
	// client; fixture cleanup verifies each mock once after shutdown.

	msf := newMockStreamFeeder(t)
	msf.expectStream(t, 1, 1)
	msf.expectStream(t, 0, 0)

	client := newTestRemoteSignerClient(t, msf, true)
	defer func() {
		// Ensure that the remote signer client is stopped successfully
		// after the test.
		require.NoError(t, client.Stop())
	}()

	// Fetch the stream context before the stream is canceled.
	streamCtx := msf.stream.Context()

	// Act by failing the entered transport read, without calling client
	// reconnect internals or changing the already arranged replacement.
	err := msf.stream.recvErr(ErrStreamCanceled)
	require.NoError(t, err)

	// Assert the old stream is cancelled and the prearranged replacement is
	// requested, exposing reconnect only through the transport boundary.
	<-streamCtx.Done()

	// Now we expect the remote signer client to retry to create a new
	// stream. We therefore ensure that the stream creation has been
	// attempted successfully.
	err = <-msf.streamCreated
	require.NoError(t, err)
}

// TestResponseError tests that the remote signer client will return a
// SignerError if it cannot process a received request.
func TestResponseError(t *testing.T) {
	t.Parallel()

	// Arrange the exact connection and wire traffic before starting the
	// client; fixture cleanup verifies each mock once after shutdown.

	msf := newMockStreamFeeder(t)
	msf.expectStream(t, 2, 2)

	client := newTestRemoteSignerClient(t, msf, true)
	defer func() {
		// Ensure that the remote signer client is stopped successfully
		// after the test.
		require.NoError(t, client.Stop())
	}()

	// Create a SignMessage request. As the remote signer client has an
	// mockSignerServer instance as the signrpc server, this request will
	// thrown an error when the signer server processes it.
	signMessageReq := &watchonlyrpc.SignCoordinatorRequest_SignMessageReq{
		SignMessageReq: &signrpc.SignMessageReq{},
	}

	requestID := uint64(2)

	req := &watchonlyrpc.SignCoordinatorRequest{
		RequestId:       requestID,
		SignRequestType: signMessageReq,
	}

	// Arrange the one backend failure that the stream must report to its
	// peer.
	backend, ok := client.signerServer.(*mockSignerServer)
	require.True(t, ok)
	backend.On(
		"SignMessage", mock.Anything, signMessageReq.SignMessageReq,
	).Return((*signrpc.SignMessageResp)(nil), ErrMockResponseErr).Once()

	// Act by sending the request through the running stream, not the
	// backend.
	err := msf.stream.recvRequest(req)
	require.NoError(t, err)

	// Wait for the response from the remote signer client.
	resp := <-msf.stream.sendChan

	// Assert the wire error retains its request ID and backend failure
	// text.
	require.Equal(t, requestID, resp.GetRefRequestId())

	// The response should be a SignerError, as the request could not be
	// processed.
	signErr := resp.GetSignerError()
	require.NotNil(t, signErr)

	// The error should contain the error message that was returned by the
	// mock signer server.
	require.Contains(t, signErr.GetError(), ErrMockResponseErr.Error())
}

// TestStreamCreationBackoff tests that the client will retry to create a stream
// if the stream creation fails, and that the backoff duration before retrying
// to set up the stream again increases with each failed attempt.
func TestStreamCreationBackoff(t *testing.T) {
	t.Parallel()

	// Arrange the exact connection and wire traffic before starting the
	// client; fixture cleanup verifies each mock once after shutdown.

	msf := newMockStreamFeeder(t)
	msf.expectStreamFailures(4)
	msf.expectStream(t, 1, 1)
	msf.expectStream(t, 0, 0)

	client := newTestRemoteSignerClient(t, msf, false)
	defer func() {
		// Ensure that the remote signer client is stopped successfully
		// after the test.
		require.NoError(t, client.Stop())
	}()

	// For testing purposes, we set the max retry timeout to a value that
	// ensures that the retry timeout will be capped on the fourth backoff.
	client.maxRetryTimeout = defaultRetryTimeout * 3

	// Act by observing the four arranged connection failures. Their
	// timestamps expose the public retry backoff before either success.
	err := <-msf.streamCreated
	require.Equal(t, ErrStreamError, err)

	lastStreamCreationAttempt := time.Now()

	// The first time the client fails to set up a stream, we expect that
	// the client will retry to create the stream after the default retry
	// timeout, without any multiplied backoff. Once that happens, the
	// streamCreated channel should receive the ErrStreamError.
	err = <-msf.streamCreated
	require.Equal(t, ErrStreamError, err)

	// Now let's verify that the client waited the default retry timeout
	// before retrying to recreate the stream.
	retryBackoff := time.Since(lastStreamCreationAttempt)
	expectedBackoff := time.Duration(float64(defaultRetryTimeout) *
		math.Pow(float64(retryMultiplier), 0)) // 0 for no multiplier

	// Assert each measured interval follows the configured backoff. We
	// allow a small margin of error (100ms) on the range bound to account
	// for the time it takes to execute the test code. We also allow a small
	// margin of error (10ms) on the lower bound to account for the time the
	// code execution takes between the creation of the stream, and when
	// retryBackoff is set.
	require.GreaterOrEqual(
		t, retryBackoff, expectedBackoff-10*time.Millisecond,
	)
	require.LessOrEqual(
		t, retryBackoff, expectedBackoff+100*time.Millisecond,
	)

	// Reset the last attempt time, so we can check the next retry.
	lastStreamCreationAttempt = time.Now()

	// Now let's wait until the client retries to create the stream again.
	// This time we expect that a multiplier of retryMultiplier^1 has been
	// applied to the backoff duration.
	err = <-msf.streamCreated
	require.Equal(t, ErrStreamError, err)

	// Verify that the retry backoff is within the expected range, with the
	// multiplier applied.
	retryBackoff = time.Since(lastStreamCreationAttempt)

	// The second backoff should have the multiplier applied once, therefore
	// the multiplier raised to the power of 1.
	expectedBackoff = time.Duration(float64(defaultRetryTimeout) *
		math.Pow(float64(retryMultiplier), 1))

	// Verify that the retry backoff is within the expected range.
	require.GreaterOrEqual(
		t, retryBackoff, expectedBackoff-10*time.Millisecond,
	)
	require.LessOrEqual(
		t, retryBackoff, expectedBackoff+100*time.Millisecond,
	)

	// Reset the last attempt time, so that we can check that the retry
	// timeout correctly gets multiplied again.
	lastStreamCreationAttempt = time.Now()

	// Now let's wait until the client retries to create the stream again.
	// This time we expect that a multiplier of retryMultiplier^2 has been
	// applied to the backoff duration.
	err = <-msf.streamCreated
	require.Equal(t, ErrStreamError, err)

	// Verify that the retry backoff is within the expected range, with the
	// multiplier applied.
	retryBackoff = time.Since(lastStreamCreationAttempt)

	// The third backoff should have the multiplier applied twice, therefore
	// the multiplier raised to the power of 2.
	expectedBackoff = time.Duration(float64(defaultRetryTimeout) *
		math.Pow(float64(retryMultiplier), 2))

	// Verify that the retry backoff is within the expected range.
	require.GreaterOrEqual(
		t, retryBackoff, expectedBackoff-10*time.Millisecond,
	)
	require.LessOrEqual(
		t, retryBackoff, expectedBackoff+100*time.Millisecond,
	)

	// The next arranged attempt succeeds. Completing its handshake resets
	// the retry delay without changing any expectations during Act.

	// Reset the last attempt time, so we can check the next retry.
	lastStreamCreationAttempt = time.Now()

	// Now let's wait until the client retries to create the stream again.
	// Even though the creation will succeed, it'll still take the expected
	// backoff time before the client attempts to make the successful stream
	// creation. However, since we capped the maximum retry timeout, and
	// this was the third time the retry timeout was multiplied, the maximum
	// retry timeout should have been reached. Therefore, we expect the
	// retry timeout to be set to client.maxRetryTimeout.
	err = <-msf.streamCreated

	// We expect the stream creation to succeed this time.
	require.NoError(t, err)

	// Verify that the retry backoff is within the expected range, with the
	// multiplier applied.
	retryBackoff = time.Since(lastStreamCreationAttempt)

	// The fourth backoff should have the multiplier applied three times,
	// which would result in a backoff larger than the client’s
	// maxRetryTimeout. Therefore, the backoff should have been capped at
	// the client’s maxRetryTimeout.
	expectedBackoff = client.maxRetryTimeout

	// Verify that the retry backoff was capped.
	require.GreaterOrEqual(
		t, retryBackoff, expectedBackoff-10*time.Millisecond,
	)
	require.LessOrEqual(
		t, retryBackoff, expectedBackoff+100*time.Millisecond,
	)

	// As the steam creation was successful, the client will proceed with
	// the handshake procedure before the stream creation is considered
	// successful. We therefore need to simulate the handshake procedure.
	err = msf.stream.handleHandshake(t)
	require.NoError(t, err)

	// Now let's cause the stream to fail again, to verify that the client
	// reset the backoff to the default value, as the last stream creation
	// attempt was successful.
	err = msf.stream.recvErr(ErrStreamCanceled)
	require.NoError(t, err)

	// Reset the last attempt time, so we can check the next retry.
	lastStreamCreationAttempt = time.Now()

	// Now let's wait till the client retries to create the stream again.
	err = <-msf.streamCreated
	// We expect the stream creation to also succeed this time.
	require.NoError(t, err)

	// As the backoff is reset to the default value, we expect that no
	// multiplier has been applied to the backoff duration.
	retryBackoff = time.Since(lastStreamCreationAttempt)
	expectedBackoff = time.Duration(float64(defaultRetryTimeout) *
		math.Pow(float64(retryMultiplier), 0))

	// Verify that the retry backoff is within the expected range.
	require.GreaterOrEqual(
		t, retryBackoff, expectedBackoff-10*time.Millisecond,
	)
	require.LessOrEqual(
		t, retryBackoff, expectedBackoff+100*time.Millisecond,
	)
}

// mockWalletKitServer is a mock walletrpc.WalletKitServer implementation that
// panics for all request methods.
type mockWalletKitServer struct {
	walletrpc.UnimplementedWalletKitServer
}

var _ walletrpc.WalletKitServer = (*mockWalletKitServer)(nil)

// Name returns a unique string representation of the sub-server. This
// can be used to identify the sub-server and also de-duplicate them.
func (m *mockWalletKitServer) Name() string { return walletrpc.SubServerName }

// Start starts the sub-server and all goroutines it needs to operate.
func (m *mockWalletKitServer) Start() error { return nil }

// Stop signals that the sub-server should wrap up any lingering
// requests, and being a graceful shutdown.
func (m *mockWalletKitServer) Stop() error { return nil }

// InjectDependencies populates the sub-server's dependencies. If the
// finalizeDependencies boolean is true, then the sub-server will finalize its
// dependencies and return an error if any required dependencies are missing.
func (m *mockWalletKitServer) InjectDependencies(
	_ lnrpc.SubServerConfigDispatcher, _ bool) error {

	return nil
}

// mockSignerServer is a mock signrpc.SignerServer implementation that panics
// for all request methods except SignMessage.
type mockSignerServer struct {
	mock.Mock
	signrpc.UnimplementedSignerServer
}

var _ signrpc.SignerServer = (*mockSignerServer)(nil)

// SignMessage delegates signing behavior to the test's explicit expectation.
func (m *mockSignerServer) SignMessage(ctx context.Context,
	req *signrpc.SignMessageReq) (*signrpc.SignMessageResp, error) {

	args := m.Called(ctx, req)
	resp, _ := args.Get(0).(*signrpc.SignMessageResp)

	return resp, args.Error(1)
}

// Name returns a unique string representation of the sub-server. This
// can be used to identify the sub-server and also de-duplicate them.
func (m *mockSignerServer) Name() string { return "SignRPC" }

// Start starts the sub-server and all goroutines it needs to operate.
func (m *mockSignerServer) Start() error { return nil }

// Stop signals that the sub-server should wrap up any lingering
// requests, and being a graceful shutdown.
func (m *mockSignerServer) Stop() error { return nil }

// InjectDependencies populates the sub-server's dependencies. If the
// finalizeDependencies boolean is true, then the sub-server will finalize its
// dependencies and return an error if any required dependencies are missing.
func (m *mockSignerServer) InjectDependencies(
	_ lnrpc.SubServerConfigDispatcher, _ bool) error {

	return nil
}

// TestRemoteSignerClientBuilder tests that the RemoteSignerClientBuilder
// returns the correct client for every combination of the watch-only node
// configuration and the availability of the sub-servers that are required to
// service signing requests.
//
// The case that matters most is a node that has been configured to act as a
// remote signer while the required sub-servers are unavailable, i.e. lnd was
// built without the signrpc and/or walletrpc build tags. That must be a loud
// error, as a NoOpClient would leave the signer node running and reporting
// healthy while never connecting to the watch-only node.
func TestRemoteSignerClientBuilder(t *testing.T) {
	t.Parallel()

	var (
		walletKitServer = &mockWalletKitServer{}
		signerServer    = &mockSignerServer{}
	)

	tests := []struct {
		name string

		// isSignerNode indicates if the node has been configured to act
		// as a remote signer node.
		isSignerNode bool

		// subServers are the sub-servers that are passed to the
		// builder.
		subServers []lnrpc.SubServer

		// expectedClient is the type of client we expect the builder to
		// return. It is nil if we expect the builder to error out.
		expectedClient RemoteSignerClient

		// expectedErr is the error we expect the builder to return. It
		// is nil if we expect no error.
		expectedErr error

		// expectedErrContains are substrings that the returned error
		// must additionally contain, to check that it names the
		// missing build tags.
		expectedErrContains []string
	}{
		{
			name:         "not a signer node, no sub-servers",
			isSignerNode: false,
			subServers:   nil,

			// A node that isn't acting as a remote signer has no
			// use for the sub-servers, so their absence is
			// expected and must remain a no-op.
			expectedClient: &NoOpClient{},
		},
		{
			name:         "not a signer node, all sub-servers",
			isSignerNode: false,
			subServers: []lnrpc.SubServer{
				walletKitServer, signerServer,
			},
			expectedClient: &NoOpClient{},
		},
		{
			name:         "signer node, no sub-servers",
			isSignerNode: true,
			subServers:   nil,

			// Both sub-servers are missing, so both build tags
			// should be named in the error.
			expectedErr: ErrSignerSubServersMissing,
			expectedErrContains: []string{
				walletBuildTag,
				signerBuildTag,
			},
		},
		{
			name:         "signer node, only the walletkit server",
			isSignerNode: true,
			subServers:   []lnrpc.SubServer{walletKitServer},

			// Only the signer sub-server is missing, so only that
			// build tag should be reported as missing.
			expectedErr: ErrSignerSubServersMissing,
			expectedErrContains: []string{
				"lnd was built without the " + signerBuildTag +
					" sub-server(s)",
			},
		},
		{
			name:         "signer node, only the signer server",
			isSignerNode: true,
			subServers:   []lnrpc.SubServer{signerServer},
			expectedErr:  ErrSignerSubServersMissing,
			expectedErrContains: []string{
				"lnd was built without the " + walletBuildTag +
					" sub-server(s)",
			},
		},
		{
			name:         "signer node, all sub-servers",
			isSignerNode: true,
			subServers: []lnrpc.SubServer{
				walletKitServer, signerServer,
			},
			expectedClient: &OutboundClient{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := lncfg.DefaultWatchOnlyNodeCfg()
			cfg.ExperimentalEnable = test.isSignerNode

			builder := NewRemoteSignerClientBuilder(cfg)

			client, err := builder.Build(test.subServers)

			if test.expectedErr != nil {
				require.ErrorIs(t, err, test.expectedErr)
				require.Nil(t, client)

				for _, sub := range test.expectedErrContains {
					require.ErrorContains(t, err, sub)
				}

				return
			}

			require.NoError(t, err)
			require.IsType(t, test.expectedClient, client)
		})
	}
}

// TestRemoteSignerClientBuilderErrorNamesBuildInstructions tests that the error
// returned when a signer node is missing its sub-servers tells the operator how
// to fix it. The root cause is a missing build tag, which an operator has no
// way of inferring from a bare "sub-server unavailable" message.
func TestRemoteSignerClientBuilderErrorNamesBuildInstructions(t *testing.T) {
	t.Parallel()

	cfg := lncfg.DefaultWatchOnlyNodeCfg()
	cfg.ExperimentalEnable = true

	_, err := NewRemoteSignerClientBuilder(cfg).Build(nil)
	require.ErrorIs(t, err, ErrSignerSubServersMissing)

	// The error must name the build tags to enable, and point at a concrete
	// way of producing a binary that has them.
	require.ErrorContains(t, err, "build tags enabled")
	require.ErrorContains(t, err, "make build with-rpc=1")
	require.ErrorContains(t, err, "official release build")
}

// remoteSignerTLSContexts provides real exporter state to existing stream
// fixtures, so their handshake success never bypasses production validation.
func remoteSignerTLSContexts(t *testing.T) (context.Context, context.Context) {
	t.Helper()
	certBytes, keyBytes, err := cert.GenCertPair(
		"remote signer test", nil, nil, false, time.Hour,
	)
	require.NoError(t, err)
	pair, err := tls.X509KeyPair(certBytes, keyBytes)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(certBytes))
	left, right := net.Pipe()
	t.Cleanup(func() {
		_ = left.Close()
		_ = right.Close()
	})
	require.NoError(t, left.SetDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, right.SetDeadline(time.Now().Add(5*time.Second)))
	server := tls.Server(left, cert.TLSConfFromCert(pair))
	client := tls.Client(right, &tls.Config{
		RootCAs:    roots,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
	})
	// Progress both TLS endpoints together and return errors to the owning
	// test goroutine. Raw connection cleanup also releases a failed
	// handshake.
	done := make(chan error, 1)
	go func() {
		done <- server.HandshakeContext(t.Context())
	}()
	require.NoError(t, client.HandshakeContext(t.Context()))
	require.NoError(t, <-done)

	return peer.NewContext(t.Context(), &peer.Peer{
			AuthInfo: credentials.TLSInfo{
				State: server.ConnectionState(),
			},
		}), peer.NewContext(t.Context(), &peer.Peer{
			AuthInfo: credentials.TLSInfo{
				State: client.ConnectionState(),
			},
		})
}

// MuSig2Sign exposes explicit expectations for ordered backend operations.
func (m *mockSignerServer) MuSig2Sign(ctx context.Context,
	req *signrpc.MuSig2SignRequest) (*signrpc.MuSig2SignResponse, error) {

	args := m.Called(ctx, req)
	response, _ := args.Get(0).(*signrpc.MuSig2SignResponse)

	return response, args.Error(1)
}

// TestRemoteSignerSessionOrdering serializes one session without reserving a
// worker merely to wait for that session's already-running predecessor.
func TestRemoteSignerSessionOrdering(t *testing.T) {
	// Arrange two same-session calls and an independent session. The second
	// call records entry so its ordering can be checked at the backend
	// boundary.
	feeder := newMockStreamFeeder(t)
	feeder.expectStream(t, 4, 4)
	client := newTestRemoteSignerClient(t, feeder, true)
	client.requestTimeout = 5 * time.Second
	backend, ok := client.signerServer.(*mockSignerServer)
	require.True(t, ok)
	entered := make(chan struct{})
	secondEntered := make(chan struct{})
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(func() {
		release()
		require.NoError(t, client.Stop())
	})
	first := &signrpc.MuSig2SignRequest{
		SessionId:     []byte{1},
		MessageDigest: []byte{1},
	}
	second := &signrpc.MuSig2SignRequest{
		SessionId:     []byte{1},
		MessageDigest: []byte{2},
	}
	other := &signrpc.MuSig2SignRequest{
		SessionId: []byte{2},
	}
	backend.On(
		"MuSig2Sign", mock.Anything, first,
	).Run(func(mock.Arguments) {
		close(entered)
		<-gate
	}).Return(&signrpc.MuSig2SignResponse{}, nil).Once()
	backend.On(
		"MuSig2Sign", mock.Anything, second,
	).Run(func(mock.Arguments) {
		close(secondEntered)
	}).Return(&signrpc.MuSig2SignResponse{}, nil).Once()
	backend.On("MuSig2Sign", mock.Anything, other).Return(
		&signrpc.MuSig2SignResponse{}, nil,
	).Once()

	// Act in receive order, holding the first call while another session
	// runs.
	requests := []*signrpc.MuSig2SignRequest{first, second, other}
	for index, request := range requests {
		require.NoError(t, feeder.stream.recvRequest(
			&watchonlyrpc.SignCoordinatorRequest{
				RequestId: uint64(index + 2),
				SignRequestType: &muSig2SignRequest{
					MuSig2SignRequest: request,
				},
			},
		))
		if index == 0 {
			<-entered
		}
	}

	// Assert the independent session completes while the successor remains
	// outside the backend, then release and observe FIFO responses for its
	// session.
	require.Equal(
		t, uint64(4), (<-feeder.stream.sendChan).GetRefRequestId(),
	)
	select {
	case <-secondEntered:
		t.Fatal("same-session successor entered " +
			"before predecessor returned")

	default:
	}
	release()
	require.Equal(
		t, uint64(2), (<-feeder.stream.sendChan).GetRefRequestId(),
	)
	require.Equal(
		t, uint64(3), (<-feeder.stream.sendChan).GetRefRequestId(),
	)
}

// TestRemoteSignerBackpressure exposes saturation instead of blocking Ping or
// growing a new set of workers behind the synchronous backend calls.
func TestRemoteSignerBackpressure(t *testing.T) {
	// Arrange enough blocked calls to fill the existing workers and queue,
	// then reserve IDs for overflow and Ping after the handshake.
	signingRequests := signerWorkerCount + maxPendingSignerRequests + 1
	busyID := handshakeRequestID + uint64(signingRequests)
	pingID := busyID + 1
	feeder := newMockStreamFeeder(t)
	feeder.expectStream(t, 3, signingRequests+2)
	client := newTestRemoteSignerClient(t, feeder, true)
	client.requestTimeout = 10 * time.Second
	backend, ok := client.signerServer.(*mockSignerServer)
	require.True(t, ok)
	entered := make(chan struct{}, signerWorkerCount)
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	stop := sync.OnceValue(client.Stop)
	t.Cleanup(func() {
		stopped := make(chan error, 1)
		go func() {
			stopped <- stop()
		}()
		<-feeder.stream.Context().Done()
		release()
		require.NoError(t, <-stopped)
	})
	request := &signrpc.SignMessageReq{
		Msg: []byte("blocked"),
	}
	backend.On(
		"SignMessage", mock.Anything, request,
	).Run(func(mock.Arguments) {
		entered <- struct{}{}
		<-gate
	}).Return(&signrpc.SignMessageResp{}, nil).Times(signerWorkerCount)

	// Act by filling the workers and bounded pending queue, then sending
	// one excess request and a Ping. Only public responses expose queue
	// pressure.
	for index := range signingRequests {
		id := handshakeRequestID + uint64(index) + 1
		require.NoError(t, feeder.stream.recvRequest(
			&watchonlyrpc.SignCoordinatorRequest{
				RequestId: id,
				SignRequestType: &watchonlyrpc.
					SignCoordinatorRequest_SignMessageReq{
					SignMessageReq: request,
				},
			},
		))
		if index < signerWorkerCount {
			<-entered
		}
	}
	require.NoError(t, feeder.stream.recvRequest(
		&watchonlyrpc.SignCoordinatorRequest{
			RequestId: pingID,
			SignRequestType: &watchonlyrpc.
				SignCoordinatorRequest_Ping{
				Ping: true,
			},
		},
	))

	// Assert saturation is visible and Ping stays independent. Cleanup
	// cancels queued work before releasing entered calls, so the backend
	// expectation permits only the original worker count to enter.
	busy := <-feeder.stream.sendChan
	require.Equal(t, busyID, busy.GetRefRequestId())
	require.Contains(t, busy.GetSignerError().GetError(), "busy")
	pong := <-feeder.stream.sendChan
	require.Equal(t, pingID, pong.GetRefRequestId())
	require.True(t, pong.GetPong())
}

// TestRemoteSignerDispatchShutdown checks that Stop drains an entered backend
// before returning, even when that backend cannot honor cancellation.
func TestRemoteSignerDispatchShutdown(t *testing.T) {
	// Arrange one entered synchronous call. Cleanup releases it before
	// joining Stop, including when an assertion aborts the normal path.
	feeder := newMockStreamFeeder(t)
	feeder.expectStream(t, 1, 2)
	client := newTestRemoteSignerClient(t, feeder, true)
	client.requestTimeout = 10 * time.Second
	backend, ok := client.signerServer.(*mockSignerServer)
	require.True(t, ok)
	entered := make(chan struct{})
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	stop := sync.OnceValue(client.Stop)
	t.Cleanup(func() {
		release()
		require.NoError(t, stop())
	})
	request := &signrpc.MuSig2SignRequest{
		SessionId:     []byte{1},
		MessageDigest: []byte{1},
	}
	backend.On(
		"MuSig2Sign", mock.Anything, request,
	).Run(func(mock.Arguments) {
		close(entered)
		<-gate
	}).Return(&signrpc.MuSig2SignResponse{}, nil).Once()
	require.NoError(t, feeder.stream.recvRequest(
		&watchonlyrpc.SignCoordinatorRequest{
			RequestId: 2,
			SignRequestType: &muSig2SignRequest{
				MuSig2SignRequest: request,
			},
		},
	))
	<-entered

	// Act by stopping the client while the entered backend still holds
	// wallet access, and wait for cancellation to reach the stream.
	stopped := make(chan error, 1)
	go func() {
		stopped <- stop()
	}()
	<-feeder.stream.Context().Done()

	// Assert Stop remains pending until wallet access ends, then joins it.
	select {
	case err := <-stopped:
		t.Fatalf("Stop abandoned entered backend: %v", err)

	default:
	}
	release()
	require.NoError(t, <-stopped)
}

// TestRemoteSignerDispatchReconnect discards an old stream's late result and
// allows its same-session successor to complete on the replacement stream.
func TestRemoteSignerDispatchReconnect(t *testing.T) {
	// Arrange a backend that ignores cancellation. Both jobs share a
	// session, so the successor response also proves its predecessor ended.
	feeder := newMockStreamFeeder(t)
	feeder.expectStream(t, 1, 2)
	feeder.expectStream(t, 2, 2)
	client := newTestRemoteSignerClient(t, feeder, true)
	client.requestTimeout = 10 * time.Second
	backend, ok := client.signerServer.(*mockSignerServer)
	require.True(t, ok)
	entered := make(chan struct{})
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(func() {
		release()
		require.NoError(t, client.Stop())
	})
	first := &signrpc.MuSig2SignRequest{
		SessionId:     []byte{1},
		MessageDigest: []byte{1},
	}
	backend.On(
		"MuSig2Sign", mock.Anything, first,
	).Run(func(mock.Arguments) {
		close(entered)
		<-gate
	}).Return(&signrpc.MuSig2SignResponse{}, nil).Once()
	next := &signrpc.MuSig2SignRequest{
		SessionId:     []byte{1},
		MessageDigest: []byte{2},
	}
	backend.On(
		"MuSig2Sign", mock.Anything, next,
	).Return(&signrpc.MuSig2SignResponse{}, nil).Once()
	require.NoError(t, feeder.stream.recvRequest(
		&watchonlyrpc.SignCoordinatorRequest{
			RequestId: 2,
			SignRequestType: &muSig2SignRequest{
				MuSig2SignRequest: first,
			},
		},
	))
	<-entered

	// Act by reconnecting while the old backend is still entered, then
	// submit its same-session successor before releasing the old call.
	require.NoError(t, feeder.stream.recvErr(ErrStreamError))
	require.NoError(t, <-feeder.streamCreated)
	require.NoError(t, feeder.stream.handleHandshake(t))
	require.NoError(t, feeder.stream.recvRequest(
		&watchonlyrpc.SignCoordinatorRequest{
			RequestId: 3,
			SignRequestType: &muSig2SignRequest{
				MuSig2SignRequest: next,
			},
		},
	))
	release()

	// Assert only the successor result reaches the new stream. FIFO
	// execution ensures the old result has already been discarded.
	response := <-feeder.stream.sendChan
	require.Equal(t, uint64(3), response.GetRefRequestId())
	require.NotNil(t, response.GetMuSig2SignResponse())
}

// TestRemoteSignerRegistration rejects invalid registration before the running
// client can admit signing work; existing Ping tests cover valid admission.
func TestRemoteSignerRegistration(t *testing.T) {
	cases := []struct {
		name    string
		proof   string
		version string
	}{
		{
			name:    "missing proof",
			proof:   "missing",
			version: watchonlyrpc.RegistrationVersion,
		},
		{
			name:    "changed challenge",
			proof:   "changed challenge",
			version: watchonlyrpc.RegistrationVersion,
		},
		{
			name:    "wrong TLS connection",
			proof:   "other connection",
			version: watchonlyrpc.RegistrationVersion,
		},
		{
			name:    "incompatible version",
			version: "unsupported",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange the client without completing admission.
			// Signing backend calls must remain unused.
			feeder := newMockStreamFeeder(t)
			feeder.expectStream(t, 1, 1)
			client := newTestRemoteSignerClient(t, feeder, false)
			defer func() {
				require.NoError(t, client.Stop())
			}()
			require.NoError(t, <-feeder.streamCreated)
			stream := feeder.stream
			registration := <-stream.sendChan
			proofCtx := stream.Context()
			if tc.proof == "other connection" {
				proofCtx, _ = remoteSignerTLSContexts(t)
			}
			challenge := registration.GetSignerRegistration().
				GetRegistrationChallenge()
			if tc.proof == "changed challenge" {
				// Copy the value to leave the original client
				// challenge intact for verification.
				challenge = append([]byte(nil), challenge...)
				challenge[0] ^= 1
			}
			proof, err := watchonlyrpc.RegistrationProof(
				proofCtx, challenge,
				watchonlyrpc.RegistrationVersion,
			)
			require.NoError(t, err)
			if tc.proof == "missing" {
				proof = ""
			}

			// Act through the running protocol with one altered
			// proof/version, leaving the transport authenticated.
			complete := &watchonlyrpc.RegistrationComplete{
				Signature:        proof,
				RegistrationInfo: tc.version,
			}
			completed := &RSRegistrationComplete{
				RegistrationComplete: complete,
			}
			response := &watchonlyrpc.RegistrationResponse{
				RegistrationResponseType: completed,
			}
			reply := &RSRegistrationResponse{
				RegistrationResponse: response,
			}
			require.NoError(t, stream.recvRequest(
				&watchonlyrpc.SignCoordinatorRequest{
					RequestId:       handshakeRequestID,
					SignRequestType: reply,
				},
			))

			// Assert invalid admission closes the stream before
			// signing can start; fixture cleanup verifies no
			// backend operation was invoked.
			select {
			case <-stream.Context().Done():

			case <-time.After(2 * time.Second):
				t.Fatal("invalid registration stayed active")
			}
		})
	}
}

// TestRegistrationProofInputs exercises protocol guards with a usable TLS
// exporter so transport failure cannot mask either rejected input.
func TestRegistrationProofInputs(t *testing.T) {
	cases := []struct {
		name      string
		version   string
		challenge []byte
		wantCode  codes.Code
	}{
		{
			name:      "valid",
			version:   watchonlyrpc.RegistrationVersion,
			challenge: make([]byte, 32),
		},
		{
			name:      "unsupported version",
			version:   "unsupported",
			challenge: make([]byte, 32),
			wantCode:  codes.InvalidArgument,
		},
		{
			name:      "truncated challenge",
			version:   watchonlyrpc.RegistrationVersion,
			challenge: make([]byte, 31),
			wantCode:  codes.InvalidArgument,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange a real TLS exporter for every case. The valid
			// control rules out transport failure as the rejection.
			ctx, _ := remoteSignerTLSContexts(t)

			// Act through the public proof producer, varying only
			// the protocol version or challenge length.
			proof, err := watchonlyrpc.RegistrationProof(
				ctx, tc.challenge, tc.version,
			)

			// Assert the stable code so transport failures cannot
			// replace the intended protocol rejection.
			require.Equal(t, tc.wantCode, status.Code(err))
			if tc.wantCode != codes.OK {
				require.Empty(t, proof)

				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, proof)
		})
	}
}

// TestRemoteSignerQueueExpiry returns queued deadlines without entering the
// backend, and reclaims their capacity while every worker remains occupied.
func TestRemoteSignerQueueExpiry(t *testing.T) {
	// Arrange the production worker and pending limits through public
	// traffic. Only the first workers may enter; Stop cancels the transport
	// before cleanup releases these non-cancellable backend calls.
	feeder := newMockStreamFeeder(t)
	requests := signerWorkerCount + maxPendingSignerRequests + 3
	feeder.expectStream(t, maxPendingSignerRequests+3, requests)
	client := newTestRemoteSignerClient(t, feeder, true)
	client.requestTimeout = 500 * time.Millisecond
	backend, ok := client.signerServer.(*mockSignerServer)
	require.True(t, ok)
	entered := make(chan struct{}, signerWorkerCount)
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	stop := sync.OnceValue(client.Stop)
	t.Cleanup(func() {
		stopped := make(chan error, 1)
		go func() {
			stopped <- stop()
		}()
		<-feeder.stream.Context().Done()
		release()
		require.NoError(t, <-stopped)
	})
	request := &signrpc.SignMessageReq{
		Msg: []byte("blocked"),
	}
	backend.On("SignMessage", mock.Anything, request).Run(
		func(mock.Arguments) {
			entered <- struct{}{}
			<-gate
		},
	).Return(&signrpc.SignMessageResp{}, nil).Times(signerWorkerCount)

	// Act by filling the workers and queue without releasing any backend.
	// Each wire request has a distinct ID so duplicate/missing replies are
	// visible independently of dispatcher state.
	for index := range signerWorkerCount + maxPendingSignerRequests {
		id := handshakeRequestID + uint64(index) + 1
		require.NoError(t, feeder.stream.recvRequest(
			&watchonlyrpc.SignCoordinatorRequest{
				RequestId: id,
				SignRequestType: &watchonlyrpc.
					SignCoordinatorRequest_SignMessageReq{
					SignMessageReq: request,
				},
			},
		))
		if index < signerWorkerCount {
			<-entered
		}
	}

	// Assert every queued job receives one deadline error before a worker
	// is freed. Backend expectations reject entry by any expired job.
	seen := make(map[uint64]bool)
	for range maxPendingSignerRequests {
		select {
		case response := <-feeder.stream.sendChan:
			id := response.GetRefRequestId()
			require.Greater(
				t, id,
				handshakeRequestID+uint64(signerWorkerCount),
			)
			require.False(t, seen[id])
			seen[id] = true
			require.Contains(
				t, response.GetSignerError().GetError(),
				context.DeadlineExceeded.Error(),
			)

		case <-time.After(2 * time.Second):
			t.Fatal("queued request received no deadline response")
		}
	}

	// Act again while all workers remain blocked: the expired queue must
	// admit fresh work, and Ping must still bypass signing capacity.
	freshID := handshakeRequestID + uint64(signerWorkerCount+
		maxPendingSignerRequests) + 1
	require.NoError(t, feeder.stream.recvRequest(
		&watchonlyrpc.SignCoordinatorRequest{
			RequestId: freshID,
			SignRequestType: &watchonlyrpc.
				SignCoordinatorRequest_SignMessageReq{
				SignMessageReq: request,
			},
		},
	))
	require.NoError(t, feeder.stream.recvRequest(
		&watchonlyrpc.SignCoordinatorRequest{
			RequestId: freshID + 1,
			SignRequestType: &watchonlyrpc.
				SignCoordinatorRequest_Ping{
				Ping: true,
			},
		},
	))

	// Assert Ping progresses and the fresh job expires rather than being
	// refused as busy. No worker has been released during either phase.
	pong := <-feeder.stream.sendChan
	require.Equal(t, freshID+1, pong.GetRefRequestId())
	require.True(t, pong.GetPong())
	response := <-feeder.stream.sendChan
	require.Equal(t, freshID, response.GetRefRequestId())
	require.Contains(
		t, response.GetSignerError().GetError(),
		context.DeadlineExceeded.Error(),
	)
}

// TestRemoteSignerWriteTimeout reconnects when an unread response writer
// fills routing capacity, so the main loop cannot consume its own IO failure.
func TestRemoteSignerWriteTimeout(t *testing.T) {
	// Arrange a bounded burst that leaves the first Pong unwritten and
	// fills the existing response buffer. Only registration and the first
	// blocked response enter Send on the old stream; the next is usable.
	// One response blocks in Send and another blocks routing after the
	// per-stream backlog fills; derive that boundary from its actual limit.
	const pings = maxPendingSignerResponses + 2
	feeder := newMockStreamFeeder(t)
	feeder.expectStream(t, 2, pings+1)
	feeder.expectStream(t, 2, 2)
	client := newTestRemoteSignerClient(t, feeder, true)
	client.requestTimeout = 500 * time.Millisecond
	t.Cleanup(func() {
		require.NoError(t, client.Stop())
	})
	old := feeder.stream

	// Act through public wire traffic without reading old Pongs. Its
	// response write deadline must close the transport and release routing.
	for index := range pings {
		id := uint64(index) + handshakeRequestID + 1
		require.NoError(t, old.recvRequest(
			&watchonlyrpc.SignCoordinatorRequest{
				RequestId: id,
				SignRequestType: &watchonlyrpc.
					SignCoordinatorRequest_Ping{
					Ping: true,
				},
			},
		))
	}

	// Assert a replacement is requested without Stop, then complete its
	// registration and verify it can answer a fresh public Ping.
	select {
	case err := <-feeder.streamCreated:
		require.NoError(t, err)

	case <-time.After(4 * time.Second):
		t.Fatal("write timeout did not release response backpressure")
	}
	require.NoError(t, feeder.stream.handleHandshake(t))
	require.NoError(t, feeder.stream.recvRequest(
		&watchonlyrpc.SignCoordinatorRequest{
			RequestId: 2,
			SignRequestType: &watchonlyrpc.
				SignCoordinatorRequest_Ping{
				Ping: true,
			},
		},
	))
	pong := <-feeder.stream.sendChan
	require.Equal(t, uint64(2), pong.GetRefRequestId())
	require.True(t, pong.GetPong())
}

// TestRemoteSignerSendFailure releases full response routing when Send returns
// an error before its write deadline, allowing reconnect without client Stop.
func TestRemoteSignerSendFailure(t *testing.T) {
	// Arrange one successful registration Send, then one gated Send error.
	// The prepared stream permits exact expectations before Start. Cleanup
	// releases the gate before joining client IO if an assertion aborts.
	const pings = maxPendingSignerResponses + 3
	feeder := newMockStreamFeeder(t)
	old := feeder.expectStream(t, 1, pings+1)
	feeder.expectStream(t, 0, 0)
	failSend := make(chan struct{})
	release := sync.OnceFunc(func() { close(failSend) })
	old.On("Send", mock.Anything).Run(func(mock.Arguments) {
		<-failSend
	}).Return(ErrMockResponseErr).Once()
	client := newTestRemoteSignerClient(t, feeder, true)
	client.requestTimeout = 10 * time.Second
	t.Cleanup(func() {
		release()
		require.NoError(t, client.Stop())
	})

	// Act through enough peer traffic to block Send, fill the response
	// backlog, block routing and complete one further reader rendezvous.
	// That last receive proves routing is blocked before Send fails.
	for index := range pings {
		id := handshakeRequestID + uint64(index) + 1
		require.NoError(t, old.recvRequest(
			&watchonlyrpc.SignCoordinatorRequest{
				RequestId: id,
				SignRequestType: &watchonlyrpc.
					SignCoordinatorRequest_Ping{
					Ping: true,
				},
			},
		))
	}
	release()

	// Assert Send failure closes the old transport and requests a new one
	// before the ten-second write timeout, without manually stopping the
	// client. Timeout coverage checks successful reconnect traffic.
	select {
	case err := <-feeder.streamCreated:
		require.NoError(t, err)

	case <-time.After(4 * time.Second):
		t.Fatal("Send error did not release response routing")
	}
	require.ErrorIs(t, old.Context().Err(), context.Canceled)
}
