package rpcwallet

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// mockSCStream is a mock implementation of the
// walletrpc.WalletKit_SignCoordinatorStreamsServer stream interface.
type mockSCStream struct {
	// sendChan is used to simulate requests sent over the stream from the
	// sign coordinator to the remote signer.
	sendChan chan *walletrpc.SignCoordinatorRequest

	// sendErrorChan is used to simulate requests sent over the stream from
	// the sign coordinator to the remote signer.
	sendErrorChan chan error

	// recvChan is used to simulate responses sent over the stream from the
	// remote signer to the sign coordinator.
	recvChan chan *walletrpc.SignCoordinatorResponse

	// cancelChan is used to simulate a canceled stream.
	cancelChan chan struct{}

	ctx context.Context //nolint:containedctx
}

// newMockSCStream creates a new mock stream.
func newMockSCStream() *mockSCStream {
	return &mockSCStream{
		sendChan:      make(chan *walletrpc.SignCoordinatorRequest),
		sendErrorChan: make(chan error),
		recvChan:      make(chan *walletrpc.SignCoordinatorResponse, 1),
		cancelChan:    make(chan struct{}),
		ctx:           context.Background(),
	}
}

// Send simulates a sent request from the sign coordinator to the remote signer
// over the mock stream.
func (ms *mockSCStream) Send(req *walletrpc.SignCoordinatorRequest) error {
	select {
	case ms.sendChan <- req:
		return nil

	case err := <-ms.sendErrorChan:
		return err
	}
}

// Recv simulates a received response from the remote signer to the sign
// coordinator over the mock stream.
func (ms *mockSCStream) Recv() (*walletrpc.SignCoordinatorResponse, error) {
	select {
	case resp := <-ms.recvChan:
		return resp, nil

	case <-ms.cancelChan:
		// To simulate a canceled stream, we return an error when the
		// cancelChan is closed.
		return nil, ErrStreamCanceled
	}
}

// Mock implementations of various WalletKit_SignCoordinatorStreamsServer
// methods.
func (ms *mockSCStream) RecvMsg(msg any) error        { return nil }
func (ms *mockSCStream) SendHeader(metadata.MD) error { return nil }
func (ms *mockSCStream) SendMsg(m any) error          { return nil }
func (ms *mockSCStream) SetHeader(metadata.MD) error  { return nil }
func (ms *mockSCStream) SetTrailer(metadata.MD)       {}

// Context returns the context of the mock stream.
func (ms *mockSCStream) Context() context.Context {
	return ms.ctx
}

// Cancel closes the cancelChan to simulate a canceled stream.
func (ms *mockSCStream) Cancel() {
	close(ms.cancelChan)
}

// Helper function to simulate responses sent over the mock stream.
func (ms *mockSCStream) sendResponse(resp *walletrpc.SignCoordinatorResponse) {
	ms.recvChan <- resp
}

// setupSignCoordinator sets up a new SignCoordinator instance with a mock
// stream to simulate communication with a remote signer. It also simulates the
// handshake between the sign coordinator and the remote signer.
func setupSignCoordinator(t *testing.T) (*SignCoordinator, *mockSCStream,
	chan error) {

	coordinator := NewSignCoordinator(2*time.Second, 3*time.Second)
	stream, errChan := setupNewStream(t, coordinator)

	return coordinator, stream, errChan
}

// setupNewStream sets up a new mock stream to simulate a communication with a
// remote signer. It also simulates the handshake between the passed sign
// coordinator and the remote signer.
func setupNewStream(t *testing.T,
	coordinator *SignCoordinator) (*mockSCStream, chan error) {

	stream := newMockSCStream()

	errChan := make(chan error)
	go func() {
		err := coordinator.Run(stream)
		if err != nil {
			errChan <- err
		}
	}()

	signReg := &walletrpc.SignerRegistration{
		RegistrationChallenge: "registrationChallenge",
		RegistrationInfo:      "outboundSigner",
	}

	regType := &walletrpc.SignCoordinatorResponse_SignerRegistration{
		SignerRegistration: signReg,
	}

	registrationMsg := &walletrpc.SignCoordinatorResponse{
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

	return stream, errChan
}

// getRequest is a helper function to get a request that has been sent from
// the sign coordinator over the mock stream.
func getRequest(s *mockSCStream) (*walletrpc.SignCoordinatorRequest, error) {
	select {
	case req := <-s.sendChan:
		return req, nil

	case <-time.After(time.Second):
		return nil, ErrRequestTimeout
	}
}

// TestPingRequests tests that the sign coordinator correctly sends a Ping
// request to the remote signer and handles the received Pong response
// correctly.
func TestPingRequests(t *testing.T) {
	t.Parallel()

	coordinator, stream, _ := setupSignCoordinator(t)

	var wg sync.WaitGroup

	// Send a Ping request in a goroutine so that we can pick up the request
	// sent over the mock stream, and respond accordingly.
	wg.Add(1)

	go func() {
		defer wg.Done()

		// The Ping method will return true if the response is a Pong
		// response.
		success, err := coordinator.Ping(t.Context(), 2*time.Second)
		require.NoError(t, err)
		require.True(t, success)
	}()

	// Get the request sent over the mock stream.
	req, err := getRequest(stream)
	require.NoError(t, err)

	// Verify that the request has the expected request ID and that it's a
	// Ping request.
	require.Equal(t, uint64(2), req.GetRequestId())
	require.True(t, req.GetPing())

	// Verify that the coordinator has correctly set up a single response
	// channel for the Ping request with the specific request ID.
	require.Equal(t, coordinator.responses.Len(), 1)
	_, ok := coordinator.responses.Load(uint64(2))
	require.True(t, ok)

	// Now we simulate the response from the remote signer by sending a Pong
	// response over the mock stream.
	stream.sendResponse(&walletrpc.SignCoordinatorResponse{
		RefRequestId: 2,
		SignResponseType: &walletrpc.SignCoordinatorResponse_Pong{
			Pong: true,
		},
	})

	// Wait for the goroutines to finish, which should only happen after the
	// requests have had their expected responses processed.
	wg.Wait()

	// Verify the responses map is empty after all responses are received
	// to ensure that no memory leaks occur.
	require.Equal(t, coordinator.responses.Len(), 0)
}

// TestConcurrentPingRequests tests that the sign coordinator correctly handles
// concurrent Ping requests and responses, and that the order in which responses
// are sent back over the stream doesn't matter.
func TestConcurrentPingRequests(t *testing.T) {
	t.Parallel()

	coordinator, stream, _ := setupSignCoordinator(t)

	var wg sync.WaitGroup

	// Let's first start by sending two concurrent Ping requests and sending
	// the respective responses back in order.
	wg.Add(1)

	go func() {
		defer wg.Done()
		success, err := coordinator.Ping(t.Context(), 2*time.Second)

		require.NoError(t, err)
		require.True(t, success)
	}()

	// Get the first request sent over the mock stream.
	req1, err := getRequest(stream)
	require.NoError(t, err)

	// Verify that the request has the expected request ID and that it's a
	// Ping request.
	require.Equal(t, uint64(2), req1.GetRequestId())
	require.True(t, req1.GetPing())

	// Verify that the coordinator has correctly set up a single response
	// channel for the Ping request with the specific request ID.
	require.Equal(t, coordinator.responses.Len(), 1)
	_, ok := coordinator.responses.Load(uint64(2))
	require.True(t, ok)

	// Now we send the second Ping request.
	wg.Add(1)

	go func() {
		defer wg.Done()
		success, err := coordinator.Ping(t.Context(), 2*time.Second)

		require.NoError(t, err)
		require.True(t, success)
	}()

	// Get the second request sent over the mock stream.
	req2, err := getRequest(stream)
	require.NoError(t, err)

	// Verify that the request has the expected request ID and that it's a
	// Ping request.
	require.Equal(t, uint64(3), req2.GetRequestId())
	require.True(t, req2.GetPing())

	// Verify that the coordinator has correctly set up two response
	// channels for the Ping requests with their specific request IDs.
	require.Equal(t, coordinator.responses.Len(), 2)
	_, ok = coordinator.responses.Load(uint64(3))
	require.True(t, ok)

	// Send responses for both Ping requests in order.
	stream.sendResponse(&walletrpc.SignCoordinatorResponse{
		RefRequestId: 2,
		SignResponseType: &walletrpc.SignCoordinatorResponse_Pong{
			Pong: true,
		},
	})
	stream.sendResponse(&walletrpc.SignCoordinatorResponse{
		RefRequestId: 3,
		SignResponseType: &walletrpc.SignCoordinatorResponse_Pong{
			Pong: true,
		},
	})

	// Wait for the goroutines to finish, which should only happen after the
	// requests have had their expected responses processed.
	wg.Wait()

	// Verify the responses map is empty after all responses are received.
	require.Equal(t, coordinator.responses.Len(), 0)

	// Now let's verify that the sign coordinator can correctly process
	// responses that are sent back in a different order than the requests
	// were sent.

	// Send a new set of concurrent Ping requests.
	wg.Add(1)

	go func() {
		defer wg.Done()

		success, err := coordinator.Ping(t.Context(), 2*time.Second)
		require.NoError(t, err)
		require.True(t, success)
	}()

	req3, err := getRequest(stream)
	require.NoError(t, err)

	require.Equal(t, uint64(4), req3.GetRequestId())
	require.True(t, req3.GetPing())

	// Verify that the coordinator has removed the response channels for the
	// previous Ping requests and set up a new one for the new request.
	require.Equal(t, coordinator.responses.Len(), 1)
	_, ok = coordinator.responses.Load(uint64(4))
	require.True(t, ok)

	wg.Add(1)

	go func() {
		defer wg.Done()

		success, err := coordinator.Ping(t.Context(), 2*time.Second)
		require.NoError(t, err)
		require.True(t, success)
	}()

	req4, err := getRequest(stream)
	require.NoError(t, err)

	require.Equal(t, uint64(5), req4.GetRequestId())
	require.True(t, req4.GetPing())

	require.Equal(t, coordinator.responses.Len(), 2)
	_, ok = coordinator.responses.Load(uint64(5))
	require.True(t, ok)

	// Send the responses back in reverse order.
	stream.sendResponse(&walletrpc.SignCoordinatorResponse{
		RefRequestId: 5,
		SignResponseType: &walletrpc.SignCoordinatorResponse_Pong{
			Pong: true,
		},
	})
	stream.sendResponse(&walletrpc.SignCoordinatorResponse{
		RefRequestId: 4,
		SignResponseType: &walletrpc.SignCoordinatorResponse_Pong{
			Pong: true,
		},
	})

	// Wait for the goroutines to finish, which should only happen after the
	// requests have had their expected responses processed.
	wg.Wait()

	// Verify the responses map is empty after all responses are received
	// to ensure that no memory leaks occur.
	require.Equal(t, coordinator.responses.Len(), 0)
}

// TestPingTimeout tests that the sign coordinator correctly handles a Ping
// request that times out.
func TestPingTimeout(t *testing.T) {
	t.Parallel()

	coordinator, stream, _ := setupSignCoordinator(t)

	var wg sync.WaitGroup

	// Simulate a Ping request that is expected to time out.
	wg.Add(1)

	go func() {
		defer wg.Done()

		// Note that the timeout is set to 1 second.
		success, err := coordinator.Ping(t.Context(), 1*time.Second)
		require.Equal(t, context.DeadlineExceeded, err)
		require.False(t, success)
	}()

	// Get the request sent over the mock stream.
	req1, err := getRequest(stream)
	require.NoError(t, err)

	// Verify that the request has the expected request ID and that it's a
	// Ping request.
	require.Equal(t, uint64(2), req1.GetRequestId())
	require.True(t, req1.GetPing())

	// Verify that the coordinator has correctly set up a single response
	// channel for the Ping request with the specific request ID.
	require.Equal(t, coordinator.responses.Len(), 1)
	_, ok := coordinator.responses.Load(uint64(2))
	require.True(t, ok)

	// Now wait for the request to time out.
	wg.Wait()

	// Verify that the responses map is empty after the timeout.
	require.Equal(t, coordinator.responses.Len(), 0)

	// Now let's simulate that the response is sent back after the request
	// has timed out.
	stream.sendResponse(&walletrpc.SignCoordinatorResponse{
		RefRequestId: 2,
		SignResponseType: &walletrpc.SignCoordinatorResponse_Pong{
			Pong: true,
		},
	})

	// Verify that the responses map still remains empty, as responses for
	// timed out requests are ignored.
	require.Equal(t, coordinator.responses.Len(), 0)
}

// TestConcurrentPingTimeout tests that the sign coordinator correctly handles a
// Ping request that times out, while another Ping request is still pending
// and then receives a response.
func TestConcurrentPingTimeout(t *testing.T) {
	t.Parallel()

	coordinator, stream, _ := setupSignCoordinator(t)

	var wg sync.WaitGroup

	timeoutChan := make(chan struct{})

	// Send a Ping request that is expected to time out.
	wg.Add(1)

	go func() {
		defer wg.Done()

		// Note that the timeout is set to 1 second.
		success, err := coordinator.Ping(t.Context(), 1*time.Second)
		require.Equal(t, context.DeadlineExceeded, err)
		require.False(t, success)

		// Signal that the request has timed out.
		close(timeoutChan)
	}()

	// Get the request sent over the mock stream.
	req1, err := getRequest(stream)
	require.NoError(t, err)

	// Verify that the request has the expected request ID and that it's a
	// Ping request.
	require.Equal(t, uint64(2), req1.GetRequestId())
	require.True(t, req1.GetPing())

	// Verify that the coordinator has correctly set up a single response
	// channel for the Ping request with the specific request ID.
	require.Equal(t, coordinator.responses.Len(), 1)
	_, ok := coordinator.responses.Load(uint64(2))
	require.True(t, ok)

	// Now let's send another Ping request that will receive a response.
	wg.Add(1)

	go func() {
		defer wg.Done()

		// Note that the timeout is set to 2 seconds and will therefore
		// time out later than the first request.
		success, err := coordinator.Ping(t.Context(), 2*time.Second)
		require.NoError(t, err)
		require.True(t, success)
	}()

	// Get the second request sent over the mock stream.
	req2, err := getRequest(stream)
	require.NoError(t, err)

	// Verify that the request has the expected request ID and that it's a
	// Ping request.
	require.Equal(t, uint64(3), req2.GetRequestId())
	require.True(t, req2.GetPing())

	// Verify that the coordinator has correctly set up two response
	// channels for the Ping requests with their specific request IDs.
	require.Equal(t, coordinator.responses.Len(), 2)
	_, ok = coordinator.responses.Load(uint64(3))
	require.True(t, ok)

	// Now let's wait for the first request to time out.
	<-timeoutChan

	// Ensure that this leads to the sign coordinator removing the response
	// channel for the timed-out request.
	require.Equal(t, coordinator.responses.Len(), 1)

	// The second request should still be pending, so the responses map
	// should contain the response channel for the second request.
	_, ok = coordinator.responses.Load(uint64(3))
	require.True(t, ok)

	// Send responses for the second Ping request.
	stream.sendResponse(&walletrpc.SignCoordinatorResponse{
		RefRequestId: 3,
		SignResponseType: &walletrpc.SignCoordinatorResponse_Pong{
			Pong: true,
		},
	})

	// Wait for the goroutines to finish, which should only happen after the
	// second request has had its expected response processed.
	wg.Wait()

	// Verify the responses map is empty after all responses have been
	// handled, to ensure that no memory leaks occur.
	require.Equal(t, coordinator.responses.Len(), 0)
}

// TestIncorrectResponseRequestId tests that the sign coordinator correctly
// ignores responses with an unknown request ID.
func TestIncorrectResponseRequestId(t *testing.T) {
	t.Parallel()

	coordinator, stream, _ := setupSignCoordinator(t)

	var wg sync.WaitGroup

	// Save the start time of the test.
	startTime := time.Now()

	// The coordinator uses a 2s request timeout (see setupSignCoordinator).
	// Make the ping timeout longer to avoid racing context cancellation
	// against the request timeout.
	pingTimeout := coordinator.requestTimeout + time.Second

	wg.Add(1)

	// Send a Ping request that times out in 3 seconds. As the request
	// timeout is lower than the ping timeout, the request should error
	// after 2 seconds with a ErrRequestTimeout.
	go func() {
		defer wg.Done()

		success, err := coordinator.Ping(t.Context(), pingTimeout)
		require.Equal(t, ErrRequestTimeout, err)
		require.False(t, success)
	}()

	// Get the request sent over the mock stream.
	req, err := getRequest(stream)
	require.NoError(t, err)

	// Verify that the request has the expected request ID and that it's a
	// Ping request.
	require.Equal(t, uint64(2), req.GetRequestId())
	require.True(t, req.GetPing())

	// Verify that the coordinator has correctly set up a single response
	// channel for the Ping request with the specific request ID.
	require.Equal(t, coordinator.responses.Len(), 1)
	_, ok := coordinator.responses.Load(uint64(2))
	require.True(t, ok)

	// Now let's send a response with another request ID than the Ping
	// request.
	stream.sendResponse(&walletrpc.SignCoordinatorResponse{
		RefRequestId: 3, // Incorrect request ID
		SignResponseType: &walletrpc.SignCoordinatorResponse_Pong{
			Pong: true,
		},
	})

	// Ensure that the response is ignored and that the responses map still
	// contains the response channel for the Ping request until the request
	// times out. We allow a small margin of error to account for the time
	// it takes to execute the Invariant function.
	invariantWaitTime := coordinator.requestTimeout -
		time.Since(startTime) - 100*time.Millisecond

	err = wait.Invariant(func() bool {
		correctLen := coordinator.responses.Len() == 1
		_, ok = coordinator.responses.Load(uint64(2))

		return correctLen && ok
	}, invariantWaitTime)
	require.NoError(t, err)

	// Wait for the goroutines to finish, which should only happen after the
	// request has timed out and verified the error.
	wg.Wait()

	// Verify the responses map is empty after all responses are received
	require.Equal(t, coordinator.responses.Len(), 0)
}

// TestSignerErrorResponse tests that the sign coordinator correctly handles a
// SignerError response from the remote signer.
func TestSignerErrorResponse(t *testing.T) {
	t.Parallel()

	coordinator, stream, _ := setupSignCoordinator(t)

	var wg sync.WaitGroup

	// Send a Ping request that will receive a SignerError response.
	wg.Add(1)

	go func() {
		defer wg.Done()

		success, err := coordinator.Ping(t.Context(), 1*time.Second)
		// Ensure that the result from the Ping method is an error,
		// which is the expected result when a SignerError response is
		// received.
		require.Equal(t, "mock error", err.Error())
		require.False(t, success)
	}()

	// Get the request sent over the mock stream.
	req, err := getRequest(stream)
	require.NoError(t, err)

	// Verify that the request has the expected request ID and that it's a
	// Ping request.
	require.Equal(t, uint64(2), req.GetRequestId())
	require.True(t, req.GetPing())

	// Verify that the coordinator has correctly set up a single response
	// channel for the Ping request with the specific request ID.
	require.Equal(t, coordinator.responses.Len(), 1)
	_, ok := coordinator.responses.Load(uint64(2))
	require.True(t, ok)

	// Now let's send a SignerError response instead of a Pong back over the
	// mock stream.
	rType := &walletrpc.SignCoordinatorResponse_SignerError{
		SignerError: &walletrpc.SignerError{
			Error: "mock error",
		},
	}

	stream.sendResponse(&walletrpc.SignCoordinatorResponse{
		RefRequestId:     2,
		SignResponseType: rType,
	})

	// Wait for the goroutines to finish, which should only happen after the
	// request has had its expected response processed.
	wg.Wait()

	// Verify the responses map is empty after all responses have been
	// processed, to ensure that no memory leaks occur.
	require.Equal(t, coordinator.responses.Len(), 0)
}

// TestStopCoordinator tests that the sign coordinator correctly stops
// processing responses for any pending requests when the sign coordinator is
// stopped.
func TestStopCoordinator(t *testing.T) {
	t.Parallel()

	coordinator, stream, runErrChan := setupSignCoordinator(t)

	pingTimeout := 3 * time.Second
	startTime := time.Now()

	var wg sync.WaitGroup

	// Send a Ping request with a long timeout to ensure that the request
	// will not time out before the coordinator is stopped.
	wg.Add(1)

	go func() {
		defer wg.Done()

		success, err := coordinator.Ping(t.Context(), pingTimeout)
		require.Equal(t, ErrShuttingDown, err)
		require.False(t, success)
	}()

	// Get the request sent over the mock stream.
	req, err := getRequest(stream)
	require.NoError(t, err)

	// Verify that the request has the expected request ID and that it's a
	// Ping request.
	require.Equal(t, uint64(2), req.GetRequestId())
	require.True(t, req.GetPing())

	// Verify that the coordinator has correctly set up a single response
	// channel for the Ping request with the specific request ID.
	require.Equal(t, coordinator.responses.Len(), 1)
	_, ok := coordinator.responses.Load(uint64(2))
	require.True(t, ok)

	// Now let's stop the sign coordinator.
	wg.Add(1)

	go func() {
		defer wg.Done()

		coordinator.Stop()
	}()

	// When the coordinator is stopped, the Run function will return an
	// error that gets sent over the runErrChan.
	err = <-runErrChan

	// Ensure that the Run function returned the expected error that lnd is
	// shutting down.
	require.Equal(t, ErrShuttingDown, err)

	// As the coordinator Run function returned the ErrShuttingDown error,
	// lnd would normally cancel the stream. We simulate this by calling the
	// Cancel method on the mock stream.
	stream.Cancel()

	// Ensure that both the Ping request goroutine and the sign coordinator
	// Stop goroutine have finished.
	wg.Wait()

	// Ensure that the Ping request goroutine returned before the timeout
	// was reached, which indicates that the request was canceled because
	// the sign coordinator was stopped.
	require.Less(t, time.Since(startTime), pingTimeout)

	// Verify the responses map is empty after all responses are received
	require.Equal(t, coordinator.responses.Len(), 0)
}

// TestRemoteSignerDisconnects tests that the sign coordinator correctly handles
// the remote signer disconnecting, which closes the stream.
func TestRemoteSignerDisconnects(t *testing.T) {
	t.Parallel()

	coordinator, stream, runErrChan := setupSignCoordinator(t)

	// Use a timeout longer than the connection timeout so the coordinator
	// returns ErrConnectTimeout because of a disconnect, instead of the
	// context deadline error for the ping request.
	pingTimeout := coordinator.connectionTimeout + (1 * time.Second)
	startTime := time.Now()

	var wg sync.WaitGroup

	// Send a Ping request with a long timeout to ensure that the request
	// will not time out before the remote signer disconnects.
	wg.Add(1)

	go func() {
		defer wg.Done()

		success, err := coordinator.Ping(t.Context(), pingTimeout)
		require.Equal(t, ErrConnectTimeout, err)
		require.False(t, success)
	}()

	// Get the request sent over the mock stream.
	req, err := getRequest(stream)
	require.NoError(t, err)

	// Verify that the request has the expected request ID and that it's a
	// Ping request.
	require.Equal(t, uint64(2), req.GetRequestId())
	require.True(t, req.GetPing())

	// Verify that the coordinator has correctly set up a single response
	// channel for the Ping request with the specific request ID.
	require.Equal(t, coordinator.responses.Len(), 1)
	_, ok := coordinator.responses.Load(uint64(2))
	require.True(t, ok)

	// We simulate the remote signer disconnecting by canceling the
	// stream.
	stream.Cancel()

	// This should cause the Run function to return the error that the
	// stream was canceled.
	err = <-runErrChan
	require.Equal(t, ErrStreamCanceled, err)

	// Ensure that the Ping request goroutine has finished.
	wg.Wait()

	// Verify that the coordinator signals that it's done receiving
	// responses after the stream is canceled, i.e. the StartReceiving
	// function is no longer running.
	<-coordinator.disconnected

	// Ensure that the Ping request goroutine returned after the connection
	// timeout, but before the ping timeout was reached, which indicates
	// that the request was canceled because the remote signer disconnected.
	require.Greater(t, time.Since(startTime), coordinator.connectionTimeout)
	require.Less(t, time.Since(startTime), pingTimeout)

	// Verify the responses map is empty after all responses are received
	require.Equal(t, coordinator.responses.Len(), 0)
}

// TestRemoteSignerReconnectsDuringResponseWait verifies that the sign
// coordinator correctly handles the scenario where the remote signer
// disconnects while a request is being processed and then reconnects. In this
// case, the sign coordinator should establish a new stream, reprocess the
// request, and ultimately receive a response.
func TestRemoteSignerReconnectsDuringResponseWait(t *testing.T) {
	t.Parallel()

	coordinator, stream, runErrChan := setupSignCoordinator(t)

	pingTimeout := 3 * time.Second
	startTime := time.Now()

	var wg sync.WaitGroup

	// Send a Ping request with a long timeout to ensure that the request
	// will not time out before the remote signer disconnects.
	wg.Add(1)

	go func() {
		defer wg.Done()

		success, err := coordinator.Ping(t.Context(), pingTimeout)
		require.NoError(t, err)
		require.True(t, success)
	}()

	// Get the request sent over the mock stream.
	req, err := getRequest(stream)
	require.NoError(t, err)

	// Verify that the request has the expected request ID and that it's a
	// Ping request.
	require.Equal(t, uint64(2), req.GetRequestId())
	require.True(t, req.GetPing())

	// Verify that the coordinator has correctly set up a single response
	// channel for the Ping request with the specific request ID.
	require.Equal(t, coordinator.responses.Len(), 1)
	_, ok := coordinator.responses.Load(uint64(2))
	require.True(t, ok)

	// Now, lets simulate that the remote signer disconnects by canceling
	// the stream, while the sign coordinator is still waiting for the Pong
	// response for the request it sent.
	stream.Cancel()

	// This should cause the Run function to return the error that the
	// stream was canceled.
	err = <-runErrChan
	require.Equal(t, ErrStreamCanceled, err)

	// Verify that the coordinator signals that it's done receiving
	// responses after the stream is canceled, i.e. the StartReceiving
	// function is no longer running.
	<-coordinator.disconnected

	// Now let's simulate that the remote signer reconnects with a new
	// stream.
	stream, _ = setupNewStream(t, coordinator)

	// This should lead to that the sign coordinator resends the Ping
	// request it's needs a response for over the new stream.
	req, err = getRequest(stream)
	require.NoError(t, err)

	// Note that the request ID will be 3 for the resent request, as the
	// coordinator will no longer wait for the response for the request with
	// request ID 2.
	require.Equal(t, uint64(3), req.GetRequestId())
	require.True(t, req.GetPing())

	// Verify that the coordinator has correctly set up a single response
	// channel for the Ping request with the specific request ID.
	require.Equal(t, coordinator.responses.Len(), 2)
	_, ok = coordinator.responses.Load(uint64(3))
	require.True(t, ok)

	// Now let's send the Pong response for the resent Ping request.
	stream.sendResponse(&walletrpc.SignCoordinatorResponse{
		RefRequestId: 3,
		SignResponseType: &walletrpc.SignCoordinatorResponse_Pong{
			Pong: true,
		},
	})

	// Ensure that the Ping request goroutine has finished.
	wg.Wait()

	// Ensure that the Ping request goroutine returned before the timeout
	// was reached, which indicates that the request didn't time out as
	// the remote signer reconnected in time and sent a response.
	require.Less(t, time.Since(startTime), pingTimeout)

	// Verify the responses map is empty after all responses are received
	require.Equal(t, coordinator.responses.Len(), 0)
}

// TestRemoteSignerDisconnectsMidSend verifies that the sign coordinator
// correctly handles the scenario in which the remote signer disconnects while
// the sign coordinator is sending data over the stream (i.e., during the
// execution of the `Send` function) and then reconnects. In such a case, the
// sign coordinator should establish a new stream, reprocess the request, and
// eventually receive a response.
func TestRemoteSignerDisconnectsMidSend(t *testing.T) {
	t.Parallel()

	coordinator, stream, runErrChan := setupSignCoordinator(t)

	pingTimeout := 3 * time.Second
	startTime := time.Now()

	var wg sync.WaitGroup

	// Send a Ping request with a long timeout to ensure that the request
	// will not time out before the remote signer disconnects.
	wg.Add(1)

	go func() {
		defer wg.Done()

		success, err := coordinator.Ping(t.Context(), pingTimeout)
		require.NoError(t, err)
		require.True(t, success)
	}()

	// Just wait slightly, to ensure that the Ping requests starts getting
	// processed before we simulate the remote signer disconnecting.
	<-time.After(10 * time.Millisecond)

	// We simulate the remote signer disconnecting by canceling the
	// stream.
	stream.Cancel()

	// This should cause the Run function to return the error that the
	// stream was canceled with.
	err := <-runErrChan
	require.Equal(t, ErrStreamCanceled, err)

	// Verify that the coordinator signals that it's done receiving
	// responses after the stream is canceled, i.e. the StartReceiving
	// function is no longer running.
	<-coordinator.disconnected

	// Now since the sign coordinator is still processing the requests, and
	// we never extracted the request sent over the stream, the sign
	// coordinator is stuck at the steam.Send function. We simulate this
	// function now errors with the codes.Unavailable error, which is what
	// the function would error with if the signer was disconnected during
	// the send operation in a real scenario.
	stream.sendErrorChan <- status.Errorf(
		codes.Unavailable, "simulated unavailable error",
	)

	// Verify that the coordinator has correctly set up a single response
	// channel for the Ping request with the specific request ID.
	require.Equal(t, 1, coordinator.responses.Len())

	// Now let's simulate that the remote signer reconnects with a new
	// stream.
	stream, _ = setupNewStream(t, coordinator)

	// This should lead to that the sign coordinator resends the Ping
	// request it's needs a response for over the new stream.
	req, err := getRequest(stream)
	require.NoError(t, err)

	// Note that the request ID will be 3 for the resent request, as the
	// coordinator will no longer wait for the response for the request with
	// request ID 2.
	require.Equal(t, uint64(3), req.GetRequestId())
	require.True(t, req.GetPing())

	// Verify that the coordinator has correctly set up a single response
	// channel for the Ping request with the specific request ID.
	require.Equal(t, coordinator.responses.Len(), 2)
	_, ok := coordinator.responses.Load(uint64(3))
	require.True(t, ok)

	// Now let's send the Pong response for the resent Ping request.
	stream.sendResponse(&walletrpc.SignCoordinatorResponse{
		RefRequestId: 3,
		SignResponseType: &walletrpc.SignCoordinatorResponse_Pong{
			Pong: true,
		},
	})

	// Ensure that the Ping request goroutine has finished.
	wg.Wait()

	// Ensure that the Ping request goroutine returned before the timeout
	// was reached, which indicates that the request didn't time out as
	// the remote signer reconnected in time and sent a response.
	require.Less(t, time.Since(startTime), pingTimeout)

	// Verify the responses map is empty after all responses are received
	require.Equal(t, coordinator.responses.Len(), 0)
}
