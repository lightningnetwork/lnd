package rpcwallet

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/watchonlyrpc"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// mockSCStream is a mock implementation of the
// watchonlyrpc.WatchOnly_SignCoordinatorStreamsServer stream interface.
type mockSCStream struct {
	mock.Mock

	// sendChan is used to simulate requests sent over the stream from the
	// sign coordinator to the remote signer.
	sendChan chan *watchonlyrpc.SignCoordinatorRequest

	// recvChan is used to simulate responses sent over the stream from the
	// remote signer to the sign coordinator.
	recvChan chan *watchonlyrpc.SignCoordinatorResponse

	// cancelChan is used to simulate a canceled stream.
	cancelChan chan struct{}
	cancel     sync.Once

	// Stream actions expose the send boundary without coordinator
	// internals.
	sendStarted chan *watchonlyrpc.SignCoordinatorRequest
	sendGate    <-chan struct{}

	ctx context.Context //nolint:containedctx
}

// newMockSCStream arranges one serialized reader and writer with the exact
// signing traffic required by the test. The final waiting Recv is optional
// because shutdown can win before the reader re-enters the transport.
func newMockSCStream(t *testing.T, sends, responses int) *mockSCStream {
	t.Helper()
	serverCtx, _ := remoteSignerTLSContexts(t)
	stream := &mockSCStream{
		sendChan:    make(chan *watchonlyrpc.SignCoordinatorRequest),
		recvChan:    make(chan *watchonlyrpc.SignCoordinatorResponse),
		cancelChan:  make(chan struct{}),
		sendStarted: make(chan *watchonlyrpc.SignCoordinatorRequest, 1),
		ctx:         serverCtx,
	}
	t.Cleanup(func() {
		stream.AssertExpectations(t)
	})

	// Context is an immutable transport accessor, read throughout Run and
	// by the test. Its call count is not a signing/lifecycle contract.
	stream.On("Context").Return(serverCtx)
	if sends > 0 {
		call := stream.On("Send", mock.Anything).Return(nil)
		if sends == 1 {
			call.Once()
		} else {
			call.Times(sends)
		}
		call.Run(func(args mock.Arguments) {
			req, _ := args.Get(0).(*watchonlyrpc.
				SignCoordinatorRequest)

			// Expose the old writer's entry while retaining
			// cancellation after the test releases a deliberately
			// stalled send.
			if req.GetRequestId() > handshakeRequestID {
				select {
				case stream.sendStarted <- req:

				default:
				}
				if stream.sendGate != nil {
					<-stream.sendGate
				}
			}
			var err error
			select {
			case <-stream.cancelChan:
				err = status.Error(codes.Unavailable,
					"stream closed before send")

			case <-serverCtx.Done():
				err = serverCtx.Err()

			case stream.sendChan <- req:
			}

			// This stream has one writer. Publish the selected
			// transport result through the expectation before
			// Called returns it.
			call.Return(err)
		})

		// A failed Send can reach the owner before the reader reports
		// disconnect, allowing a safe retry on this cancelled stream.
		// Only observed traffic has an exact count; these writes fail
		// immediately without reaching the simulated wire.
		cancelled := mock.MatchedBy(func(
			_ *watchonlyrpc.SignCoordinatorRequest) bool {

			select {
			case <-stream.cancelChan:
				return true

			default:
				return false
			}
		})
		stream.On("Send", cancelled).Return(status.Error(
			codes.Unavailable, "stream closed before send",
		)).Maybe()
	}
	if responses > 0 {
		for index, count := range []int{responses, 1} {
			call := stream.On("Recv").Return(nil, nil)
			switch {
			case index == 1:
				call.Maybe()

			case count == 1:
				call.Once()

			default:
				call.Times(count)
			}
			call.Run(func(mock.Arguments) {
				var response *RSResponse
				var err error
				select {
				case response = <-stream.recvChan:

				case <-serverCtx.Done():
					err = serverCtx.Err()

				case <-stream.cancelChan:
					err = ErrStreamCanceled
				}

				// Handshake and response reads are serialized,
				// so each invocation returns its own observed
				// channel result.
				call.Return(response, err)
			})
		}
	}

	return stream
}

// Send returns the transport result selected by the arranged wire behavior.
func (ms *mockSCStream) Send(req *watchonlyrpc.SignCoordinatorRequest) error {
	args := ms.Called(req)

	return args.Error(0)
}

// Recv returns the next arranged signer message or transport failure.
func (ms *mockSCStream) Recv() (*watchonlyrpc.SignCoordinatorResponse,
	error) {

	args := ms.Called()
	response, _ := args.Get(0).(*watchonlyrpc.SignCoordinatorResponse)

	return response, args.Error(1)
}

// RecvMsg rejects unexpected use of the generic gRPC receive path.
func (ms *mockSCStream) RecvMsg(msg any) error {
	args := ms.Called(msg)

	return args.Error(0)
}

// SendHeader requires an explicit expectation if a test uses gRPC headers.
func (ms *mockSCStream) SendHeader(header metadata.MD) error {
	args := ms.Called(header)

	return args.Error(0)
}

// SendMsg rejects unexpected use of the generic gRPC send path.
func (ms *mockSCStream) SendMsg(msg any) error {
	args := ms.Called(msg)

	return args.Error(0)
}

// SetHeader requires an explicit expectation if a test configures headers.
func (ms *mockSCStream) SetHeader(header metadata.MD) error {
	args := ms.Called(header)

	return args.Error(0)
}

// SetTrailer records trailer changes instead of silently accepting them.
func (ms *mockSCStream) SetTrailer(trailer metadata.MD) {
	ms.Called(trailer)
}

// Context returns the immutable TLS transport context from Arrange.
func (ms *mockSCStream) Context() context.Context {
	args := ms.Called()
	ctx, _ := args.Get(0).(context.Context)

	return ctx
}

// Compile-time checking keeps the fixture on the public stream interface.
var _ StreamServer = (*mockSCStream)(nil)

// Cancel closes the cancelChan to simulate a canceled stream.
func (ms *mockSCStream) Cancel() {
	ms.cancel.Do(func() { close(ms.cancelChan) })
}

// Helper function to simulate responses sent over the mock stream.
func (ms *mockSCStream) sendResponse(
	resp *watchonlyrpc.SignCoordinatorResponse) {

	ms.recvChan <- resp
}

// setupSignCoordinator sets up a new SignCoordinator instance with a mock
// stream to simulate communication with a remote signer. It also simulates the
// handshake between the sign coordinator and the remote signer.
func setupSignCoordinator(t *testing.T, sends, responses int) (*SignCoordinator,
	*mockSCStream, chan error) {

	coordinator := NewSignCoordinator(2*time.Second, 3*time.Second)

	// One owner joins the coordinator after test-owned gates are released.
	// Replacement streams must never register another shutdown callback.
	t.Cleanup(coordinator.Stop)
	stream, errChan := setupNewStream(t, coordinator, sends, responses)

	return coordinator, stream, errChan
}

// setupNewStream sets up a new mock stream to simulate a communication with a
// remote signer. It also simulates the handshake between the passed sign
// coordinator and the remote signer.
func setupNewStream(t *testing.T,
	coordinator *SignCoordinator, sends, responses int) (*mockSCStream,
	chan error) {

	stream := newMockSCStream(t, sends, responses)

	errChan := make(chan error, 1)
	go func() {
		err := coordinator.Run(stream)

		// Returning from the real gRPC handler cancels its stream;
		// model that lifecycle so Stop can join reader/writer IO.
		stream.Cancel()
		if err != nil {
			errChan <- err
		}
	}()

	version := watchonlyrpc.RegistrationVersion
	signReg := &watchonlyrpc.SignerRegistration{
		RegistrationChallenge: make([]byte, 32),
		RegistrationInfo:      version,
	}

	regType := &watchonlyrpc.SignCoordinatorResponse_SignerRegistration{
		SignerRegistration: signReg,
	}

	registrationMsg := &watchonlyrpc.SignCoordinatorResponse{
		RefRequestId:     1, // Request ID is always 1 for registration.
		SignResponseType: regType,
	}

	stream.sendResponse(registrationMsg)

	// Ensure that the sign coordinator responds with a registration
	// complete message.
	select {
	case req := <-stream.sendChan:
		require.Equal(t, uint64(1), req.GetRequestId())

		comp := req.GetRegistrationResponse().GetRegistrationComplete()
		require.NotNil(t, comp)

	case <-time.After(time.Second):
		require.Fail(
			t, "registration complete message not received",
		)
	}

	require.NoError(t, coordinator.WaitUntilConnected(t.Context()))

	return stream, errChan
}

// getRequest is a helper function to get a request that has been sent from
// the sign coordinator over the mock stream.
func getRequest(s *mockSCStream) (*watchonlyrpc.SignCoordinatorRequest,
	error) {

	select {
	case req := <-s.sendChan:
		return req, nil

	case <-time.After(time.Second):
		return nil, ErrRequestTimeout
	}
}

// pong completes a specific observed request, keeping response correlation
// visible at the transport boundary rather than inspecting pending maps.
func (ms *mockSCStream) pong(id uint64) {
	ms.sendResponse(&watchonlyrpc.SignCoordinatorResponse{
		RefRequestId: id,
		SignResponseType: &watchonlyrpc.SignCoordinatorResponse_Pong{
			Pong: true,
		},
	})
}

// TestPingRequests verifies an observed request completes through its response.
func TestPingRequests(t *testing.T) {
	t.Parallel()

	// Arrange an authenticated stream and one waiting public RPC.
	coordinator, stream, _ := setupSignCoordinator(t, 2, 2)

	// Act through Ping asynchronously so this test can drive its stream.
	result := make(chan fn.T2[bool, error], 1)
	go func() {
		pong, err := coordinator.Ping(t.Context(), time.Second)
		result <- fn.NewT2(pong, err)
	}()
	request, err := getRequest(stream)
	require.NoError(t, err)

	// Act by returning a pong for the observed request ID.
	stream.pong(request.GetRequestId())

	// Assert the public result and the reserved handshake ID boundary.
	require.Equal(t, uint64(2), request.GetRequestId())
	require.True(t, request.GetPing())
	resultPong, err := (<-result).Unpack()
	require.NoError(t, err)
	require.True(t, resultPong)
}

// TestConcurrentPingRequests verifies independent response correlation when the
// signer replies in a different order from request submission.
func TestConcurrentPingRequests(t *testing.T) {
	t.Parallel()

	// Arrange two requests with distinct observed IDs on one stream.
	coordinator, stream, _ := setupSignCoordinator(t, 3, 3)

	// Act through Ping asynchronously so this test can drive its stream.
	first := make(chan fn.T2[bool, error], 1)
	go func() {
		pong, err := coordinator.Ping(t.Context(), time.Second)
		first <- fn.NewT2(pong, err)
	}()
	request1, err := getRequest(stream)
	require.NoError(t, err)

	// Act through Ping asynchronously so this test can drive its stream.
	second := make(chan fn.T2[bool, error], 1)
	go func() {
		pong, err := coordinator.Ping(t.Context(), time.Second)
		second <- fn.NewT2(pong, err)
	}()
	request2, err := getRequest(stream)
	require.NoError(t, err)

	// Act by answering the second request before the first.
	stream.pong(request2.GetRequestId())

	// Assert only its caller completes, then finish the remaining request.
	secondPong, err := (<-second).Unpack()
	require.NoError(t, err)
	require.True(t, secondPong)
	select {
	case err := <-first:
		t.Fatalf("unanswered request completed: %v", err)

	default:
	}
	stream.pong(request1.GetRequestId())
	firstPong, err := (<-first).Unpack()
	require.NoError(t, err)
	require.True(t, firstPong)
}

// TestPingTimeout preserves the caller deadline while late responses are
// ignored.
func TestPingTimeout(t *testing.T) {
	t.Parallel()

	// Arrange a caller deadline shorter than the configured request
	// timeout.
	coordinator, stream, _ := setupSignCoordinator(t, 2, 2)

	// Act through Ping asynchronously so this test can drive its stream.
	result := make(chan fn.T2[bool, error], 1)
	go func() {
		pong, err := coordinator.Ping(t.Context(), 50*time.Millisecond)
		result <- fn.NewT2(pong, err)
	}()
	request, err := getRequest(stream)
	require.NoError(t, err)

	// Act by withholding the response until the caller's deadline expires.
	resultPong, err := (<-result).Unpack()
	stream.pong(request.GetRequestId())

	// Assert the caller deadline remains distinguishable from RPC timeout.
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.False(t, resultPong)
}

// TestConcurrentPingTimeout verifies one expired caller does not cancel
// another.
func TestConcurrentPingTimeout(t *testing.T) {
	t.Parallel()

	// Arrange one short caller and an independent outstanding request.
	coordinator, stream, _ := setupSignCoordinator(t, 3, 2)

	// Act through Ping asynchronously so this test can drive its stream.
	first := make(chan fn.T2[bool, error], 1)
	go func() {
		pong, err := coordinator.Ping(t.Context(), 50*time.Millisecond)
		first <- fn.NewT2(pong, err)
	}()
	_, err := getRequest(stream)
	require.NoError(t, err)

	// Act through Ping asynchronously so this test can drive its stream.
	second := make(chan fn.T2[bool, error], 1)
	go func() {
		pong, err := coordinator.Ping(t.Context(), time.Second)
		second <- fn.NewT2(pong, err)
	}()
	request, err := getRequest(stream)
	require.NoError(t, err)

	// Act by letting the first expire before answering the second.
	firstPong, firstErr := (<-first).Unpack()
	stream.pong(request.GetRequestId())

	// Assert only the intended caller expired and the other still succeeds.
	require.ErrorIs(t, firstErr, context.DeadlineExceeded)
	require.False(t, firstPong)
	secondPong, err := (<-second).Unpack()
	require.NoError(t, err)
	require.True(t, secondPong)
}

// TestIncorrectResponseRequestId verifies unknown responses cannot settle an
// RPC.
func TestIncorrectResponseRequestId(t *testing.T) {
	t.Parallel()

	// Arrange a caller that outlives the configured round-trip timeout.
	coordinator, stream, _ := setupSignCoordinator(t, 2, 2)

	// Act through Ping asynchronously so this test can drive its stream.
	result := make(chan fn.T2[bool, error], 1)
	go func() {
		pong, err := coordinator.Ping(t.Context(), 3*time.Second)
		result <- fn.NewT2(pong, err)
	}()
	_, err := getRequest(stream)
	require.NoError(t, err)

	// Act by supplying a response for a request that was never submitted.
	stream.pong(999)

	// Assert the real request times out rather than accepting that
	// response.
	resultPong, err := (<-result).Unpack()
	require.ErrorIs(t, err, ErrRequestTimeout)
	require.False(t, resultPong)
}

// TestSignerErrorResponse preserves signer-side application errors.
func TestSignerErrorResponse(t *testing.T) {
	t.Parallel()

	// Arrange an observed request whose backend will reject it.
	coordinator, stream, _ := setupSignCoordinator(t, 2, 2)

	// Act through Ping asynchronously so this test can drive its stream.
	result := make(chan fn.T2[bool, error], 1)
	go func() {
		pong, err := coordinator.Ping(t.Context(), time.Second)
		result <- fn.NewT2(pong, err)
	}()
	request, err := getRequest(stream)
	require.NoError(t, err)

	// Act through the wire error variant instead of invoking internal
	// handlers.
	stream.sendResponse(&watchonlyrpc.SignCoordinatorResponse{
		RefRequestId: request.GetRequestId(),
		SignResponseType: &watchonlyrpc.
			SignCoordinatorResponse_SignerError{
			SignerError: &watchonlyrpc.SignerError{
				Error: "mock error",
			},
		},
	})

	// Assert the caller sees the application error unchanged.
	resultPong, err := (<-result).Unpack()
	require.EqualError(t, err, "mock error")
	require.False(t, resultPong)
}

// TestStopCoordinator settles all outstanding requests and closes admission.
func TestStopCoordinator(t *testing.T) {
	t.Parallel()

	// Arrange two observed requests with no responses and a running stream.
	coordinator, stream, runErr := setupSignCoordinator(t, 3, 1)

	// Act through Ping asynchronously so this test can drive its stream.
	first := make(chan fn.T2[bool, error], 1)
	go func() {
		pong, err := coordinator.Ping(t.Context(), 3*time.Second)
		first <- fn.NewT2(pong, err)
	}()
	_, err := getRequest(stream)
	require.NoError(t, err)

	// Act through Ping asynchronously so this test can drive its stream.
	second := make(chan fn.T2[bool, error], 1)
	go func() {
		pong, err := coordinator.Ping(t.Context(), 3*time.Second)
		second <- fn.NewT2(pong, err)
	}()
	_, err = getRequest(stream)
	require.NoError(t, err)

	// Act by shutting down while both callers are waiting on signer
	// responses.
	coordinator.Stop()

	// Assert Run and both callers settle, and a later request is rejected.
	require.ErrorIs(t, <-runErr, ErrShuttingDown)
	firstPong, err := (<-first).Unpack()
	require.ErrorIs(t, err, ErrShuttingDown)
	require.False(t, firstPong)
	secondPong, err := (<-second).Unpack()
	require.ErrorIs(t, err, ErrShuttingDown)
	require.False(t, secondPong)
	pong, err := coordinator.Ping(t.Context(), time.Second)
	require.ErrorIs(t, err, ErrShuttingDown)
	require.False(t, pong)
}

// TestRemoteSignerDisconnects preserves the configured reconnect wait timeout.
func TestRemoteSignerDisconnects(t *testing.T) {
	t.Parallel()

	// Arrange an in-flight safe RPC with a caller timeout beyond reconnect
	// waiting.
	coordinator, stream, runErr := setupSignCoordinator(t, 2, 1)

	// Act through Ping asynchronously so this test can drive its stream.
	result := make(chan fn.T2[bool, error], 1)
	go func() {
		pong, err := coordinator.Ping(t.Context(), 5*time.Second)
		result <- fn.NewT2(pong, err)
	}()
	_, err := getRequest(stream)
	require.NoError(t, err)

	// Act by disconnecting without providing a replacement signer.
	stream.Cancel()

	// Assert the stream closes and the caller reports connection timeout.
	require.ErrorIs(t, <-runErr, ErrStreamCanceled)
	resultPong, err := (<-result).Unpack()
	require.ErrorIs(t, err, ErrConnectTimeout)
	require.False(t, resultPong)
}

// TestWaitUntilConnectedNoTimeout preserves indefinite startup waiting at zero.
func TestWaitUntilConnectedNoTimeout(t *testing.T) {
	t.Parallel()

	// Arrange a disconnected coordinator with no internal connection
	// deadline.
	coordinator := NewSignCoordinator(time.Second, 0)
	t.Cleanup(coordinator.Stop)
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- coordinator.WaitUntilConnected(ctx)
	}()

	// Act by leaving startup pending, then explicitly canceling its caller.
	select {
	case err := <-result:
		t.Fatalf("startup returned before connection or "+
			"cancellation: %v", err)

	case <-time.After(50 * time.Millisecond):
	}
	cancel()

	// Assert zero startup timeout still honors explicit cancellation.
	require.ErrorIs(t, <-result, context.Canceled)
}

// TestRemoteSignerReconnectsDuringResponseWait retries a safe outstanding RPC.
func TestRemoteSignerReconnectsDuringResponseWait(t *testing.T) {
	t.Parallel()

	// Arrange a request already sent but not yet answered by the signer.
	coordinator, stream, runErr := setupSignCoordinator(t, 2, 1)

	// Act through Ping asynchronously so this test can drive its stream.
	result := make(chan fn.T2[bool, error], 1)
	go func() {
		pong, err := coordinator.Ping(t.Context(), 3*time.Second)
		result <- fn.NewT2(pong, err)
	}()
	original, err := getRequest(stream)
	require.NoError(t, err)

	// Act by closing that generation and completing the retry on its
	// replacement.
	stream.Cancel()
	require.ErrorIs(t, <-runErr, ErrStreamCanceled)
	replacement, _ := setupNewStream(t, coordinator, 2, 2)
	resent, err := getRequest(replacement)
	require.NoError(t, err)
	replacement.pong(resent.GetRequestId())

	// Assert retry gets a new ID and the original caller receives its
	// response.
	require.Equal(t, original.GetRequestId()+1, resent.GetRequestId())
	require.True(t, resent.GetPing())
	resultPong, err := (<-result).Unpack()
	require.NoError(t, err)
	require.True(t, resultPong)
}

// TestMuSig2RequestDoesNotRetryOnReconnect preserves stateful single-attempt
// calls.
func TestMuSig2RequestDoesNotRetryOnReconnect(t *testing.T) {
	t.Parallel()

	// Arrange a stateful MuSig2 call whose signer response will be lost.
	coordinator, stream, runErr := setupSignCoordinator(t, 2, 1)
	result := make(chan error, 1)
	go func() {
		_, err := coordinator.MuSig2CreateSession(
			t.Context(), &signrpc.MuSig2SessionRequest{},
		)
		result <- err
	}()
	request, err := getRequest(stream)
	require.NoError(t, err)
	require.NotNil(t, request.GetMuSig2SessionRequest())

	// Act by disconnecting, then bringing a new signer stream online.
	stream.Cancel()
	require.ErrorIs(t, <-runErr, ErrStreamCanceled)
	replacement, _ := setupNewStream(t, coordinator, 2, 2)

	// Assert the original call failed without replay; a control Ping must
	// be the next request the new signer observes.
	require.ErrorIs(t, <-result, ErrNotConnected)

	// Act through Ping asynchronously so this test can drive its stream.
	control := make(chan fn.T2[bool, error], 1)
	go func() {
		pong, err := coordinator.Ping(t.Context(), time.Second)
		control <- fn.NewT2(pong, err)
	}()
	request, err = getRequest(replacement)
	require.NoError(t, err)
	require.True(t, request.GetPing())
	replacement.pong(request.GetRequestId())
	controlPong, err := (<-control).Unpack()
	require.NoError(t, err)
	require.True(t, controlPong)
}

// TestRemoteSignerDisconnectsMidSend covers cancellation while Send is blocked.
func TestRemoteSignerDisconnectsMidSend(t *testing.T) {
	t.Parallel()

	// Arrange a stream whose transport Send has started but cannot yet
	// write.
	coordinator, stream, runErr := setupSignCoordinator(t, 2, 1)

	// Act through Ping asynchronously so this test can drive its stream.
	result := make(chan fn.T2[bool, error], 1)
	go func() {
		pong, err := coordinator.Ping(t.Context(), 3*time.Second)
		result <- fn.NewT2(pong, err)
	}()
	original := <-stream.sendStarted

	// Act by interrupting that Send and reconnecting before answering the
	// RPC.
	stream.Cancel()
	require.ErrorIs(t, <-runErr, ErrStreamCanceled)
	replacement, _ := setupNewStream(t, coordinator, 2, 2)
	resent, err := getRequest(replacement)
	require.NoError(t, err)
	replacement.pong(resent.GetRequestId())

	// Assert the safe request is retried on a fresh generation and ID.
	require.Greater(t, resent.GetRequestId(), original.GetRequestId())
	resultPong, err := (<-result).Unpack()
	require.NoError(t, err)
	require.True(t, resultPong)
}

// TestRemoteSignerReconnectsBeforeSend ignores late sends from an old
// generation.
func TestRemoteSignerReconnectsBeforeSend(t *testing.T) {
	t.Parallel()

	// Arrange a gate before bytes leave the old stream. Cleanup releases it
	// before joining the coordinator if any assertion aborts the test.
	coordinator, stream, runErr := setupSignCoordinator(t, 2, 1)
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	stream.sendGate = gate

	// Act through Ping asynchronously so this test can drive its stream.
	result := make(chan fn.T2[bool, error], 1)
	go func() {
		pong, err := coordinator.Ping(t.Context(), 3*time.Second)
		result <- fn.NewT2(pong, err)
	}()
	original := <-stream.sendStarted

	// Act by replacing the generation while its old Send remains blocked.
	stream.Cancel()
	require.ErrorIs(t, <-runErr, ErrStreamCanceled)
	replacement, _ := setupNewStream(t, coordinator, 3, 3)
	resent, err := getRequest(replacement)
	require.NoError(t, err)
	replacement.pong(resent.GetRequestId())
	release()

	// Assert the old completion cannot corrupt the replacement's request
	// state.
	require.Greater(t, resent.GetRequestId(), original.GetRequestId())
	resultPong, err := (<-result).Unpack()
	require.NoError(t, err)
	require.True(t, resultPong)

	// Act through Ping asynchronously so this test can drive its stream.
	control := make(chan fn.T2[bool, error], 1)
	go func() {
		pong, err := coordinator.Ping(t.Context(), time.Second)
		control <- fn.NewT2(pong, err)
	}()
	request, err := getRequest(replacement)
	require.NoError(t, err)
	replacement.pong(request.GetRequestId())
	controlPong, err := (<-control).Unpack()
	require.NoError(t, err)
	require.True(t, controlPong)
}

// TestRemoteSignerSecondRun rejects a competing stream during handshake.
func TestRemoteSignerSecondRun(t *testing.T) {
	t.Parallel()

	// Arrange an admitted stream whose registration-complete Send is
	// blocked.
	coordinator := NewSignCoordinator(time.Second, time.Second)
	t.Cleanup(coordinator.Stop)
	stream := newMockSCStream(t, 1, 1)
	first := make(chan error, 1)
	go func() {
		first <- coordinator.Run(stream)
		stream.Cancel()
	}()
	version := watchonlyrpc.RegistrationVersion
	stream.recvChan <- &watchonlyrpc.SignCoordinatorResponse{
		RefRequestId: handshakeRequestID,
		SignResponseType: &RSRegistration{
			SignerRegistration: &watchonlyrpc.SignerRegistration{
				RegistrationChallenge: make([]byte, 32),
				RegistrationInfo:      version,
			},
		},
	}

	// Act with another stream while the first handshake is still active.
	err := coordinator.Run(newMockSCStream(t, 0, 0))

	// Assert exclusive admission and then complete/close the original
	// stream.
	require.ErrorIs(t, err, ErrMultipleConnections)
	_, err = getRequest(stream)
	require.NoError(t, err)
	stream.Cancel()
	require.ErrorIs(t, <-first, ErrStreamCanceled)
}

// TestRemoteSignerHandshakeDeadline preserves the stream context cause when
// handshake IO cannot be admitted after its deadline.
func TestRemoteSignerHandshakeDeadline(t *testing.T) {
	// Arrange admission on a live TLS context, then expose expiry at the
	// handshake boundary. Ordered mock returns model a deadline crossed
	// after admission without racing the owner against a closed context.
	coordinator := NewSignCoordinator(time.Second, time.Second)
	t.Cleanup(coordinator.Stop)
	tlsCtx, _ := remoteSignerTLSContexts(t)
	ctx, cancel := context.WithDeadline(
		tlsCtx, time.Now().Add(-time.Second),
	)
	defer cancel()
	stream := &mockSCStream{}
	stream.On("Context").Return(tlsCtx).Once()
	stream.On("Context").Return(ctx).Once()
	t.Cleanup(func() {
		stream.AssertExpectations(t)
	})

	// Act through the public handler. Admission must succeed first, then
	// handshake IO admission observes the expired context and cannot run.
	err := coordinator.Run(stream)

	// Assert deadline expiry remains distinguishable from client shutdown.
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
