//go:build switchrpc
// +build switchrpc

package switchrpc

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The SendOnion outcomes a test can expect, named for the response arm.
const (
	outcomeAccepted  = "accepted"
	outcomeRejected  = "rejected"
	outcomeAmbiguous = "ambiguous"
)

// TestSendOnion is a unit test that rigorously verifies the behavior of the
// SendOnion RPC handler in isolation. It ensures that the server correctly
// implements its side of the RPC contract by sending the correct signals back
// to the client under various conditions. The test guarantees:
//  1. Correct translation of internal htlcswitch errors (e.g., ErrDuplicateAdd)
//     into the specific error codes defined in the protobuf contract.
//  2. Robust validation of incoming requests, ensuring malformed requests are
//     rejected with the appropriate gRPC status code.
//  3. Correct propagation of success signals when the underlying dispatcher
//     succeeds.
func TestSendOnion(t *testing.T) {
	t.Parallel()

	// Create a valid request that can be used as a template for tests.
	makeValidRequest := func() *SendOnionRequest {
		return &SendOnionRequest{
			OnionBlob:      make([]byte, lnwire.OnionPacketSize),
			PaymentHash:    make([]byte, 32),
			AttemptId:      1,
			Amount:         1000,
			FirstHopChanId: 12345,
		}
	}

	//nolint:ll
	testCases := []struct {
		name string

		// setup is a function that modifies the server or request for a
		// specific test case.
		setup func(*testing.T, *Server, *SendOnionRequest)

		// expectedErrCode is the gRPC error code we expect from the
		// call. Only meaningful when expectedOutcome is empty.
		expectedErrCode codes.Code

		// expectedOutcome is the response outcome we expect, one of
		// outcomeAccepted, outcomeRejected or outcomeAmbiguous. When
		// set, the call must succeed and carry that outcome.
		expectedOutcome string

		// wantAlreadyRegistered is the already_registered flag an
		// Accepted outcome must carry.
		wantAlreadyRegistered bool

		// check, if set, makes further assertions on the server and
		// the response once the call returns.
		check func(*testing.T, *Server, *SendOnionResponse)

		// wantFailCalls is the number of times the handler must move
		// the attempt to a terminal failed state. Every validation
		// failure and every non-duplicate dispatch error is terminal,
		// because a pending record for an attempt that will never
		// resolve would make TrackOnion hang.
		wantFailCalls int
	}{
		{
			name: "valid request",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				// Mock a successful dispatch.
				payer, ok := s.cfg.HtlcDispatcher.(*mockPayer)
				require.True(t, ok)
				payer.sendErr = nil
			},
			expectedOutcome: outcomeAccepted,
		},
		{
			name: "missing onion blob",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				req.OnionBlob = nil
			},
			expectedErrCode: codes.InvalidArgument,
			wantFailCalls:   1,
		},
		{
			name: "invalid onion blob size",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				req.OnionBlob = make([]byte, 1)
			},
			expectedErrCode: codes.InvalidArgument,
			wantFailCalls:   1,
		},
		{
			name: "missing payment hash",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				req.PaymentHash = nil
			},
			expectedErrCode: codes.InvalidArgument,
			wantFailCalls:   1,
		},
		{
			name: "zero amount",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				req.Amount = 0
			},
			expectedErrCode: codes.InvalidArgument,
			wantFailCalls:   1,
		},
		{
			name: "dispatcher internal error",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				// Mock a generic error from the dispatcher.
				payer, ok := s.cfg.HtlcDispatcher.(*mockPayer)
				require.True(t, ok)
				payer.sendErr = errors.New("internal error")
			},
			expectedOutcome: outcomeAmbiguous,
			wantFailCalls:   1,
		},
		{
			// The ErrPaymentIDAlreadyExists error is the means by
			// which an rpc client is safe to retry the SendOnion
			// RPC until an explicit acknowledgement of HTLC
			// dispatch can be received from the server. The
			// acknowledgement is carried as an Accepted outcome
			// with already_registered set.
			name: "duplicate attempt initialization",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				// Mock a duplicate error from the attempt
				// store.
				store, ok := s.cfg.AttemptStore.(*mockAttemptStore)
				require.True(t, ok)
				store.initErr = htlcswitch.ErrPaymentIDAlreadyExists
			},
			expectedOutcome:       outcomeAccepted,
			wantAlreadyRegistered: true,
		},
		{
			// ErrDuplicateAdd from the switch/dispatcher is a
			// last line of defense against duplicate htlc attempts.
			// Under normal operation, InitAttempt catches
			// duplicates first and we never reach SendHTLC. This
			// path could be reached if the InitAttempt record were
			// removed (e.g., via a future DeleteAttempts RPC) while
			// the circuit is still registered.
			name: "dispatcher duplicate htlc error",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				payer, ok := s.cfg.HtlcDispatcher.(*mockPayer)
				require.True(t, ok)
				payer.sendErr = htlcswitch.ErrDuplicateAdd
			},
			expectedOutcome:       outcomeAccepted,
			wantAlreadyRegistered: true,
		},
		{
			// Recording the failure is best effort. If the store
			// write fails, the response must still describe what
			// happened to the HTLC: this rejection is still true.
			name: "rejection when the failure cannot be recorded",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				linkErr := htlcswitch.NewLinkError(
					lnwire.NewTemporaryChannelFailure(nil),
				)
				payer, ok := s.cfg.HtlcDispatcher.(*mockPayer)
				require.True(t, ok)
				payer.sendErr = linkErr

				store, ok := s.cfg.AttemptStore.(*mockAttemptStore)
				require.True(t, ok)
				store.failErr = errors.New("store unavailable")
			},
			expectedOutcome: outcomeRejected,
			wantFailCalls:   1,
		},
		{
			name: "ambiguous when the failure cannot be recorded",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				payer, ok := s.cfg.HtlcDispatcher.(*mockPayer)
				require.True(t, ok)
				payer.sendErr = errors.New("internal error")

				store, ok := s.cfg.AttemptStore.(*mockAttemptStore)
				require.True(t, ok)
				store.failErr = errors.New("store unavailable")
			},
			expectedOutcome: outcomeAmbiguous,
			wantFailCalls:   1,
		},
		{
			name: "first hop pubkey with an eligible channel",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				setFirstHopPubkey(t, req)
				s.cfg.ChannelInfoAccessor = &mockChannelInfoAccessor{
					links: []htlcswitch.ChannelInfoProvider{
						&mockChannelLink{
							scid:      lnwire.NewShortChanIDFromInt(1),
							eligible:  true,
							bandwidth: lnwire.MilliSatoshi(req.Amount),
						},
					},
				}
			},
			expectedOutcome: outcomeAccepted,
		},
		{
			// The peer has a channel, but it cannot carry the
			// amount. The server hands it to the switch anyway, so
			// the switch reports the precise failure for it.
			name: "first hop pubkey with no channel that can carry the amount",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				setFirstHopPubkey(t, req)
				s.cfg.ChannelInfoAccessor = &mockChannelInfoAccessor{
					links: []htlcswitch.ChannelInfoProvider{
						&mockChannelLink{
							scid: lnwire.NewShortChanIDFromInt(7),
						},
					},
				}

				payer, ok := s.cfg.HtlcDispatcher.(*mockPayer)
				require.True(t, ok)
				payer.sendErr = htlcswitch.NewLinkError(
					lnwire.NewTemporaryChannelFailure(nil),
				)
			},
			expectedOutcome: outcomeRejected,
			wantFailCalls:   1,
			check: func(t *testing.T, s *Server,
				resp *SendOnionResponse) {

				payer, ok := s.cfg.HtlcDispatcher.(*mockPayer)
				require.True(t, ok)
				require.NotNil(t, payer.sentFirstHop)
				require.Equal(t, lnwire.NewShortChanIDFromInt(7),
					*payer.sentFirstHop)
				requireRejectCode(t, resp, lnwire.CodeTemporaryChannelFailure)
			},
		},
		{
			name: "first hop pubkey with no channel to the peer",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				setFirstHopPubkey(t, req)
				s.cfg.ChannelInfoAccessor = &mockChannelInfoAccessor{}
			},
			expectedOutcome: outcomeRejected,
			wantFailCalls:   1,
			check: func(t *testing.T, s *Server,
				resp *SendOnionResponse) {

				payer, ok := s.cfg.HtlcDispatcher.(*mockPayer)
				require.True(t, ok)
				require.Nil(t, payer.sentFirstHop)
				requireRejectCode(t, resp, lnwire.CodeUnknownNextPeer)
			},
		},
		{
			name: "ambiguous attempt init error",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				store, ok := s.cfg.AttemptStore.(*mockAttemptStore)
				require.True(t, ok)
				store.initErr = htlcswitch.ErrAmbiguousAttemptInit
			},
			expectedOutcome: outcomeAmbiguous,
		},
		{
			name: "attempt init returns generic internal error",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				// Mock a generic, unexpected error from
				// InitAttempt to test the defensive fallback
				// error path.
				store, ok := s.cfg.AttemptStore.(*mockAttemptStore)
				require.True(t, ok)
				store.initErr = errors.New("unexpected DB error")
			},
			expectedOutcome: outcomeAmbiguous,
		},
		{
			name: "clear text error",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				wireMsg := lnwire.NewTemporaryChannelFailure(nil)
				linkErr := htlcswitch.NewDetailedLinkError(
					wireMsg,
					htlcswitch.OutgoingFailureInsufficientBalance,
				)

				payer, ok := s.cfg.HtlcDispatcher.(*mockPayer)
				require.True(t, ok)
				payer.sendErr = linkErr
			},
			expectedOutcome: outcomeRejected,
			wantFailCalls:   1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Create a new server for each test case to ensure
			// isolation.
			store := &mockAttemptStore{}
			server, _, err := New(&Config{
				HtlcDispatcher: &mockPayer{},
				AttemptStore:   store,
			})
			require.NoError(t, err)

			req := makeValidRequest()

			// Apply the test-specific setup.
			if tc.setup != nil {
				tc.setup(t, server, req)
			}

			resp, err := server.SendOnion(t.Context(), req)

			require.Equal(t, tc.wantFailCalls, store.failCalls,
				"terminal records written")

			// An expected outcome means the call itself succeeds
			// and the outcome rides in the response body.
			if tc.expectedOutcome != "" {
				require.NoError(t, err)
				require.NotNil(t, resp)
				requireOutcome(t, resp, tc.expectedOutcome)
				if tc.check != nil {
					tc.check(t, server, resp)
				}
				if tc.expectedOutcome == outcomeAccepted {
					accepted := resp.GetAccepted()
					require.Equal(
						t, tc.wantAlreadyRegistered,
						accepted.AlreadyRegistered,
					)
				}

				return
			}

			// Otherwise the call itself failed, so we expect a
			// gRPC status error carrying the code and nothing
			// else, since outcomes are carried in the response.
			require.Error(t, err)

			s, ok := status.FromError(err)
			require.True(t, ok)
			require.Equal(t, tc.expectedErrCode, s.Code())
		})
	}
}

// requireOutcome asserts the response carries the named outcome.
func requireOutcome(t *testing.T, resp *SendOnionResponse, want string) {
	t.Helper()

	switch want {
	case outcomeAccepted:
		require.NotNil(t, resp.GetAccepted(), "want accepted")

	case outcomeRejected:
		rej := resp.GetRejected()
		require.NotNil(t, rej, "want rejected")
		require.NotEmpty(t, rej.WireFailure,
			"a rejection must carry a wire failure")

	case outcomeAmbiguous:
		require.NotNil(t, resp.GetAmbiguous(), "want ambiguous")

	default:
		t.Fatalf("unknown outcome %q", want)
	}
}

// setFirstHopPubkey makes req address its first hop by node public key
// instead of by channel id.
func setFirstHopPubkey(t *testing.T, req *SendOnionRequest) {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	req.FirstHopChanId = 0
	req.FirstHopPubkey = priv.PubKey().SerializeCompressed()
}

// requireRejectCode asserts that resp is a rejection carrying a wire failure
// with the given code.
func requireRejectCode(t *testing.T, resp *SendOnionResponse,
	want lnwire.FailCode) {

	t.Helper()

	rej := resp.GetRejected()
	require.NotNil(t, rej)

	failure, err := UnmarshallFailureMessage(rej.WireFailure)
	require.NoError(t, err)
	require.Equal(t, want, failure.Code())
}

// TestTrackOnion is a unit test that rigorously verifies the behavior of the
// TrackOnion RPC handler in isolation.
func TestTrackOnion(t *testing.T) {
	t.Parallel()

	preimage := lntypes.Preimage{1, 2, 3}
	preimageBytes := preimage[:]

	// Create a valid request that can be used as a template for tests.
	makeValidRequest := func() *TrackOnionRequest {
		return &TrackOnionRequest{
			PaymentHash: make([]byte, 32),
			AttemptId:   1,
		}
	}

	//nolint:ll
	testCases := []struct {
		name string

		// setup is a function that modifies the server or request for a
		// specific test case.
		setup func(*testing.T, *mockPayer, *TrackOnionRequest)

		// getCtx is a function that returns the context to use for the
		// RPC call.
		getCtx func() context.Context

		// expectedErrCode is the gRPC error code we expect from the
		// call.
		expectedErrCode codes.Code

		// checkResponse is a function that asserts the response from the
		// RPC call.
		checkResponse func(*testing.T, *TrackOnionResponse)
	}{
		{
			name: "payment success",
			setup: func(t *testing.T, m *mockPayer,
				req *TrackOnionRequest) {

				m.getResultResult = &htlcswitch.PaymentResult{
					Preimage: preimage,
				}
			},
			getCtx: t.Context,
			checkResponse: func(t *testing.T, resp *TrackOnionResponse) {
				require.Equal(t, preimageBytes, resp.GetPreimage())
			},
		},
		{
			name: "payment failed with generic internal error",
			setup: func(t *testing.T, m *mockPayer,
				req *TrackOnionRequest) {

				m.getResultResult = &htlcswitch.PaymentResult{
					Error: errors.New("test error"),
				}
			},
			getCtx: t.Context,
			checkResponse: func(t *testing.T, resp *TrackOnionResponse) {
				details := resp.GetFailureDetails()
				require.NotNil(t, details)
				require.NotNil(t, details.GetSwitchError())
				require.Contains(t, details.ErrorMessage, "test error")
			},
		},
		{
			name: "payment failed with clear text error",
			setup: func(t *testing.T, m *mockPayer,
				req *TrackOnionRequest) {

				wireMsg := lnwire.NewTemporaryChannelFailure(nil)
				linkErr := htlcswitch.NewLinkError(wireMsg)
				m.getResultResult = &htlcswitch.PaymentResult{
					Error: linkErr,
				}
			},
			getCtx: t.Context,
			checkResponse: func(t *testing.T, resp *TrackOnionResponse) {
				details := resp.GetFailureDetails()
				require.NotNil(t, details)
				require.NotNil(t, details.GetLocalFailure())
				require.Empty(t, details.GetForwardingFailure())
			},
		},
		{
			name: "payment failed with forwarding error",
			setup: func(t *testing.T, m *mockPayer,
				req *TrackOnionRequest) {

				wireMsg := lnwire.NewTemporaryChannelFailure(nil)
				fwdErr := htlcswitch.NewForwardingError(wireMsg, 1)
				m.getResultResult = &htlcswitch.PaymentResult{
					Error: fwdErr,
				}
			},
			getCtx: t.Context,
			checkResponse: func(t *testing.T, resp *TrackOnionResponse) {
				details := resp.GetFailureDetails()
				require.NotNil(t, details)
				require.NotNil(t, details.GetForwardingFailure())
				require.Empty(t, details.GetLocalFailure())
				require.Equal(t, uint32(1),
					details.GetForwardingFailure().FailureSourceIndex)
			},
		},
		{
			name: "payment failed with unknown forwarding error",
			setup: func(t *testing.T, m *mockPayer,
				req *TrackOnionRequest) {

				// NewUnknownForwardingError has a nil wire
				// message. Without the nil guard in
				// marshallFailureDetails this would panic.
				fwdErr := htlcswitch.NewUnknownForwardingError(2)
				m.getResultResult = &htlcswitch.PaymentResult{
					Error: fwdErr,
				}
			},
			getCtx: t.Context,
			checkResponse: func(t *testing.T, resp *TrackOnionResponse) {
				details := resp.GetFailureDetails()
				require.NotNil(t, details)
				require.NotNil(t, details.GetForwardingFailure())
				require.Equal(t, uint32(2),
					details.GetForwardingFailure().FailureSourceIndex)
				require.Empty(t,
					details.GetForwardingFailure().WireMessage)
			},
		},
		{
			name: "payment not found",
			setup: func(t *testing.T, m *mockPayer,
				req *TrackOnionRequest) {

				m.getResultErr = htlcswitch.ErrPaymentIDNotFound
			},
			getCtx:          t.Context,
			expectedErrCode: codes.NotFound,
		},
		{
			name: "invalid payment hash",
			setup: func(t *testing.T, m *mockPayer,
				req *TrackOnionRequest) {

				req.PaymentHash = []byte{1, 2, 3}
			},
			getCtx:          t.Context,
			expectedErrCode: codes.InvalidArgument,
		},
		{
			name:  "context canceled",
			setup: nil, // No setup needed, mock will block.
			getCtx: func() context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx
			},
			expectedErrCode: codes.Canceled,
		},
		{
			name: "invalid decryptor args",
			setup: func(t *testing.T, m *mockPayer,
				req *TrackOnionRequest) {

				// Provide an invalid session key and a valid
				// hop public key to trigger an error in
				// buildErrorDecryptor.
				req.SessionKey = []byte{1, 2, 3}
				req.HopPubkeys = [][]byte{{
					2, 153, 44, 150, 184, 220, 236, 177, 70, 240, 51,
					88, 154, 232, 72, 158, 23, 39, 58, 18, 201, 79, 200, 164, 48,
					103, 208, 148, 27, 216, 153, 206, 77,
				}}
			},
			getCtx:          t.Context,
			expectedErrCode: codes.InvalidArgument,
		},
		{
			name: "encrypted error",
			setup: func(t *testing.T, m *mockPayer,
				req *TrackOnionRequest) {

				m.getResultResult = &htlcswitch.PaymentResult{
					EncryptedError: []byte("encrypted error"),
				}
			},
			getCtx: t.Context,
			checkResponse: func(t *testing.T, resp *TrackOnionResponse) {
				details := resp.GetFailureDetails()
				require.NotNil(t, details)
				require.Equal(t, []byte("encrypted error"),
					details.GetEncryptedErrorData())
			},
		},
		{
			name: "switch exiting",
			setup: func(t *testing.T, m *mockPayer,
				req *TrackOnionRequest) {

				// To simulate the switch exiting, we'll
				// provide a result channel that is already
				// closed.
				closedChan := make(chan *htlcswitch.PaymentResult)
				close(closedChan)
				m.resultChan = closedChan
			},
			getCtx:          t.Context,
			expectedErrCode: codes.Unavailable,
		},
		{
			name: "ambiguous result",
			setup: func(t *testing.T, m *mockPayer,
				req *TrackOnionRequest) {

				m.getResultResult = &htlcswitch.PaymentResult{
					// A result with no error and a zero-value
					// preimage.
					Error:    nil,
					Preimage: lntypes.Preimage{},
				}
			},
			getCtx:          t.Context,
			expectedErrCode: codes.Internal,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mockPayer := &mockPayer{}

			// Create a new server for each test case to ensure
			// isolation.
			server, _, err := New(&Config{
				HtlcDispatcher: mockPayer,
			})
			require.NoError(t, err)

			req := makeValidRequest()

			// Apply the test-specific setup.
			if tc.setup != nil {
				tc.setup(t, mockPayer, req)
			}

			resp, err := server.TrackOnion(tc.getCtx(), req)

			// Check for gRPC level errors.
			if tc.expectedErrCode != codes.OK {
				require.Error(t, err)
				s, ok := status.FromError(err)
				require.True(t, ok)
				require.Equal(t, tc.expectedErrCode, s.Code())

				return
			}

			// If no gRPC error was expected, check the response.
			require.NoError(t, err)
			tc.checkResponse(t, resp)
		})
	}
}

// TestBuildOnion is a unit test that verifies the behavior of the BuildOnion
// RPC handler, ensuring that it correctly validates inputs, handles dependency
// failures, and constructs a valid response upon success.
func TestBuildOnion(t *testing.T) {
	t.Parallel()

	// Create a valid session key for the "provided key" test case.
	validSessionKeyBytes := []byte{
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
		17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32,
	}

	// Create a mock route to be returned by the processor.
	// We need to generate some mock public keys for the hops.
	privKey1, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKey1 := privKey1.PubKey()

	privKey2, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKey2 := privKey2.PubKey()

	// The mock route needs to have realistic values for amounts,
	// timelocks, and channel IDs, otherwise sphinx packet construction
	// will fail.
	paymentAmt := lnwire.MilliSatoshi(10000)
	finalCltv := uint32(40)

	mockRoute := &route.Route{
		TotalAmount: paymentAmt,
		Hops: []*route.Hop{
			{
				PubKeyBytes:      route.NewVertex(pubKey1),
				AmtToForward:     paymentAmt,
				OutgoingTimeLock: finalCltv + 10,
				ChannelID:        1,
			},
			{
				PubKeyBytes:      route.NewVertex(pubKey2),
				AmtToForward:     paymentAmt,
				OutgoingTimeLock: finalCltv,
				ChannelID:        2,
			},
		},
	}

	//nolint:ll
	testCases := []struct {
		name string

		// setup is a function that modifies the server or request for a
		// specific test case.
		setup func(*testing.T, *mockRouteProcessor, *BuildOnionRequest)

		// expectedErrCode is the gRPC error code we expect.
		expectedErrCode codes.Code

		// checkResponse is a function that validates the response on
		// success.
		checkResponse func(*testing.T, *BuildOnionResponse)
	}{
		{
			name: "success new session key",
			setup: func(t *testing.T, m *mockRouteProcessor,
				req *BuildOnionRequest) {

				m.unmarshallRoute = mockRoute
			},
			checkResponse: func(t *testing.T, resp *BuildOnionResponse) {
				require.Len(t, resp.OnionBlob, lnwire.OnionPacketSize)
				require.Len(t, resp.SessionKey, 32)
				require.Len(t, resp.HopPubkeys, len(mockRoute.Hops))

				// Check that expected route is constructed.
				for i, hop := range mockRoute.Hops {
					require.Equal(t, hop.PubKeyBytes[:],
						resp.HopPubkeys[i])
				}
			},
		},
		{
			name: "success with provided session key",
			setup: func(t *testing.T, m *mockRouteProcessor,
				req *BuildOnionRequest) {

				m.unmarshallRoute = mockRoute
				req.SessionKey = validSessionKeyBytes
			},
			checkResponse: func(t *testing.T, resp *BuildOnionResponse) {
				require.Len(t, resp.OnionBlob, lnwire.OnionPacketSize)
				require.Equal(t, validSessionKeyBytes, resp.SessionKey)
				require.Len(t, resp.HopPubkeys, len(mockRoute.Hops))

				// Check that expected route is constructed.
				for i, hop := range mockRoute.Hops {
					require.Equal(t, hop.PubKeyBytes[:],
						resp.HopPubkeys[i])
				}
			},
		},
		{
			name: "missing route",
			setup: func(t *testing.T, m *mockRouteProcessor,
				req *BuildOnionRequest) {

				req.Route = nil
			},
			expectedErrCode: codes.InvalidArgument,
		},
		{
			name: "missing payment hash",
			setup: func(t *testing.T, m *mockRouteProcessor,
				req *BuildOnionRequest) {

				req.PaymentHash = nil
			},
			expectedErrCode: codes.InvalidArgument,
		},
		{
			name: "invalid session key",
			setup: func(t *testing.T, m *mockRouteProcessor,
				req *BuildOnionRequest) {

				req.SessionKey = []byte{1, 2, 3}
			},
			expectedErrCode: codes.InvalidArgument,
		},
		{
			name: "route unmarshall fails",
			setup: func(t *testing.T, m *mockRouteProcessor,
				req *BuildOnionRequest) {

				m.unmarshallErr = errors.New("unmarshall error")
			},
			expectedErrCode: codes.InvalidArgument,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mockProcessor := &mockRouteProcessor{}
			server := &Server{
				cfg: &Config{RouteProcessor: mockProcessor},
			}

			req := &BuildOnionRequest{
				Route:       &lnrpc.Route{},
				PaymentHash: make([]byte, 32),
			}

			if tc.setup != nil {
				tc.setup(t, mockProcessor, req)
			}

			resp, err := server.BuildOnion(t.Context(), req)

			if tc.expectedErrCode != codes.OK {
				require.Error(t, err)
				s, ok := status.FromError(err)
				require.True(t, ok)
				require.Equal(t, tc.expectedErrCode, s.Code())

				return
			}

			require.NoError(t, err)
			if tc.checkResponse != nil {
				tc.checkResponse(t, resp)
			}
		})
	}
}

// TestDeleteAttempts is a unit test that verifies the behavior of the
// DeleteAttempts RPC handler in isolation. It ensures correct translation of
// store-level deletion results into the proto response, proper ordering of
// results to match the request, and correct gRPC error handling when the
// store fails.
func TestDeleteAttempts(t *testing.T) {
	t.Parallel()

	//nolint:ll
	testCases := []struct {
		name string

		// setup configures the mock store and returns the request.
		setup func(*testing.T, *mockAttemptStore) *DeleteAttemptsRequest

		// expectedErrCode is the gRPC error code we expect from the
		// call. codes.OK means we expect a successful response.
		expectedErrCode codes.Code

		// checkResponse validates the response on success.
		checkResponse func(*testing.T, *DeleteAttemptsResponse)
	}{
		{
			name: "successful batch delete with mixed results",
			setup: func(t *testing.T,
				store *mockAttemptStore) *DeleteAttemptsRequest {

				store.deleteResp = map[uint64]htlcswitch.DeletionStatus{
					1: htlcswitch.DeletionOK,
					2: htlcswitch.DeletionPending,
					3: htlcswitch.DeletionNotFound,
					4: htlcswitch.DeletionFailed,
				}

				return &DeleteAttemptsRequest{
					AttemptIds: []uint64{1, 2, 3, 4},
				}
			},
			expectedErrCode: codes.OK,
			checkResponse: func(t *testing.T,
				resp *DeleteAttemptsResponse) {

				require.Len(t, resp.Results, 4)

				// Verify results preserve request order.
				require.Equal(t, uint64(1),
					resp.Results[0].AttemptId)
				require.Equal(t,
					AttemptDeletionStatus_DELETION_OK,
					resp.Results[0].Status,
				)

				require.Equal(t, uint64(2),
					resp.Results[1].AttemptId)
				require.Equal(t,
					AttemptDeletionStatus_DELETION_PENDING,
					resp.Results[1].Status,
				)

				require.Equal(t, uint64(3),
					resp.Results[2].AttemptId)
				require.Equal(t,
					AttemptDeletionStatus_DELETION_NOT_FOUND,
					resp.Results[2].Status,
				)

				require.Equal(t, uint64(4),
					resp.Results[3].AttemptId)
				require.Equal(t,
					AttemptDeletionStatus_DELETION_FAILED,
					resp.Results[3].Status,
				)
			},
		},
		{
			name: "empty request returns empty results",
			setup: func(t *testing.T,
				store *mockAttemptStore) *DeleteAttemptsRequest {

				store.deleteResp = map[uint64]htlcswitch.DeletionStatus{}

				return &DeleteAttemptsRequest{
					AttemptIds: []uint64{},
				}
			},
			expectedErrCode: codes.OK,
			checkResponse: func(t *testing.T,
				resp *DeleteAttemptsResponse) {

				require.Empty(t, resp.Results)
			},
		},
		{
			name: "duplicate IDs deduplicated",
			setup: func(t *testing.T,
				store *mockAttemptStore) *DeleteAttemptsRequest {

				store.deleteResp = map[uint64]htlcswitch.DeletionStatus{
					1: htlcswitch.DeletionOK,
					2: htlcswitch.DeletionOK,
				}

				return &DeleteAttemptsRequest{
					AttemptIds: []uint64{1, 2, 1},
				}
			},
			expectedErrCode: codes.OK,
			checkResponse: func(t *testing.T,
				resp *DeleteAttemptsResponse) {

				// Duplicates are silently collapsed.
				require.Len(t, resp.Results, 2)
			},
		},
		{
			name: "store error returns internal gRPC error",
			setup: func(t *testing.T,
				store *mockAttemptStore) *DeleteAttemptsRequest {

				store.deleteErr = errors.New("db failure")

				return &DeleteAttemptsRequest{
					AttemptIds: []uint64{1},
				}
			},
			expectedErrCode: codes.Internal,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mockStore := &mockAttemptStore{}

			server, _, err := New(&Config{
				HtlcDispatcher: &mockPayer{},
				AttemptStore:   mockStore,
			})
			require.NoError(t, err)

			req := tc.setup(t, mockStore)

			resp, err := server.DeleteAttempts(
				t.Context(), req,
			)

			if tc.expectedErrCode != codes.OK {
				require.Error(t, err)
				s, ok := status.FromError(err)
				require.True(t, ok)
				require.Equal(t,
					tc.expectedErrCode, s.Code(),
				)

				return
			}

			require.NoError(t, err)
			require.NotNil(t, resp)

			if tc.checkResponse != nil {
				tc.checkResponse(t, resp)
			}
		})
	}
}

// TestDeletionStatusToProto verifies the mapping from internal DeletionStatus
// values to their proto enum counterparts.
func TestDeletionStatusToProto(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		input    htlcswitch.DeletionStatus
		expected AttemptDeletionStatus
	}{
		{
			name:     "OK",
			input:    htlcswitch.DeletionOK,
			expected: AttemptDeletionStatus_DELETION_OK,
		},
		{
			name:     "Pending",
			input:    htlcswitch.DeletionPending,
			expected: AttemptDeletionStatus_DELETION_PENDING,
		},
		{
			name:     "NotFound",
			input:    htlcswitch.DeletionNotFound,
			expected: AttemptDeletionStatus_DELETION_NOT_FOUND,
		},
		{
			name:     "Failed",
			input:    htlcswitch.DeletionFailed,
			expected: AttemptDeletionStatus_DELETION_FAILED,
		},
		{
			name:     "unknown defaults to NotFound",
			input:    htlcswitch.DeletionStatus(99),
			expected: AttemptDeletionStatus_DELETION_NOT_FOUND,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := deletionStatusToProto(tc.input)
			require.Equal(t, tc.expected, result)
		})
	}
}

// TestMarshallFailureDetails tests the conversion of internal errors types
// produced by the Switch into the wire/rpc representation.
func TestMarshallFailureDetails(t *testing.T) {
	t.Parallel()

	mockWireMsg := lnwire.NewTemporaryChannelFailure(nil)
	mockLinkErr := htlcswitch.NewLinkError(mockWireMsg)
	mockFwdErr := htlcswitch.NewForwardingError(mockWireMsg, 1)

	//nolint:ll
	testCases := []struct {
		name            string
		err             error
		expectedDetails *FailureDetails
	}{
		{
			name: "unreadable",
			err:  htlcswitch.ErrUnreadableFailureMessage,
			expectedDetails: &FailureDetails{
				ErrorMessage: htlcswitch.ErrUnreadableFailureMessage.Error(),
				Failure: &FailureDetails_UnreadableFailure{
					UnreadableFailure: &UnreadableFailure{},
				},
			},
		},

		{
			name: "clear text error",
			err:  mockLinkErr,
			expectedDetails: &FailureDetails{
				ErrorMessage: mockLinkErr.Error(),
				Failure: &FailureDetails_LocalFailure{
					LocalFailure: &LocalFailure{},
				},
			},
		},
		{
			name: "forwarding error",
			err:  mockFwdErr,
			expectedDetails: &FailureDetails{
				ErrorMessage: mockFwdErr.Error(),
				Failure: &FailureDetails_ForwardingFailure{
					ForwardingFailure: &ForwardingFailure{
						FailureSourceIndex: 1,
					},
				},
			},
		},
		{
			name: "internal error",
			err:  errors.New("some unexpected error"),
			expectedDetails: &FailureDetails{
				ErrorMessage: "some unexpected error",
				Failure: &FailureDetails_SwitchError{
					SwitchError: &SwitchError{},
				},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			details := marshallFailureDetails(tc.err)

			require.Contains(t, details.ErrorMessage,
				tc.expectedDetails.ErrorMessage)

			if tc.expectedDetails.Failure == nil {
				require.Nil(t, details.Failure)
				return
			}

			// For clear text and forwarding errors, we expect the
			// wire message to be encoded correctly.
			switch failure := details.Failure.(type) {
			case *FailureDetails_ForwardingFailure:
				require.NotNil(t, failure.ForwardingFailure)
				require.Equal(
					t,
					tc.expectedDetails.
						GetForwardingFailure().
						FailureSourceIndex,
					failure.ForwardingFailure.
						FailureSourceIndex,
				)

				decoded, err := UnmarshallFailureMessage(
					failure.ForwardingFailure.WireMessage,
				)
				require.NoError(t, err)
				require.Equal(t, mockWireMsg, decoded)

			case *FailureDetails_LocalFailure:
				require.NotNil(t, failure.LocalFailure)

				decoded, err := UnmarshallFailureMessage(
					failure.LocalFailure.WireMessage,
				)
				require.NoError(t, err)
				require.Equal(t, mockWireMsg, decoded)

			case *FailureDetails_UnreadableFailure:
				require.NotNil(t, failure.UnreadableFailure)

			case *FailureDetails_SwitchError:
				require.NotNil(t, failure.SwitchError)

			default:
				t.Fatalf("unexpected failure type: %T",
					details.Failure)
			}
		})
	}
}

// TestUnmarshallFailureDetails tests the client helper for unmarshalling a
// TrackOnion FailureDetails message. This is a round-trip test that ensures the
// client helper can correctly decode the exact message that the server-side
// logic produces.
func TestUnmarshallFailureDetails(t *testing.T) {
	t.Parallel()

	// Create mock errors to be marshalled.
	wireMsg := lnwire.NewTemporaryChannelFailure(nil)
	linkErr := htlcswitch.NewLinkError(wireMsg)
	fwdErr := htlcswitch.NewForwardingError(wireMsg, 1)

	testCases := []struct {
		name        string
		originalErr error
	}{
		{
			name:        "forwarding failure",
			originalErr: fwdErr,
		},
		{
			name:        "clear text failure",
			originalErr: linkErr,
		},
		{
			name:        "unreadable failure",
			originalErr: htlcswitch.ErrUnreadableFailureMessage,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Use the server-side helper to create the
			// FailureDetails message.
			details := marshallFailureDetails(tc.originalErr)

			// Use the client-side helper to translate it back to a
			// Go error.
			translatedErr, err := UnmarshallFailureDetails(
				details, nil,
			)
			require.NoError(t, err)

			// Confirm that the final error is of the same type as
			// the original.
			require.IsType(t, tc.originalErr, translatedErr)
			require.ErrorContains(
				t, translatedErr, tc.originalErr.Error(),
			)
		})
	}

	// Test the fallback case where the oneof is not populated. This
	// simulates a backward-compatibility scenario or a server-side bug.
	// We expect the unmarshaller to return an error in this case which
	// more clearly indicates to the rpc client that the htlc status is
	// unknown.
	t.Run("empty failure oneof", func(t *testing.T) {
		details := &FailureDetails{
			ErrorMessage: "simulated server bug",
			Failure:      nil,
		}

		translatedErr, err := UnmarshallFailureDetails(details, nil)
		require.Error(t, err)
		require.Nil(t, translatedErr)
	})

	// Add a test case for nil FailureDetails input.
	t.Run("nil failure details", func(t *testing.T) {
		t.Parallel()

		_, err := UnmarshallFailureDetails(nil, nil)
		require.Error(t, err)
		require.EqualError(t, err,
			"cannot unmarshall nil FailureDetails")
	})

	t.Run("generic error message fallback", func(t *testing.T) {
		t.Parallel()

		expectedMsg := "some generic error"
		details := &FailureDetails{
			Failure: &FailureDetails_SwitchError{
				SwitchError: &SwitchError{},
			},
			ErrorMessage: expectedMsg,
		}

		translatedErr, err := UnmarshallFailureDetails(details, nil)
		require.NoError(t, err)
		require.EqualError(t, translatedErr, expectedMsg)
	})

	t.Run("encrypted error with decryptor", func(t *testing.T) {
		t.Parallel()

		expectedEncryptedData := []byte("some encrypted data")
		details := &FailureDetails{
			Failure: &FailureDetails_EncryptedErrorData{
				EncryptedErrorData: expectedEncryptedData,
			},
		}

		// Create a forwarding error to be returned by the mock
		// decrypter. Mock error decrypter that always returns a
		// specific error.
		mockErrorDecrypter := &mockErrorDecrypter{
			decryptedErr: *fwdErr,
		}

		translatedErr, err := UnmarshallFailureDetails(
			details, mockErrorDecrypter,
		)
		require.NoError(t, err)
		require.Equal(t, fwdErr, translatedErr)

		// Assert that the original encrypted data was passed to the
		// decryptor.
		require.Equal(
			t,
			expectedEncryptedData,
			[]byte(mockErrorDecrypter.receivedData),
			"decryptor did not receive the expected encrypted data",
		)
	})
}

// TestBuildErrorDecryptor tests the buildErrorDecryptor function.
func TestBuildErrorDecryptor(t *testing.T) {
	t.Parallel()

	validSessionKey := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14,
		15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30,
		31, 32}
	validPubKey := []byte{2, 153, 44, 150, 184, 220, 236, 177, 70, 240, 51,
		88, 154, 232, 72, 158, 23, 39, 58, 18, 201, 79, 200, 164, 48,
		103, 208, 148, 27, 216, 153, 206, 77}
	invalidSessionKey := []byte{1, 2, 3}
	invalidPubKey := []byte{1, 2, 3}

	tests := []struct {
		name           string
		sessionKey     []byte
		hopPubkeys     [][]byte
		expectsError   bool
		expectsDecrypt bool
	}{
		{
			name:           "Valid session key and pubkeys",
			sessionKey:     validSessionKey,
			hopPubkeys:     [][]byte{validPubKey, validPubKey},
			expectsError:   false,
			expectsDecrypt: true,
		},
		{
			name:           "Empty session key",
			sessionKey:     []byte{},
			hopPubkeys:     [][]byte{validPubKey},
			expectsError:   true,
			expectsDecrypt: false,
		},
		{
			name:           "Empty pubkeys",
			sessionKey:     validSessionKey,
			hopPubkeys:     [][]byte{},
			expectsError:   true,
			expectsDecrypt: false,
		},
		{
			name:           "Empty session key and pubkeys",
			sessionKey:     []byte{},
			hopPubkeys:     [][]byte{},
			expectsError:   false,
			expectsDecrypt: false,
		},
		{
			name:           "Invalid pubkey format",
			sessionKey:     validSessionKey,
			hopPubkeys:     [][]byte{invalidPubKey},
			expectsError:   true,
			expectsDecrypt: false,
		},
		{
			name:           "Invalid session key format",
			sessionKey:     invalidSessionKey,
			hopPubkeys:     [][]byte{validPubKey},
			expectsError:   true,
			expectsDecrypt: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decryptor, err := buildErrorDecryptor(
				tt.sessionKey, tt.hopPubkeys,
			)

			if tt.expectsError {
				require.Error(t, err)
				require.Nil(t, decryptor)
				return
			}

			require.NoError(t, err)
			if tt.expectsDecrypt {
				require.NotNil(t, decryptor)
			} else {
				require.Nil(t, decryptor)
			}
		})
	}
}

// TestMarshallDispatchOutcome pins the mapping from a non-duplicate SendHTLC
// error onto the response a caller receives: a ClearTextError is a rejection
// carrying its wire failure, and anything else is ambiguous.
func TestMarshallDispatchOutcome(t *testing.T) {
	t.Parallel()

	wireMsg := lnwire.NewTemporaryChannelFailure(nil)

	tests := []struct {
		name   string
		err    error
		assert func(*testing.T, *SendOnionResponse)
	}{
		{
			name: "link rejection is a rejection with a failure",
			err:  htlcswitch.NewLinkError(wireMsg),
			assert: func(t *testing.T, r *SendOnionResponse) {
				rej := r.GetRejected()
				require.NotNil(t, rej)
				require.NotEmpty(t, rej.WireFailure)
			},
		},
		{
			name: "rejection without a wire failure is ambiguous",
			err:  htlcswitch.NewLinkError(nil),
			assert: func(t *testing.T, r *SendOnionResponse) {
				require.NotNil(t, r.GetAmbiguous())
				require.Nil(t, r.GetRejected())
			},
		},
		{
			name: "switch exiting is ambiguous",
			err:  htlcswitch.ErrSwitchExiting,
			assert: func(t *testing.T, r *SendOnionResponse) {
				amb := r.GetAmbiguous()
				require.NotNil(t, amb)
				require.NotEmpty(t, amb.Reason)
			},
		},
		{
			name: "an unrecognised error is ambiguous",
			err:  errors.New("something new"),
			assert: func(t *testing.T, r *SendOnionResponse) {
				require.NotNil(t, r.GetAmbiguous())
				require.Nil(t, r.GetRejected())
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tc.assert(t, marshallDispatchOutcome(tc.err))
		})
	}
}
