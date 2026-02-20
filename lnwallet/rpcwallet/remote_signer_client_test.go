package rpcwallet

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
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

// mockStreamFeeder is a mock implementation of SignCoordinatorStreamFeeder.
type mockStreamFeeder struct {
	// stream is the current mock stream instance that gets set when the
	// GetStream execution is successful.
	stream *mockStream

	// streamShouldFail is a boolean that indicates if the stream should
	// fail when GetStream is called.
	streamShouldFail bool

	// streamCreated is a channel that is used to signal when the stream has
	// been created. If the stream creation fails, an error is sent over the
	// channel instead.
	streamCreated chan error

	quit chan struct{}

	mu sync.Mutex
}

// newMockStreamFeeder creates a new mock stream feeder instance. If
// getStreamShouldFail is set to true, the GetStream method will fail and return
// an error when executed, until the SetStreamFailure method is called to change
// the behavior.
func newMockStreamFeeder(getStreamShouldFail bool) *mockStreamFeeder {
	return &mockStreamFeeder{
		streamCreated:    make(chan error),
		quit:             make(chan struct{}),
		streamShouldFail: getStreamShouldFail,
	}
}

// GetStream returns a mock stream instance. If the stream creation fails, an
// error is returned instead.
func (msf *mockStreamFeeder) GetStream(ctx context.Context) (
	*Stream, error) {

	msf.mu.Lock()

	select {
	case <-msf.quit:
		msf.mu.Unlock()
		return nil, ErrShuttingDown
	default:
	}

	// If we've configured the stream feeder to fail, we'll fail the stream
	// creation and return an error.
	if msf.streamShouldFail {
		msf.mu.Unlock()

		// Signal that the stream creation has failed.
		select {
		case msf.streamCreated <- ErrStreamError:
		case <-ctx.Done():
		case <-msf.quit:
		}

		return nil, ErrStreamError
	}

	// Wrap the context in a cancelable context, so that the stream will be
	// canceled when either party cancels to the context.
	// If cancel function is executed, that simulates that the stream was
	// cancelled by the other party (i.e. the watch-only node).
	// If the parent context is cancelled, the remote signer client has
	// cancelled the stream.
	ctxc, cancel := context.WithCancel(ctx)

	// Else create a new mock stream instance.
	mStream := newMockStream(ctxc)

	msf.stream = mStream

	// cancel the context on the closure of the stream.
	closeFunc := func() error {
		cancel()

		return nil
	}

	returnStream := NewStream(msf.stream, closeFunc)

	msf.mu.Unlock()

	// Signal that the stream creation has succeeded.
	select {
	case msf.streamCreated <- nil:
	case <-ctxc.Done():
	case <-msf.quit:
	}

	return returnStream, nil
}

// SetStreamFailure sets the streamShouldFail boolean to the provided value.
// If set to true, the GetStream method will fail and return an error when
// executed. If set to false, the GetStream method will succeed and return a
// mock stream instance.
func (msf *mockStreamFeeder) SetStreamFailure(shouldFail bool) {
	msf.mu.Lock()
	defer msf.mu.Unlock()

	msf.streamShouldFail = shouldFail
}

// GetStreamShouldFail returns the current value of the streamShouldFail
// boolean.
func (msf *mockStreamFeeder) GetStreamShouldFail() bool {
	msf.mu.Lock()
	defer msf.mu.Unlock()

	return msf.streamShouldFail
}

// Stop signals the mock stream feeder to stop.
func (msf *mockStreamFeeder) Stop() {
	close(msf.quit)
}

// A compile time assertion to ensure mockStreamFeeder meets the
// SignCoordinatorStreamFeeder interface.
var _ SignCoordinatorStreamFeeder = (*mockStreamFeeder)(nil)

// Mock implementation of a stream.
type mockStream struct {
	sendChan chan *walletrpc.SignCoordinatorResponse
	recvChan chan *walletrpc.SignCoordinatorRequest

	// recvErrChan can be used to simulate that the stream errors.
	recvErrChan chan error

	// ctx is the context that the stream was created with.
	ctx context.Context //nolint:containedctx
}

// newMockStream creates a new mock stream instance.
// The second return value is a cancel function that can be used to cancel the
// stream.
func newMockStream(ctx context.Context) *mockStream {
	return &mockStream{
		sendChan:    make(chan *walletrpc.SignCoordinatorResponse),
		recvChan:    make(chan *walletrpc.SignCoordinatorRequest),
		recvErrChan: make(chan error),
		ctx:         ctx,
	}
}

// Send sends a response over the mock stream. This is called by the remote
// signer client when it responds to a request.
func (ms *mockStream) Send(resp *walletrpc.SignCoordinatorResponse) error {
	select {
	case <-ms.ctx.Done():
		// If the context is canceled, we return an error to indicate
		// that the stream has been canceled.
		return ErrStreamCanceled
	case ms.sendChan <- resp:
	}

	return nil
}

// Recv simulates that a request over is sent over the mock stream to the
// remote signer client. If a request is sent over the recvChan, the remote
// signer client will handle the request. If an error is sent over the
// recvErrChan channel, the error will be received by the remote signer client.
func (ms *mockStream) Recv() (*walletrpc.SignCoordinatorRequest, error) {
	select {
	case resp := <-ms.recvChan:
		return resp, nil
	case err := <-ms.recvErrChan:
		return nil, err
	case <-ms.ctx.Done():
		// If the context is canceled, we return an error to indicate
		// that the stream has been canceled.
		return nil, ErrStreamCanceled
	}
}

// Helper function to simulate requests sent over the mock stream.
// The function will return an error if the stream is canceled before the
// request is received.
func (ms *mockStream) recvRequest(req *walletrpc.SignCoordinatorRequest) error {
	select {
	case ms.recvChan <- req:
		return nil
	case <-ms.ctx.Done():
		return ErrStreamCanceled
	}
}

// Helper function to simulate that the stream errors.
// The function will return an error if the stream is canceled before the error
// is received.
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
	var resp *walletrpc.SignCoordinatorResponse

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

	complete := &walletrpc.RegistrationResponse_RegistrationComplete{
		RegistrationComplete: &walletrpc.RegistrationComplete{
			Signature:        "",
			RegistrationInfo: "watch-only registration info",
		},
	}

	rType := &walletrpc.SignCoordinatorRequest_RegistrationResponse{
		RegistrationResponse: &walletrpc.RegistrationResponse{
			RegistrationResponseType: complete,
		},
	}

	// Send a message to the client to simulate that the watch-only node has
	// accepted the registration and that it's completed.
	regCompleteMsg := &walletrpc.SignCoordinatorRequest{
		RequestId:       handshakeRequestID,
		SignRequestType: rType,
	}

	return ms.recvRequest(regCompleteMsg)
}

// Mock implementations of various WalletKit_SignCoordinatorStreamsClient
// methods.
func (ms *mockStream) Header() (metadata.MD, error) { return nil, nil }
func (ms *mockStream) SendMsg(m any) error          { return nil }
func (ms *mockStream) Trailer() metadata.MD         { return nil }
func (ms *mockStream) CloseSend() error             { return nil }
func (ms *mockStream) RecvMsg(m any) error          { return nil }
func (ms *mockStream) Context() context.Context     { return ms.ctx }

// newTestRemoteSignerClient creates a new outbound remote signer client
// instance for testing purposes, and inserts the passed streamFeeder together
// with a mock sub-servers into the created instance.
func newTestRemoteSignerClient(t *testing.T,
	streamFeeder *mockStreamFeeder) *OutboundClient {

	client, err := NewOutboundClient(
		&mockWalletKitServer{}, &mockSignerServer{}, streamFeeder,
		1*time.Second,
	)
	require.NoError(t, err)
	require.NoError(t, client.Start(t.Context()))

	// We expect the remote signer client attempt to create a stream during
	// the start up. So if the stream feeder is configured to succeed, we
	// need to handle the handshake procedure to finalize the stream set up.
	if !streamFeeder.GetStreamShouldFail() {
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

	mockFeeder := newMockStreamFeeder(false)

	client := newTestRemoteSignerClient(t, mockFeeder)
	defer func() {
		// Ensure that the remote signer client is stopped successfully
		// after the test.
		require.NoError(t, client.Stop())
	}()

	// create the ping request.
	pingReq := &walletrpc.SignCoordinatorRequest_Ping{
		Ping: true,
	}

	requestID := uint64(2)

	req := &walletrpc.SignCoordinatorRequest{
		RequestId:       requestID,
		SignRequestType: pingReq,
	}

	// Send the request to the remote signer client.
	err := mockFeeder.stream.recvRequest(req)
	require.NoError(t, err)

	// Wait for the response from the remote signer client.
	resp := <-mockFeeder.stream.sendChan

	// Ensure that the response contains the correct request ID and that
	// it's a pong response.
	require.Equal(t, requestID, resp.GetRefRequestId())
	require.True(t, resp.GetPong())
}

// TestMultiplePingResponses tests that we can send multiple ping requests to
// the remote signer client, and that it will respond with a pong for each
// request.
func TestMultiplePingResponses(t *testing.T) {
	t.Parallel()

	mockFeeder := newMockStreamFeeder(false)

	client := newTestRemoteSignerClient(t, mockFeeder)
	defer func() {
		// Ensure that the remote signer client is stopped successfully
		// after the test.
		require.NoError(t, client.Stop())
	}()

	// Create the first ping request.
	pingReq := &walletrpc.SignCoordinatorRequest_Ping{
		Ping: true,
	}

	requestID1 := uint64(2)

	req1 := &walletrpc.SignCoordinatorRequest{
		RequestId:       requestID1,
		SignRequestType: pingReq,
	}

	// Send the first request to the remote signer client.
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

	req2 := &walletrpc.SignCoordinatorRequest{
		RequestId:       requestID2,
		SignRequestType: pingReq,
	}

	// Send the second request to the remote signer client.
	err = mockFeeder.stream.recvRequest(req2)
	require.NoError(t, err)

	// Wait for the second response from the remote signer client.
	resp2 := <-mockFeeder.stream.sendChan

	// Ensure that the response contains the correct request ID, which
	// differs from the first request, and that it's a pong response.
	require.Equal(t, requestID2, resp2.GetRefRequestId())
	require.True(t, resp2.GetPong())
}

// TestStreamRecvErrorHandling tests that the remote signer client will cancel
// the stream if an error is received over the stream.Recv() method.
// The remote signer client should then proceed to retry to create a new stream.
func TestStreamRecvErrorHandling(t *testing.T) {
	t.Parallel()

	msf := newMockStreamFeeder(false)

	client := newTestRemoteSignerClient(t, msf)
	defer func() {
		// Ensure that the remote signer client is stopped successfully
		// after the test.
		require.NoError(t, client.Stop())
	}()

	// Fetch the stream context before the stream is canceled.
	streamCtx := msf.stream.Context()

	// Simulate that the stream errors, which should cause the remote signer
	// client to cancel the stream.
	err := msf.stream.recvErr(ErrStreamCanceled)
	require.NoError(t, err)

	// Ensure that the stream has been canceled, as that should cause the
	// remote signer client to cancel the stream context.
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

	msf := newMockStreamFeeder(false)

	client := newTestRemoteSignerClient(t, msf)
	defer func() {
		// Ensure that the remote signer client is stopped successfully
		// after the test.
		require.NoError(t, client.Stop())
	}()

	// Create a SignMessage request. As the remote signer client has an
	// mockSignerServer instance as the signrpc server, this request will
	// thrown an error when the signer server processes it.
	signMessageReq := &walletrpc.SignCoordinatorRequest_SignMessageReq{
		SignMessageReq: &signrpc.SignMessageReq{},
	}

	requestID := uint64(2)

	req := &walletrpc.SignCoordinatorRequest{
		RequestId:       requestID,
		SignRequestType: signMessageReq,
	}

	// Send the request to the remote signer client.
	err := msf.stream.recvRequest(req)
	require.NoError(t, err)

	// Wait for the response from the remote signer client.
	resp := <-msf.stream.sendChan

	// Ensure that the response contains the correct request ID.
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

	msf := newMockStreamFeeder(true)

	client := newTestRemoteSignerClient(t, msf)
	defer func() {
		// Ensure that the remote signer client is stopped successfully
		// after the test.
		require.NoError(t, client.Stop())
	}()

	// For testing purposes, we set the max retry timeout to a value that
	// ensures that the retry timeout will be capped on the fourth backoff.
	client.maxRetryTimeout = defaultRetryTimeout * 3

	// As we passed false to the newMockStreamFeeder constructor, the stream
	// creation should fail.
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

	// Verify that the retry backoff is within the expected range. We allow
	// a small margin of error (100ms) on the range bound to account for the
	// time it takes to execute the test code. We also allow a small margin
	// of error (10ms) on the lower bound to account for the time the code
	// execution takes between the creation of the stream, and when
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

	// For the next retry, we want the stream creation to succeed. This will
	// reset the retry backoff to the default value, once the stream is
	// successfully created.
	msf.SetStreamFailure(false)

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
	signrpc.UnimplementedSignerServer
}

var _ signrpc.SignerServer = (*mockSignerServer)(nil)

func (m *mockSignerServer) SignMessage(_ context.Context,
	_ *signrpc.SignMessageReq) (*signrpc.SignMessageResp, error) {

	return nil, ErrMockResponseErr
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
