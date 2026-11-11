package rpcwallet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnrpc/watchonlyrpc"
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

// requestRetryPolicy controls whether a sign coordinator request may be
// replayed after a transient disconnect.
type requestRetryPolicy uint8

const (
	// noRequestRetry marks requests that must not be replayed
	// automatically after a disconnect because they may be stateful.
	noRequestRetry requestRetryPolicy = iota

	// retryOnDisconnect marks requests that are safe to replay after a
	// disconnect before their response is received.
	retryOnDisconnect
)

// coordinatorResult settles one request without sharing mutable response state.
type coordinatorResult struct {
	response *RSResponse
	err      error
}

// coordinatorConnection is an immutable stream generation shared with its IO
// routines. Only the owner closes done or settles finished.
type coordinatorConnection struct {
	send     chan *watchonlyrpc.SignCoordinatorRequest
	done     chan struct{}
	finished chan error
}

// coordinatorLoop is the sole entry point to connection and request state.
type coordinatorLoop struct {
	events chan any
	done   chan struct{}
}

// coordinatorState is accessed only by the owner loop; no request or IO
// goroutine may read its maps, counter, queue or readiness state.
type coordinatorState struct {
	connection *coordinatorConnection
	ready      chan struct{}
	connected  bool
	nextID     uint64
	pending    map[uint64]chan coordinatorResult
	queued     []*watchonlyrpc.SignCoordinatorRequest
}

// coordinatorConnect asks the owner to reserve a stream generation; its
// buffered reply returns the admitted connection or ErrMultipleConnections.
type coordinatorConnect struct {
	reply chan coordinatorConnected
}

// coordinatorConnected replies to a reservation with its stream generation
// or rejection, without sharing mutable coordinator state.
type coordinatorConnected struct {
	connection *coordinatorConnection
	err        error
}

// coordinatorReady reports a completed handshake; the owner releases
// readiness only for the current stream generation.
type coordinatorReady struct {
	connection *coordinatorConnection
}

// coordinatorReadiness asks the owner for its current readiness channel;
// the buffered reply lets the owner finish if the caller is cancelled.
type coordinatorReadiness struct {
	reply chan (<-chan struct{})
}

// coordinatorCall asks the owner to admit caller work before Stop can
// join it; the buffered reply confirms wait-group ownership.
type coordinatorCall struct {
	reply chan struct{}
}

// coordinatorSubmit hands the owner a request for ID allocation and
// response routing, returning admission through a buffered reply.
type coordinatorSubmit struct {
	request *watchonlyrpc.SignCoordinatorRequest
	reply   chan coordinatorSubmitted
}

// coordinatorSubmitted returns a request ID and private result channel,
// or a rejection when the owner has no ready connection.
type coordinatorSubmitted struct {
	id       uint64
	response chan coordinatorResult
	err      error
}

// coordinatorCancel asks the owner to remove routing for a finished
// caller; later sends or responses cannot settle that caller again.
type coordinatorCancel struct {
	id uint64
}

// coordinatorReceived passes a reader result to the owner; stale stream
// generations cannot affect the current connection or its callers.
type coordinatorReceived struct {
	connection *coordinatorConnection
	response   *RSResponse
	err        error
}

// coordinatorSent passes a writer completion to the owner; only a
// failure for the current stream generation can settle its request.
type coordinatorSent struct {
	connection *coordinatorConnection
	id         uint64
	err        error
}

// coordinatorDisconnect reports a returning Run; the owner disconnects
// only when that Run still owns the current stream generation.
type coordinatorDisconnect struct {
	connection *coordinatorConnection
	err        error
}

// SignCoordinator delegates RPCs over the signer-initiated stream.
//
// A single owner, coordinatorLoop.run, holds the connection, readiness, request
// IDs, pending response channels and send queue. It never performs stream IO.
// RPC callers and the Run reader/writer pumps communicate with the owner via
// typed events. Buffered admission/result channels let it finish an accepted
// event even if the caller has timed out.
//
// Run reserves a connection generation before its handshake, so only one Run
// can consume the stream. After handshake it starts one reader and writer and
// posts coordinatorReady. IO events carry that generation; an old reader,
// writer or returning Run cannot settle a replacement's requests.
//
// The handshake produces the watch-only connection-bound proof; the signer
// verifies it before replying to the readiness Ping. Only that Pong permits
// coordinatorReady to publish readiness.
//
// A call is admitted for Stop's wait group, waits for readiness, then submits
// its payload. The owner assigns an ID, installs a result route and queues the
// request before the writer can send it. The reader posts a received event,
// and the owner correlates RefRequestId and resolves the caller exactly once.
// Caller cancellation removes routing, so unsent or late work is ignored.
//
// A disconnect clears the queue, fails pending calls with ErrNotConnected and
// resets readiness. Only requests marked retryOnDisconnect may resubmit;
// stateful MuSig2 requests return the error without automatic retry. Stop
// closes admission, resolves pending calls with ErrShuttingDown, releases the
// active Run to cancel stream IO, then joins admitted calls and the IO pumps.
//
// For example, a call on generation A can time out while its Send is blocked.
// The owner still accepts cancellation; when B replaces A, late A events are
// ignored and B's pending calls keep their own routes.
type SignCoordinator struct {
	loop           *coordinatorLoop
	cg             *fn.ContextGuard
	gManager       *fn.GoroutineManager
	requestTimeout time.Duration

	// startupTimeout is the maximum time we will wait for the remote
	// signer to connect to us during startup. A value of 0 means that we
	// wait indefinitely.
	//
	// NOTE: This is the startup timeout, and is distinct from the
	// connection timeout in lncfg.ConnectionCfg, which bounds a single
	// attempt at establishing an outbound connection.
	startupTimeout time.Duration
}

// A compile-time assertion keeps the RPC adapter surface unchanged.
var _ RemoteSignerRequests = (*SignCoordinator)(nil)

// NewSignCoordinator creates an owner loop that lives until Stop. A startup
// timeout of zero retains indefinite startup waiting, independently of RPC
// timeouts.
func NewSignCoordinator(requestTimeout time.Duration,
	startupTimeout time.Duration) *SignCoordinator {

	s := &SignCoordinator{
		loop: &coordinatorLoop{
			events: make(chan any),
			done:   make(chan struct{}),
		},
		cg:             fn.NewContextGuard(),
		gManager:       fn.NewGoroutineManager(),
		requestTimeout: requestTimeout,
		startupTimeout: startupTimeout,
	}

	// The manager is new, so admission cannot fail. Quit releases the loop
	// before Stop waits for accepted calls and stream IO to finish.
	s.gManager.Go(context.Background(), func(_ context.Context) {
		s.loop.run(s)
	})

	return s
}

// event transfers an immutable operation without blocking shutdown or caller
// cancellation. Accepted operations always deliver their buffered admission
// reply.
func (s *SignCoordinator) event(ctx context.Context, event any) error {
	select {
	case <-s.cg.Done():
		return ErrShuttingDown

	case <-ctx.Done():
		return ctx.Err()

	case s.loop.events <- event:
		return nil
	}
}

// disconnect settles the active generation exactly once. Keeping an unopened
// ready channel across failed handshakes lets existing startup waiters recover.
func (state *coordinatorState) disconnect(err error) {
	// Stop also reaches here before the first stream connects or after
	// its predecessor has already disconnected.
	if state.connection == nil {
		return
	}
	close(state.connection.done)
	state.connection.finished <- err
	state.connection = nil
	for id, result := range state.pending {
		result <- coordinatorResult{err: ErrNotConnected}
		delete(state.pending, id)
	}
	state.queued = nil
	if state.connected {
		state.ready = make(chan struct{})
	}
	state.connected = false
}

// run owns state as local variables and never performs blocking stream IO.
// Readiness, cancellation and shutdown can therefore progress during a stalled
// Send.
func (loop *coordinatorLoop) run(s *SignCoordinator) {
	defer close(loop.done)
	state := coordinatorState{
		ready:   make(chan struct{}),
		nextID:  handshakeRequestID,
		pending: make(map[uint64]chan coordinatorResult),
	}

	for {
		// A nil send lane disables queue draining until the handshake
		// is complete. Cancelled queued requests are discarded before
		// Send.
		for len(state.queued) > 0 &&
			state.pending[state.queued[0].RequestId] == nil {
			state.queued = state.queued[1:]
		}

		var send chan *watchonlyrpc.SignCoordinatorRequest
		var request *watchonlyrpc.SignCoordinatorRequest

		if state.connected && len(state.queued) > 0 {
			send = state.connection.send
			request = state.queued[0]
		}

		select {
		case <-s.cg.Done():
			// Settle callers with the shutdown sentinel before
			// releasing Run. Its return closes the real RPC stream
			// and unblocks IO.
			for id, result := range state.pending {
				result <- coordinatorResult{
					err: ErrShuttingDown,
				}
				delete(state.pending, id)
			}
			state.disconnect(ErrShuttingDown)

			return

		case send <- request:
			state.queued = state.queued[1:]

		case message := <-loop.events:
			switch event := message.(type) {
			case coordinatorConnect:
				if state.connection != nil {
					event.reply <- coordinatorConnected{
						err: ErrMultipleConnections,
					}

					continue
				}
				connection := &coordinatorConnection{
					send: make(
						chan *watchonlyrpc.
							SignCoordinatorRequest,
					),
					done:     make(chan struct{}),
					finished: make(chan error, 1),
				}
				state.connection = connection

				// Only this owner admits wait-group work. Stop
				// first joins the owner, preventing Add from
				// racing its subsequent Wait.
				s.cg.WgAdd(1)
				event.reply <- coordinatorConnected{
					connection: connection,
				}

			case coordinatorCall:
				// Admit callers through the owner so Stop can
				// join admission before waiting for accepted
				// work.
				s.cg.WgAdd(1)
				event.reply <- struct{}{}

			case coordinatorReady:
				// Only the current handshake may release
				// startup waiters; a superseded stream cannot
				// mark ready.
				if event.connection == state.connection &&
					!state.connected {

					state.connected = true
					log.Infof("Remote signer connected " +
						"and ready")
					close(state.ready)
				}

			case coordinatorReadiness:
				// Return the current generation's readiness
				// channel without exposing mutable connection
				// state.
				event.reply <- state.ready

			case coordinatorSubmit:
				// Install routing before queueing so even an
				// immediate response finds its waiting caller.
				if !state.connected {
					event.reply <- coordinatorSubmitted{
						err: ErrNotConnected,
					}

					continue
				}
				state.nextID++
				result := make(chan coordinatorResult, 1)
				payload := event.request.SignRequestType
				request := &watchonlyrpc.SignCoordinatorRequest{
					RequestId:       state.nextID,
					SignRequestType: payload,
				}
				state.pending[state.nextID] = result
				state.queued = append(state.queued, request)
				event.reply <- coordinatorSubmitted{
					id:       state.nextID,
					response: result,
				}

			case coordinatorCancel:
				// Remove routing immediately. Queue draining
				// drops unsent entries separately; late replies
				// are ignored.
				delete(state.pending, event.id)

			case coordinatorDisconnect:
				// A returning old Run must not disconnect its
				// replacement or settle the replacement's
				// requests.
				if event.connection == state.connection {
					state.disconnect(event.err)
				}

			case coordinatorReceived:
				// Generation matching and deletion settle a
				// caller once despite late or duplicate stream
				// responses.
				if event.connection != state.connection {
					continue
				}
				if event.err != nil {
					state.disconnect(event.err)

					continue
				}
				id := event.response.GetRefRequestId()
				if result := state.pending[id]; result != nil {
					result <- coordinatorResult{
						response: event.response,
					}
					delete(state.pending, id)
				}

			case coordinatorSent:
				// Successful sends await their responses. Only
				// a current-generation failure may settle
				// routing here.
				if event.connection != state.connection ||
					event.err == nil {

					continue
				}
				result := state.pending[event.id]
				if result != nil {
					result <- coordinatorResult{
						err: event.err,
					}
					delete(state.pending, event.id)
				}
			}
		}
	}
}

// Run reserves the stream before handshaking, so a second Run cannot compete
// for Recv. Returning releases the gRPC handler and thus cancels blocked IO.
func (s *SignCoordinator) Run(stream StreamServer) error {
	reply := make(chan coordinatorConnected, 1)
	err := s.event(stream.Context(), coordinatorConnect{reply: reply})
	if err != nil {
		return err
	}
	accepted := <-reply
	if accepted.err != nil {
		return accepted.err
	}
	defer s.cg.WgDone()
	connection := accepted.connection
	defer func() {
		_ = s.event(context.Background(), coordinatorDisconnect{
			connection: connection,
			err:        ErrNotConnected,
		})
	}()
	if err := s.handshake(stream); err != nil {
		return err
	}

	// The single reader and writer replace lock-based snapshots and
	// unstructured sends. Both publish immutable generation-tagged events.
	if !s.gManager.Go(stream.Context(), func(_ context.Context) {
		for {
			response, err := stream.Recv()
			_ = s.event(context.Background(), coordinatorReceived{
				connection: connection,
				response:   response,
				err:        err,
			})
			if err != nil {
				return
			}
		}
	}) {

		if err := stream.Context().Err(); err != nil {
			return err
		}

		return ErrShuttingDown
	}
	if !s.gManager.Go(stream.Context(), func(_ context.Context) {
		for {
			select {
			case <-connection.done:
				return

			case request := <-connection.send:
				log.Debugf("Sending a %T to the remote signer "+
					"with request ID %d",
					request.SignRequestType,
					request.RequestId,
				)
				log.Tracef("Request content: %v",
					formatSignCoordinatorMsg(request))
				err := stream.Send(request)
				event := coordinatorSent{
					connection: connection,
					id:         request.RequestId,
					err:        err,
				}
				_ = s.event(context.Background(), event)
			}
		}
	}) {

		if err := stream.Context().Err(); err != nil {
			return err
		}

		return ErrShuttingDown
	}
	err = s.event(stream.Context(), coordinatorReady{
		connection: connection,
	})
	if err != nil {
		return err
	}

	return <-connection.finished
}

// Stop closes admission, settles outstanding requests, and joins Run and IO.
func (s *SignCoordinator) Stop() {
	log.Infof("Stopping Sign Coordinator")
	defer log.Debugf("Sign coordinator stopped")

	s.cg.Quit()
	<-s.loop.done
	s.cg.WgWait()
	s.gManager.Stop()
}

// WaitUntilConnected waits until the remote signer has connected. If the remote
// signer does not connect within the configured startup timeout, or if the
// passed context is canceled, an error is returned. A startup timeout of 0
// means that we wait indefinitely for the remote signer to connect.
func (s *SignCoordinator) WaitUntilConnected(ctx context.Context) error {
	reply := make(chan (<-chan struct{}), 1)
	if err := s.event(ctx, coordinatorReadiness{reply: reply}); err != nil {
		return err
	}
	ready := <-reply

	// A startup timeout of 0 means that we wait indefinitely, which we
	// achieve by leaving the timeout channel nil so that it never fires.
	var timeout <-chan time.Time
	if s.startupTimeout > 0 {
		timer := time.NewTimer(s.startupTimeout)
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case <-ready:
		return nil

	case <-s.cg.Done():
		return ErrShuttingDown

	case <-ctx.Done():
		return ctx.Err()

	case <-timeout:
		return ErrConnectTimeout
	}
}

// handshake performs the initial handshake with the remote signer. This must
// be done before any other requests are sent to the remote signer.
func (s *SignCoordinator) handshake(stream StreamServer) error {
	var (
		proof            string
		registerChan     = make(chan *watchonlyrpc.SignerRegistration)
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
	if !s.gManager.Go(ctxt, func(_ context.Context) {
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
		case *watchonlyrpc.SignCoordinatorResponse_SignerRegistration:
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
	}) {

		// Admission can fail on the stream deadline as well as Stop.
		// Preserve the context cause so callers can distinguish them.
		if err := ctxt.Err(); err != nil {
			return err
		}

		return ErrShuttingDown
	}

	// Wait for the initial message to arrive or time out if it takes too
	// long. The initial message must be a registration message from the
	// remote signer.
	select {
	case signerRegistration := <-registerChan:
		// Bind registration to this connection, rather than
		// re-selecting a certificate key that may have changed since
		// the TLS handshake.
		var err error
		proof, err = watchonlyrpc.RegistrationProof(
			stream.Context(),
			signerRegistration.GetRegistrationChallenge(),
			signerRegistration.GetRegistrationInfo(),
		)
		if err != nil {
			return err
		}

	case err := <-errChan:
		return fmt.Errorf("error receiving initial remote signer "+
			"registration message: %w", err)

	case <-s.cg.Done():
		return ErrShuttingDown

	case <-ctxt.Done():
		return ctxt.Err()
	}

	complete := &watchonlyrpc.RegistrationResponse_RegistrationComplete{
		RegistrationComplete: &watchonlyrpc.RegistrationComplete{
			Signature:        proof,
			RegistrationInfo: watchonlyrpc.RegistrationVersion,
		},
	}

	// Send a message to the client to indicate that the registration has
	// successfully completed.
	req := &watchonlyrpc.SignCoordinatorRequest_RegistrationResponse{
		RegistrationResponse: &watchonlyrpc.RegistrationResponse{
			RegistrationResponseType: complete,
		},
	}

	regCompleteMsg := &watchonlyrpc.SignCoordinatorRequest{
		RequestId:       handshakeRequestID,
		SignRequestType: req,
	}

	// Send the message in a goroutine because the Send method blocks until
	// the message is read by the client.
	if !s.gManager.Go(ctxt, func(_ context.Context) {
		err := stream.Send(regCompleteMsg)
		if err == nil {
			// A Pong confirms proof acceptance using the existing
			// protocol: signer dispatch starts after verification.
			err = stream.Send(&watchonlyrpc.SignCoordinatorRequest{
				RequestId: handshakeRequestID,
				SignRequestType: &watchonlyrpc.
					SignCoordinatorRequest_Ping{Ping: true},
			})
		}
		if err == nil {
			var response *RSResponse
			response, err = stream.Recv()
			if err == nil && (response.GetRefRequestId() !=
				handshakeRequestID || !response.GetPong()) {

				err = ErrUnexpectedResponse
			}
		}
		if err != nil {
			select {
			case errChan <- err:

			case <-ctxt.Done():
			}

			return
		}

		close(registerDoneChan)
	}) {

		// Admission can fail on the stream deadline as well as Stop.
		// Preserve the context cause so callers can distinguish them.
		if err := ctxt.Err(); err != nil {
			return err
		}

		return ErrShuttingDown
	}

	select {
	case err := <-errChan:
		return fmt.Errorf("error completing remote signer "+
			"registration: %w", err)

	case <-ctxt.Done():
		return ctxt.Err()

	case <-s.cg.Done():
		return ErrShuttingDown

	case <-registerDoneChan:
	}

	return nil
}

// Ping sends a ping request to the remote signer and waits for a pong response.
func (s *SignCoordinator) Ping(ctx context.Context,
	timeout time.Duration) (bool, error) {

	req := &watchonlyrpc.SignCoordinatorRequest_Ping{
		Ping: true,
	}

	// As we're pinging, we will time out the request if we don't receive a
	// response within the timeout.
	ctxt, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return processRequest(
		ctxt, s,
		&watchonlyrpc.SignCoordinatorRequest{SignRequestType: req},
		func(resp *RSResponse) bool {
			return resp.GetPong()
		},
		retryOnDisconnect,
	)
}

// DeriveSharedKey sends a SharedKeyRequest to the remote signer and waits for
// the corresponding response.
//
// NOTE: This is part of the RemoteSignerRequests interface.
func (s *SignCoordinator) DeriveSharedKey(ctx context.Context,
	in *signrpc.SharedKeyRequest,
	_ ...grpc.CallOption) (*signrpc.SharedKeyResponse, error) {

	req := &watchonlyrpc.SignCoordinatorRequest_SharedKeyRequest{
		SharedKeyRequest: in,
	}

	return processRequest(
		ctx, s,
		&watchonlyrpc.SignCoordinatorRequest{SignRequestType: req},
		func(resp *RSResponse) *signrpc.SharedKeyResponse {
			return resp.GetSharedKeyResponse()
		},
		noRequestRetry,
	)
}

// MuSig2Cleanup sends a MuSig2CleanupRequest to the remote signer and waits for
// the corresponding response.
//
// NOTE: This is part of the RemoteSignerRequests interface.
func (s *SignCoordinator) MuSig2Cleanup(ctx context.Context,
	in *signrpc.MuSig2CleanupRequest,
	_ ...grpc.CallOption) (*signrpc.MuSig2CleanupResponse, error) {

	req := &watchonlyrpc.SignCoordinatorRequest_MuSig2CleanupRequest{
		MuSig2CleanupRequest: in,
	}

	return processRequest(
		ctx, s,
		&watchonlyrpc.SignCoordinatorRequest{SignRequestType: req},
		func(resp *RSResponse) *signrpc.MuSig2CleanupResponse {
			return resp.GetMuSig2CleanupResponse()
		},
		noRequestRetry,
	)
}

// MuSig2CombineSig sends a MuSig2CombineSigRequest to the remote signer and
// waits for the corresponding response.
//
// NOTE: This is part of the RemoteSignerRequests interface.
func (s *SignCoordinator) MuSig2CombineSig(ctx context.Context,
	in *signrpc.MuSig2CombineSigRequest,
	_ ...grpc.CallOption) (*signrpc.MuSig2CombineSigResponse, error) {

	req := &watchonlyrpc.SignCoordinatorRequest_MuSig2CombineSigRequest{
		MuSig2CombineSigRequest: in,
	}

	return processRequest(
		ctx, s,
		&watchonlyrpc.SignCoordinatorRequest{SignRequestType: req},
		func(resp *RSResponse) *signrpc.MuSig2CombineSigResponse {
			return resp.GetMuSig2CombineSigResponse()
		},
		noRequestRetry,
	)
}

// MuSig2CreateSession sends a MuSig2SessionRequest to the remote signer and
// waits for the corresponding response.
//
// NOTE: This is part of the RemoteSignerRequests interface.
func (s *SignCoordinator) MuSig2CreateSession(ctx context.Context,
	in *signrpc.MuSig2SessionRequest,
	_ ...grpc.CallOption) (*signrpc.MuSig2SessionResponse, error) {

	req := &watchonlyrpc.SignCoordinatorRequest_MuSig2SessionRequest{
		MuSig2SessionRequest: in,
	}

	return processRequest(
		ctx, s,
		&watchonlyrpc.SignCoordinatorRequest{SignRequestType: req},
		func(resp *RSResponse) *signrpc.MuSig2SessionResponse {
			return resp.GetMuSig2SessionResponse()
		},
		noRequestRetry,
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

	req := &watchonlyrpc.SignCoordinatorRequest_MuSig2RegisterNoncesRequest{
		MuSig2RegisterNoncesRequest: in,
	}

	type muSig2RegisterNoncesResp = *signrpc.MuSig2RegisterNoncesResponse

	return processRequest(
		ctx, s,
		&watchonlyrpc.SignCoordinatorRequest{SignRequestType: req},
		func(resp *RSResponse) muSig2RegisterNoncesResp {
			return resp.GetMuSig2RegisterNoncesResponse()
		},
		noRequestRetry,
	)
}

// MuSig2RegisterCombinedNonce sends a MuSig2RegisterCombinedNonce to
// the remote signer and waits for the corresponding response.
func (s *SignCoordinator) MuSig2RegisterCombinedNonce(ctx context.Context,
	in *signrpc.MuSig2RegisterCombinedNonceRequest,
	_ ...grpc.CallOption) (*signrpc.MuSig2RegisterCombinedNonceResponse,
	error) {

	req := &watchonlyrpc.SignCoordinatorRequest_MuSig2CombinedNoncesReq{
		MuSig2CombinedNoncesReq: in,
	}

	type muSig2RegCombNResp = *signrpc.MuSig2RegisterCombinedNonceResponse

	return processRequest(
		ctx, s,
		&watchonlyrpc.SignCoordinatorRequest{SignRequestType: req},
		func(resp *RSResponse) muSig2RegCombNResp {
			return resp.GetMuSig2CombNoncesResp()
		},
		noRequestRetry,
	)
}

// MuSig2GetCombinedNonce sends a MuSig2GetCombinedNonceRequest to the
// remote signer and waits for the corresponding response.
func (s *SignCoordinator) MuSig2GetCombinedNonce(ctx context.Context,
	in *signrpc.MuSig2GetCombinedNonceRequest,
	_ ...grpc.CallOption) (*signrpc.MuSig2GetCombinedNonceResponse, error) {

	req := &watchonlyrpc.SignCoordinatorRequest_MuSig2GetCombinedNoncesReq{
		MuSig2GetCombinedNoncesReq: in,
	}

	type muSig2GetCombNonceResp = *signrpc.MuSig2GetCombinedNonceResponse

	return processRequest(
		ctx, s,
		&watchonlyrpc.SignCoordinatorRequest{SignRequestType: req},
		func(resp *RSResponse) muSig2GetCombNonceResp {
			return resp.GetMuSig2GetCombNoncesResp()
		},
		noRequestRetry,
	)
}

// MuSig2Sign sends a MuSig2SignRequest to the remote signer and waits for the
// corresponding response.
//
// NOTE: This is part of the RemoteSignerRequests interface.
func (s *SignCoordinator) MuSig2Sign(ctx context.Context,
	in *signrpc.MuSig2SignRequest,
	_ ...grpc.CallOption) (*signrpc.MuSig2SignResponse, error) {

	req := &watchonlyrpc.SignCoordinatorRequest_MuSig2SignRequest{
		MuSig2SignRequest: in,
	}

	return processRequest(
		ctx, s,
		&watchonlyrpc.SignCoordinatorRequest{SignRequestType: req},
		func(resp *RSResponse) *signrpc.MuSig2SignResponse {
			return resp.GetMuSig2SignResponse()
		},
		noRequestRetry,
	)
}

// SignMessage sends a SignMessageReq to the remote signer and waits for the
// corresponding response.
//
// NOTE: This is part of the RemoteSignerRequests interface.
func (s *SignCoordinator) SignMessage(ctx context.Context,
	in *signrpc.SignMessageReq,
	_ ...grpc.CallOption) (*signrpc.SignMessageResp, error) {

	req := &watchonlyrpc.SignCoordinatorRequest_SignMessageReq{
		SignMessageReq: in,
	}

	return processRequest(
		ctx, s,
		&watchonlyrpc.SignCoordinatorRequest{SignRequestType: req},
		func(resp *RSResponse) *signrpc.SignMessageResp {
			return resp.GetSignMessageResp()
		},
		retryOnDisconnect,
	)
}

// SignPsbt sends a SignPsbtRequest to the remote signer and waits for the
// corresponding response.
//
// NOTE: This is part of the RemoteSignerRequests interface.
func (s *SignCoordinator) SignPsbt(ctx context.Context,
	in *walletrpc.SignPsbtRequest,
	_ ...grpc.CallOption) (*walletrpc.SignPsbtResponse, error) {

	req := &watchonlyrpc.SignCoordinatorRequest_SignPsbtRequest{
		SignPsbtRequest: in,
	}

	return processRequest(
		ctx, s,
		&watchonlyrpc.SignCoordinatorRequest{SignRequestType: req},
		func(resp *RSResponse) *walletrpc.SignPsbtResponse {
			return resp.GetSignPsbtResponse()
		},
		retryOnDisconnect,
	)
}

// processRequestAttempt submits one generation-bound request. Only the owner
// touches its ID and pending entry; this caller owns deadlines and
// cancellation.
func processRequestAttempt[R comparable](ctx context.Context,
	s *SignCoordinator, request *watchonlyrpc.SignCoordinatorRequest,
	extractResponse func(*RSResponse) R, retryPolicy requestRetryPolicy) (
	bool, R, error) {

	var zero R
	if err := s.WaitUntilConnected(ctx); err != nil {
		return false, zero, err
	}
	reply := make(chan coordinatorSubmitted, 1)
	err := s.event(ctx, coordinatorSubmit{
		request: request,
		reply:   reply,
	})
	if err != nil {
		return false, zero, err
	}
	accepted := <-reply
	if accepted.err != nil {
		return retryPolicy == retryOnDisconnect, zero, accepted.err
	}
	defer func() {
		_ = s.event(context.Background(), coordinatorCancel{
			id: accepted.id,
		})
	}()

	// Include queued Send time in the round-trip deadline without a
	// per-request watcher goroutine or blocking the coordinator owner.
	timer := time.NewTimer(s.requestTimeout)
	defer timer.Stop()
	var result coordinatorResult
	select {
	case result = <-accepted.response:

	case <-ctx.Done():
		log.Debugf("Context cancelled while waiting for remote "+
			"signer response for request ID %d", accepted.id)

		return false, zero, ctx.Err()

	case <-timer.C:
		log.Debugf("Remote signer response timed out for request ID %d",
			accepted.id)

		return false, zero, ErrRequestTimeout

	case <-s.cg.Done():
		log.Debugf("Stopped waiting for remote signer response for "+
			"request ID %d as we are shutting down", accepted.id)

		return false, zero, ErrShuttingDown
	}
	if result.err != nil {
		log.Debugf("Remote signer request ID %d failed: %v",
			accepted.id, result.err)
		retry := retryPolicy == retryOnDisconnect &&
			(errors.Is(result.err, ErrNotConnected) ||
				status.Code(result.err) == codes.Unavailable)

		return retry, zero, result.err
	}
	if failure := result.response.GetSignerError(); failure != nil {
		log.Debugf("Received an error response from remote signer "+
			"for request ID %d: %v",
			accepted.id, failure.GetError())

		return false, zero, errors.New(failure.GetError())
	}
	log.Debugf("Received remote signer %T response for request ID %d",
		result.response.GetSignResponseType(), accepted.id)
	log.Tracef("Remote signer response content: %v",
		formatSignCoordinatorMsg(result.response))
	response := extractResponse(result.response)
	if response == zero {
		return false, zero, ErrUnexpectedResponse
	}

	return false, response, nil
}

// processRequest retains the existing safe-retry policy and keeps stateful
// MuSig2 calls single-attempt even if a replacement stream becomes ready.
func processRequest[R comparable](ctx context.Context, s *SignCoordinator,
	request *watchonlyrpc.SignCoordinatorRequest,
	extractResponse func(*RSResponse) R,
	retryPolicy requestRetryPolicy) (R, error) {

	var zero R
	reply := make(chan struct{}, 1)
	if err := s.event(ctx, coordinatorCall{reply: reply}); err != nil {
		return zero, err
	}
	<-reply
	defer s.cg.WgDone()
	for {
		retry, response, err := processRequestAttempt(
			ctx, s, request, extractResponse, retryPolicy,
		)
		if retry {
			log.Debugf("Remote signer disconnected; retrying %T",
				request.SignRequestType)

			continue
		}

		return response, err
	}
}
