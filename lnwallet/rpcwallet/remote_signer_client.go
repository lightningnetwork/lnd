package rpcwallet

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnrpc/watchonlyrpc"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type (
	// RSResponse is a type alias for the SignCoordinator response type,
	// created to keep line length within 80 characters.
	RSResponse = watchonlyrpc.SignCoordinatorResponse

	// RSRegistrationResponse is a type alias for the registration response
	// type, created to keep line length within 80 characters.
	RSRegistrationResponse = watchonlyrpc.
				SignCoordinatorRequest_RegistrationResponse

	// RSRegistrationComplete is a type alias for the registration complete
	// type, created to keep line length within 80 characters.
	RSRegistrationComplete = watchonlyrpc.
				RegistrationResponse_RegistrationComplete

	// RSRegistration is a type alias for the signer registration type,
	// created to keep line length within 80 characters.
	RSRegistration = watchonlyrpc.SignCoordinatorResponse_SignerRegistration
)

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

	// handshakeRequestID is the request ID reserved for the handshake with
	// the watch-only node.
	handshakeRequestID = uint64(1)

	// signerWorkerCount conservatively bounds synchronous wallet calls
	// across reconnects; Stop must drain entered calls that cannot cancel.
	signerWorkerCount = 4

	// maxPendingSignerRequests retains a small fixed burst backlog beyond
	// the workers. This conservative bound is not benchmark-tuned; excess
	// requests fail fast instead of growing memory without limit.
	maxPendingSignerRequests = 32

	// maxPendingSignerResponses bounds each stream's response backlog.
	// A stalled writer must close the transport to release full routing.
	maxPendingSignerResponses = 64
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

	// Wrap the connection in a RemoteSigner stream client.
	watchOnlyClient := watchonlyrpc.NewWatchOnlyClient(conn)

	// Create a new stream to the watch-only node.
	streamClient, err := watchOnlyClient.SignCoordinatorStreams(ctx)
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

	// Identify the destination before shared dialing so reconnect failures
	// can be traced to the configured watch-only endpoint.
	log.InfoS(ctx, "Attempting to connect to the watch-only node",
		slog.String("rpc_host", s.cfg.RPCHost))

	return dialRemoteSignerRPC(ctx, s.cfg, s.cfg.Timeout)
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
	stopped    atomic.Bool
	dispatcher *signerDispatcher

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

	// requestTimeout bounds queued/backend work and each response write.
	// Synchronous backends must return before Stop can finish draining.
	requestTimeout time.Duration

	// maxRetryTimeout is the max value for the retry timeout, defining
	// the maximum backoff period before attempting to reconnect to the
	// watch-only node.
	maxRetryTimeout time.Duration

	cg       *fn.ContextGuard
	gManager *fn.GoroutineManager
}

// NewOutboundClient creates a new instance of the remote signer client used by
// a signer node that dials the watch-only node.
// The passed subServers need to include a walletrpc.WalletKitServer and a
// signrpc.SignerServer, or the OutboundClient will be disabled.
// Note that the client will only fully start if this node is configured to act
// as a remote signer that dials out.
func NewOutboundClient(walletServer walletrpc.WalletKitServer,
	signerServer signrpc.SignerServer,
	streamFeeder SignCoordinatorStreamFeeder,
	requestTimeout time.Duration) (*OutboundClient, error) {

	if walletServer == nil || signerServer == nil {
		return nil, errors.New("sub-servers cannot be nil when " +
			"acting as a remote signer that dials the watch-only " +
			"node")
	}

	if streamFeeder == nil {
		return nil, errors.New("streamFeeder cannot be nil")
	}

	return &OutboundClient{
		dispatcher: &signerDispatcher{
			incoming:  make(chan signerJob),
			work:      make(chan signerJob),
			completed: make(chan string),
		},
		log:             log.WithPrefix("Remote signer client: "),
		walletServer:    walletServer,
		signerServer:    signerServer,
		streamFeeder:    streamFeeder,
		requestTimeout:  requestTimeout,
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

	// One pool spans reconnects, bounding entered backend calls even when
	// old synchronous handlers cannot honor cancellation yet.
	r.dispatcher.start.Do(func() {
		r.gManager.Go(ctx, r.dispatcher.run)
		for range signerWorkerCount {
			r.gManager.Go(ctx, r.dispatcher.worker)
		}
	})

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
	// retryTimeout is the current backoff timeout used when retrying to set
	// up a connection to the watch-only node, if the previous
	// connection/attempt failed. The variable is reset to the
	// defaultRetryTimeout once a successful connection is set up with the
	// watch-only node.
	retryTimeout := defaultRetryTimeout

	for {
		// Check if we are shutting down.
		select {
		case <-ctx.Done():
			return
		default:
		}

		connected, err := r.runOnce(ctx)
		if err != nil {
			r.log.ErrorS(ctx, "runOnce error", err)
		}

		// Reset the retry timeout after a successful connection.
		if connected {
			retryTimeout = defaultRetryTimeout
		}

		r.log.InfoS(
			ctx,
			"Connection retry to watch-only node scheduled",
			"retry_after", retryTimeout,
		)

		// Backoff before retrying to connect to the watch-only node.
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryTimeout):
		}

		r.log.InfoS(ctx, "Retrying to connect to watch-only node")

		// Increase the retry timeout by 50% for every retry.
		retryTimeout = time.Duration(
			float64(retryTimeout) * retryMultiplier,
		)

		// But cap the retryTimeout at r.maxRetryTimeout.
		if retryTimeout > r.maxRetryTimeout {
			retryTimeout = r.maxRetryTimeout
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
func (r *OutboundClient) runOnce(ctx context.Context) (bool, error) {
	// Derive a context for the lifetime of the stream.
	ctx, cancel := r.cg.Create(ctx)

	// Cancel the stream context whenever we return from this function.
	defer cancel()

	log.InfoS(ctx, "Attempting to setup the watch-only node connection")

	// Try to get a new stream to the watch-only node.
	stream, err := r.streamFeeder.GetStream(ctx)
	if err != nil {
		return false, err
	}
	defer func() {
		err := stream.Close()
		if err != nil {
			log.ErrorS(ctx, "Unable to close watch-only node "+
				"connection", err)
		}
	}()

	// Transport closure must cancel blocked response/admission routing even
	// when the main loop cannot yet consume its IO failure. The guard
	// also cancels this stream's readers and writers on client Stop.
	ctx, cancelStream := r.cg.Create(stream.Context())
	defer cancelStream()

	// Once the stream has been created, we'll need to perform the handshake
	// process with the watch-only node, before it will start sending us
	// requests.
	err = r.handshake(ctx, stream)
	if err != nil {
		return false, err
	}

	log.InfoS(ctx, "Completed setup connection to watch-only node")

	return true, r.processSignRequestsForever(ctx, stream)
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
		msg     *watchonlyrpc.SignCoordinatorRequest
		errChan = make(chan error, 1)
	)

	// A fresh challenge binds this registration to the actual TLS stream,
	// retaining TLS/macaroons as the endpoint/caller authentication.
	challenge := make([]byte, sha256.Size)
	if _, err := rand.Read(challenge); err != nil {
		return fmt.Errorf("generate registration challenge: %w", err)
	}
	version := watchonlyrpc.RegistrationVersion
	var registrationMsg = &watchonlyrpc.SignCoordinatorResponse{
		RefRequestId: handshakeRequestID,
		SignResponseType: &RSRegistration{
			SignerRegistration: &watchonlyrpc.SignerRegistration{
				RegistrationChallenge: challenge,
				RegistrationInfo:      version,
			},
		},
	}

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
	resp, ok := msg.GetSignRequestType().(*RSRegistrationResponse)
	if !ok {
		return fmt.Errorf("expected registration response, but got: %T",
			msg.GetSignRequestType())
	}

	switch rType := resp.RegistrationResponse.
		GetRegistrationResponseType().(type) {
	// The registration was successful.
	case *RSRegistrationComplete:
		// Accept requests only after the peer confirms the fresh
		// challenge on this TLS connection using the supported protocol
		// version.
		return watchonlyrpc.VerifyRegistrationProof(
			stream.Context(), challenge,
			rType.RegistrationComplete.GetRegistrationInfo(),
			rType.RegistrationComplete.GetSignature(),
		)

	// An error occurred during the registration process.
	case *watchonlyrpc.RegistrationResponse_RegistrationError:
		return fmt.Errorf("registration error: %s",
			rType.RegistrationError)

	default:
		return fmt.Errorf("unknown registration response type: %T",
			resp.RegistrationResponse.GetRegistrationResponseType())
	}
}

// processSignRequestsForever owns one stream's reader/router/writer flow.
//
// One reader feeds this loop; one writer drains its bounded responses.
// Ping goes straight to responses but still shares wire backpressure.
// Signing flows via dispatcher -> worker -> formResponse -> responses.
// The client-wide four-worker/32-pending pool survives reconnects and
// runs one job per MuSig2 session; independent sessions may overlap.
// Deadlines include queue time: full admission returns busy; expiry runs
// only the response closure without backend entry. Results stay with the
// original stream, and formResponse discards late signatures. Send errors
// or timeouts close the transport, release routing and permit reconnect.
// Stop drains entered backend calls. A1 -> A2 stays FIFO while Ping proceeds.
func (r *OutboundClient) processSignRequestsForever(ctx context.Context,
	stream *Stream) error {

	requests := make(chan *watchonlyrpc.SignCoordinatorRequest)
	responses := make(chan *RSResponse, maxPendingSignerResponses)
	failures := make(chan error, 2)

	// Exactly one reader and one response writer belong to this stream.
	// runOnce closes the real connection on return, releasing blocked IO.
	if !r.gManager.Go(ctx, func(_ context.Context) {
		for {
			request, err := stream.Recv()
			if err != nil {
				select {
				case failures <- err:

				case <-ctx.Done():
				}

				return
			}

			select {
			case requests <- request:

			case <-ctx.Done():
				return
			}
		}
	}) {

		return ErrShuttingDown
	}

	if !r.gManager.Go(ctx, func(_ context.Context) {
		for {
			select {
			case <-ctx.Done():
				return

			case response := <-responses:
				err := r.sendResponse(ctx, response, stream)
				if err != nil {
					select {
					case failures <- err:

					case <-ctx.Done():
					}

					return
				}
			}
		}
	}) {

		return ErrShuttingDown
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err := <-failures:
			return err

		case request := <-requests:
			// Control messages do not wait for a backend worker or
			// a slot in the signing queue. Wire backpressure
			// remains stream-scoped.
			_, ping := request.GetSignRequestType().(*watchonlyrpc.
				SignCoordinatorRequest_Ping)
			if ping {
				select {
				case responses <- r.formResponse(ctx, request):

				case <-ctx.Done():
					return ctx.Err()
				}

				continue
			}

			jobCtx, cancel := context.WithTimeout(
				ctx, r.requestTimeout,
			)
			job := signerJob{
				session:  signerSession(request),
				done:     jobCtx.Done(),
				accepted: make(chan bool, 1),
				run: func() {
					defer cancel()
					response := r.formResponse(
						jobCtx, request,
					)

					// A late backend result belongs only to
					// its original stream. Never route it
					// through a replacement sink.
					select {
					case <-ctx.Done():

					case responses <- response:
					}
				},
			}

			select {
			case r.dispatcher.incoming <- job:

			case <-ctx.Done():
				cancel()

				return ctx.Err()
			}

			select {
			case accepted := <-job.accepted:
				if accepted {
					continue
				}

			case <-ctx.Done():
				cancel()

				return ctx.Err()
			}

			cancel()

			// Saturation is a visible application error, not an
			// unbounded goroutine or a receiver stalled behind the
			// slow backend.
			failure := &watchonlyrpc.SignerError{
				Error: "remote signer busy",
			}
			variant := &watchonlyrpc.
				SignCoordinatorResponse_SignerError{
				SignerError: failure,
			}
			response := &RSResponse{
				RefRequestId:     request.GetRequestId(),
				SignResponseType: variant,
			}
			select {
			case responses <- response:

			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// signerJob carries an ordered operation without retaining a context in shared
// state. Cancellation and the response sink are bound in its execution closure.
type signerJob struct {
	session  string
	done     <-chan struct{}
	accepted chan bool
	run      func()
}

// signerDispatcher owns eligibility and queue bounds; workers never block on a
// per-session lock while independent sessions are waiting for execution.
type signerDispatcher struct {
	start     sync.Once
	incoming  chan signerJob
	work      chan signerJob
	completed chan string
}

// run dispatches FIFO work per session up to maxPendingSignerRequests. It
// alone accesses active and pending; reconnects reuse this same owner.
func (d *signerDispatcher) run(ctx context.Context) {
	active := make(map[string]bool)
	var pending []signerJob

	// Expired closures send deadline errors without using a backend
	// worker. Reuse this pass before admission because expiry can race
	// the select below.
	prune := func() {
		for i := 0; i < len(pending); {
			select {
			case <-pending[i].done:
				job := pending[i]
				pending = append(pending[:i], pending[i+1:]...)
				job.run()

			default:
				i++
			}
		}
	}
	for {
		prune()

		// Jobs share one timeout and arrive in deadline order. Wake on
		// the oldest to expire work even while all workers are busy.
		var expired <-chan struct{}
		if len(pending) > 0 {
			expired = pending[0].done
		}

		var work chan signerJob
		var next signerJob
		index := -1
		for i, job := range pending {
			if job.session == "" || !active[job.session] {
				work, next, index = d.work, job, i
				break
			}
		}

		select {
		case <-ctx.Done():
			return

		case <-expired:
			continue

		case job := <-d.incoming:
			prune()
			if len(pending) == maxPendingSignerRequests {
				job.accepted <- false

				continue
			}
			pending = append(pending, job)
			job.accepted <- true

		case session := <-d.completed:
			delete(active, session)

		case work <- next:
			if next.session != "" {
				active[next.session] = true
			}
			pending = append(pending[:index], pending[index+1:]...)
		}
	}
}

// worker drains entered synchronous calls during Stop, while cancelled queued
// jobs return deadline errors before backend entry. Always run the closure:
// expiry may race handoff from the owner, and live callers still need a reply.
// Completion cannot block after the owner exits.
func (d *signerDispatcher) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return

		case job := <-d.work:
			job.run()
			select {
			case d.completed <- job.session:

			case <-ctx.Done():
				return
			}
		}
	}
}

// signerSession extracts only order-sensitive session IDs. Creation and
// stateless RPCs may run independently, like the classic gRPC backend.
func signerSession(request *watchonlyrpc.SignCoordinatorRequest) string {
	switch request := request.GetSignRequestType().(type) {
	case *watchonlyrpc.SignCoordinatorRequest_MuSig2RegisterNoncesRequest:
		return string(
			request.MuSig2RegisterNoncesRequest.GetSessionId(),
		)

	case *watchonlyrpc.SignCoordinatorRequest_MuSig2CombinedNoncesReq:
		return string(request.MuSig2CombinedNoncesReq.GetSessionId())

	case *watchonlyrpc.SignCoordinatorRequest_MuSig2GetCombinedNoncesReq:
		return string(request.MuSig2GetCombinedNoncesReq.GetSessionId())

	case *watchonlyrpc.SignCoordinatorRequest_MuSig2SignRequest:
		return string(request.MuSig2SignRequest.GetSessionId())

	case *watchonlyrpc.SignCoordinatorRequest_MuSig2CombineSigRequest:
		return string(request.MuSig2CombineSigRequest.GetSessionId())

	case *watchonlyrpc.SignCoordinatorRequest_MuSig2CleanupRequest:
		return string(request.MuSig2CleanupRequest.GetSessionId())

	default:
		return ""
	}
}

// formResponse processes the received request from the watch-only node, and
// sends the corresponding response back.
func (r *OutboundClient) formResponse(ctx context.Context,
	req *watchonlyrpc.SignCoordinatorRequest) *RSResponse {

	// Queue expiry must yield an error without entering a backend. Check
	// again after synchronous work so a late signature is never released.
	var resp *RSResponse
	err := ctx.Err()
	if err == nil {
		resp, err = r.process(ctx, req)
		if ctx.Err() != nil {
			err = ctx.Err()
		}
	}
	if err != nil {
		r.log.WarnS(ctx, "Could not process signing request", err)

		// If we fail to process the request, we will send a SignerError
		// back to the watch-only node, indicating the nature of the
		// error.
		eType := &watchonlyrpc.SignCoordinatorResponse_SignerError{
			SignerError: &watchonlyrpc.SignerError{
				Error: "error processing the request in the " +
					"remote signer: " + err.Error(),
			},
		}

		resp = &RSResponse{
			RefRequestId:     req.GetRequestId(),
			SignResponseType: eType,
		}
	}

	return resp
}

// process sends the passed request on to the appropriate server for processing
// it, and returns the response.
func (r *OutboundClient) process(ctx context.Context,
	req *watchonlyrpc.SignCoordinatorRequest) (*RSResponse, error) {

	r.log.DebugS(ctx, "Processing a request from watch-only",
		btclog.Fmt("request_type", "%T", req.GetSignRequestType()))

	r.log.TraceS(ctx, "Request content",
		"content", formatSignCoordinatorMsg(req))

	var (
		requestID = req.GetRequestId()
		signResp  = &RSResponse{
			RefRequestId: requestID,
		}
	)

	//nolint:ll
	switch reqType := req.GetSignRequestType().(type) {
	case *watchonlyrpc.SignCoordinatorRequest_SharedKeyRequest:
		resp, err := r.signerServer.DeriveSharedKey(
			ctx, reqType.SharedKeyRequest,
		)
		if err != nil {
			return nil, err
		}

		rType := &watchonlyrpc.SignCoordinatorResponse_SharedKeyResponse{
			SharedKeyResponse: resp,
		}

		signResp.SignResponseType = rType

		return signResp, nil

	case *watchonlyrpc.SignCoordinatorRequest_SignMessageReq:
		resp, err := r.signerServer.SignMessage(
			ctx, reqType.SignMessageReq,
		)
		if err != nil {
			return nil, err
		}

		rType := &watchonlyrpc.SignCoordinatorResponse_SignMessageResp{
			SignMessageResp: resp,
		}

		signResp.SignResponseType = rType

		return signResp, nil

	case *watchonlyrpc.SignCoordinatorRequest_MuSig2SessionRequest:
		resp, err := r.signerServer.MuSig2CreateSession(
			ctx, reqType.MuSig2SessionRequest,
		)
		if err != nil {
			return nil, err
		}

		rType := &watchonlyrpc.SignCoordinatorResponse_MuSig2SessionResponse{
			MuSig2SessionResponse: resp,
		}

		signResp.SignResponseType = rType

		return signResp, nil

	case *watchonlyrpc.SignCoordinatorRequest_MuSig2RegisterNoncesRequest:
		resp, err := r.signerServer.MuSig2RegisterNonces(
			ctx, reqType.MuSig2RegisterNoncesRequest,
		)
		if err != nil {
			return nil, err
		}

		rType := &watchonlyrpc.SignCoordinatorResponse_MuSig2RegisterNoncesResponse{
			MuSig2RegisterNoncesResponse: resp,
		}

		signResp.SignResponseType = rType

		return signResp, nil

	case *watchonlyrpc.SignCoordinatorRequest_MuSig2CombinedNoncesReq:
		resp, err := r.signerServer.MuSig2RegisterCombinedNonce(
			ctx, reqType.MuSig2CombinedNoncesReq,
		)
		if err != nil {
			return nil, err
		}

		rType := &watchonlyrpc.SignCoordinatorResponse_MuSig2CombNoncesResp{
			MuSig2CombNoncesResp: resp,
		}

		signResp.SignResponseType = rType

		return signResp, nil

	case *watchonlyrpc.SignCoordinatorRequest_MuSig2GetCombinedNoncesReq:
		resp, err := r.signerServer.MuSig2GetCombinedNonce(
			ctx, reqType.MuSig2GetCombinedNoncesReq,
		)
		if err != nil {
			return nil, err
		}

		rType := &watchonlyrpc.SignCoordinatorResponse_MuSig2GetCombNoncesResp{
			MuSig2GetCombNoncesResp: resp,
		}

		signResp.SignResponseType = rType

		return signResp, nil

	case *watchonlyrpc.SignCoordinatorRequest_MuSig2SignRequest:
		resp, err := r.signerServer.MuSig2Sign(
			ctx, reqType.MuSig2SignRequest,
		)
		if err != nil {
			return nil, err
		}

		rType := &watchonlyrpc.SignCoordinatorResponse_MuSig2SignResponse{
			MuSig2SignResponse: resp,
		}

		signResp.SignResponseType = rType

		return signResp, nil

	case *watchonlyrpc.SignCoordinatorRequest_MuSig2CombineSigRequest:
		resp, err := r.signerServer.MuSig2CombineSig(
			ctx, reqType.MuSig2CombineSigRequest,
		)
		if err != nil {
			return nil, err
		}

		rType := &watchonlyrpc.SignCoordinatorResponse_MuSig2CombineSigResponse{
			MuSig2CombineSigResponse: resp,
		}

		signResp.SignResponseType = rType

		return signResp, nil

	case *watchonlyrpc.SignCoordinatorRequest_MuSig2CleanupRequest:
		resp, err := r.signerServer.MuSig2Cleanup(
			ctx, reqType.MuSig2CleanupRequest,
		)
		if err != nil {
			return nil, err
		}

		rType := &watchonlyrpc.SignCoordinatorResponse_MuSig2CleanupResponse{
			MuSig2CleanupResponse: resp,
		}

		signResp.SignResponseType = rType

		return signResp, nil

	case *watchonlyrpc.SignCoordinatorRequest_SignPsbtRequest:
		resp, err := r.walletServer.SignPsbt(
			ctx, reqType.SignPsbtRequest,
		)
		if err != nil {
			return nil, err
		}

		rType := &watchonlyrpc.SignCoordinatorResponse_SignPsbtResponse{
			SignPsbtResponse: resp,
		}

		signResp.SignResponseType = rType

		return signResp, nil

	case *watchonlyrpc.SignCoordinatorRequest_Ping:
		// If the received request is a ping, we don't need to pass the
		// request on to a server, but can respond with a pong directly.
		rType := &watchonlyrpc.SignCoordinatorResponse_Pong{
			Pong: true,
		}

		signResp.SignResponseType = rType

		return signResp, nil

	default:
		return nil, ErrRequestType
	}
}

// sendResponse is called only by the stream's serialized writer. Closing the
// real connection on timeout releases Send without a per-response goroutine.
func (r *OutboundClient) sendResponse(ctx context.Context, resp *RSResponse,
	stream *Stream) error {

	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.AfterFunc(r.requestTimeout, func() {
		_ = stream.Close()
	})
	defer timer.Stop()
	if err := stream.Send(resp); err != nil {
		// A full sink cannot consume the writer's failure. Closing
		// the transport cancels its existing routing context.
		_ = stream.Close()

		return fmt.Errorf("error sending response: %w", err)
	}
	r.log.TraceS(ctx, "Sent response to watch-only node",
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
