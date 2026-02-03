//go:build switchrpc
// +build switchrpc

package switchrpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
		// call.
		expectedErrCode codes.Code

		// checkFailureDetails is a function that asserts the contents
		// of the SendOnionFailureDetails message.
		checkFailureDetails func(*testing.T, *SendOnionFailureDetails)
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
			expectedErrCode: codes.OK,
		},
		{
			name: "missing onion blob",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				req.OnionBlob = nil
			},
			expectedErrCode: codes.InvalidArgument,
		},
		{
			name: "invalid onion blob size",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				req.OnionBlob = make([]byte, 1)
			},
			expectedErrCode: codes.InvalidArgument,
		},
		{
			name: "missing payment hash",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				req.PaymentHash = nil
			},
			expectedErrCode: codes.InvalidArgument,
		},
		{
			name: "zero amount",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				req.Amount = 0
			},
			expectedErrCode: codes.InvalidArgument,
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
			expectedErrCode: codes.Internal,
			checkFailureDetails: func(t *testing.T,
				details *SendOnionFailureDetails) {

				require.Contains(t, details.ErrorMessage,
					"internal error")
			},
		},
		{
			// The ErrPaymentIDAlreadyExists error is the means by
			// which an rpc client is safe to retry the SendOnion
			// RPC until an explicit acknowledgement of HTLC
			// dispatch can be received from the server.
			name: "idempotency anchor fails",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				// Mock a duplicate error from the attempt
				// store.
				store, ok := s.cfg.AttemptStore.(*mockAttemptStore)
				require.True(t, ok)
				store.initErr = htlcswitch.ErrPaymentIDAlreadyExists
			},
			expectedErrCode: codes.AlreadyExists,
		},
		{
			name: "ambiguous attempt init error",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				store, ok := s.cfg.AttemptStore.(*mockAttemptStore)
				require.True(t, ok)
				store.initErr = htlcswitch.ErrAmbiguousAttemptInit
			},
			expectedErrCode: codes.Unavailable,
			checkFailureDetails: func(t *testing.T,
				details *SendOnionFailureDetails) {

				require.Equal(t, ErrorCode_HTLC_STATUS_UNKNOWN,
					details.ErrorCode)
			},
		},
		{
			name: "clear text error",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				wireMsg := lnwire.NewTemporaryChannelFailure(nil)
				linkErr := htlcswitch.NewLinkError(wireMsg)

				payer, ok := s.cfg.HtlcDispatcher.(*mockPayer)
				require.True(t, ok)
				payer.sendErr = linkErr
			},
			expectedErrCode: codes.FailedPrecondition,
			checkFailureDetails: func(t *testing.T,
				details *SendOnionFailureDetails) {

				require.Equal(t, ErrorCode_CLEAR_TEXT_ERROR,
					details.ErrorCode)

				failure := details.GetClearTextFailure()
				require.NotNil(t, failure)

				_, err := UnmarshallFailureMessage(
					failure.WireMessage,
				)
				require.NoError(t, err)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Create a new server for each test case to ensure
			// isolation.
			server, _, err := New(&Config{
				HtlcDispatcher: &mockPayer{},
				AttemptStore:   &mockAttemptStore{},
			})
			require.NoError(t, err)

			req := makeValidRequest()

			// Apply the test-specific setup.
			if tc.setup != nil {
				tc.setup(t, server, req)
			}

			resp, err := server.SendOnion(t.Context(), req)

			// If we expected OK, assert a nil error and non-nil
			// response.
			if tc.expectedErrCode == codes.OK {
				require.NoError(t, err)
				require.NotNil(t, resp)
				return
			}

			// Otherwise, we expect a gRPC status error.
			require.Error(t, err)

			// If we don't need to check details, we're done.
			if tc.checkFailureDetails == nil {
				s, ok := status.FromError(err)
				require.True(t, ok)
				require.Equal(t, tc.expectedErrCode, s.Code())

				return
			}

			// Otherwise confirm the failure details are as
			// expected.
			details := requireSendOnionFailureDetails(
				t, err, tc.expectedErrCode,
			)
			tc.checkFailureDetails(t, details)
		})
	}
}

// requireSendOnionFailureDetails is a test helper that asserts a SendOnion call
// failed with a specific gRPC status code and extracts the embedded
// SendOnionFailureDetails message.
func requireSendOnionFailureDetails(t *testing.T, err error,
	expectedRPCCode codes.Code) *SendOnionFailureDetails {

	s, ok := status.FromError(err)
	require.True(t, ok, "expected gRPC status error")
	require.Equal(t, expectedRPCCode, s.Code(),
		"unexpected gRPC status code")

	details := s.Details()
	require.Len(t, details, 1, "expected one failure detail")

	failureDetails, ok := details[0].(*SendOnionFailureDetails)
	require.True(t, ok, "expected SendOnionFailureDetails")

	return failureDetails
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
				require.NotNil(t, details.GetInternalError())
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
				require.NotNil(t, details.GetClearTextFailure())
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
				require.Empty(t, details.GetClearTextFailure())
				require.Equal(t, uint32(1),
					details.GetForwardingFailure().FailureSourceIndex)
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
					// A result with no error, and a zero
					// valuepreimage.
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
				Failure: &FailureDetails_ClearTextFailure{
					ClearTextFailure: &ClearTextFailure{},
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
				Failure: &FailureDetails_InternalError{
					InternalError: &InternalError{},
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

			case *FailureDetails_ClearTextFailure:
				require.NotNil(t, failure.ClearTextFailure)

				decoded, err := UnmarshallFailureMessage(
					failure.ClearTextFailure.WireMessage,
				)
				require.NoError(t, err)
				require.Equal(t, mockWireMsg, decoded)

			case *FailureDetails_UnreadableFailure:
				require.NotNil(t, failure.UnreadableFailure)

			case *FailureDetails_InternalError:
				require.NotNil(t, failure.InternalError)

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

	// Create a forwarding error to be returned by the mock decrypter.
	// Mock error decrypter that always returns a specific error.
	mockErrorDecrypter := &mockErrorDecrypter{
		decryptedErr: *fwdErr,
	}

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

	t.Run("unreadable failure message fallback", func(t *testing.T) {
		t.Parallel()

		details := &FailureDetails{
			Failure: &FailureDetails_UnreadableFailure{
				UnreadableFailure: &UnreadableFailure{},
			},
			ErrorMessage: htlcswitch.
				ErrUnreadableFailureMessage.Error(),
		}

		translatedErr, err := UnmarshallFailureDetails(details, nil)
		require.NoError(t, err)
		require.ErrorIs(t, translatedErr,
			htlcswitch.ErrUnreadableFailureMessage)
	})

	t.Run("generic error message fallback", func(t *testing.T) {
		t.Parallel()

		expectedMsg := "some generic error"
		details := &FailureDetails{
			Failure: &FailureDetails_InternalError{
				InternalError: &InternalError{},
			},
			ErrorMessage: expectedMsg,
		}

		translatedErr, err := UnmarshallFailureDetails(details, nil)
		require.NoError(t, err)
		require.EqualError(t, translatedErr, expectedMsg)
	})

	t.Run("encrypted error with decryptor", func(t *testing.T) {
		t.Parallel()

		encryptedData := []byte("some encrypted data")
		details := &FailureDetails{
			Failure: &FailureDetails_EncryptedErrorData{
				EncryptedErrorData: encryptedData,
			},
		}

		translatedErr, err := UnmarshallFailureDetails(
			details, mockErrorDecrypter,
		)
		require.NoError(t, err)
		require.Equal(t, fwdErr, translatedErr)
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

// TestUnmarshallSendOnionError tests the client helper for unmarshalling a
// SendOnion error. This is a round-trip test that ensures the client helper can
// correctly decode the exact error that the server-side
// logic produces.
func TestUnmarshallSendOnionError(t *testing.T) {
	t.Parallel()

	// Create mock errors to be marshalled by the server-side helper.
	wireMsg := lnwire.NewTemporaryChannelFailure(nil)
	linkErr := htlcswitch.NewLinkError(wireMsg)
	exitErr := htlcswitch.ErrSwitchExiting
	internalErr := errors.New("internal error")
	dbFailure := errors.New("db failure")
	ambiguousInitErr := fmt.Errorf("%w: %s",
		htlcswitch.ErrAmbiguousAttemptInit, dbFailure.Error())

	testCases := []struct {
		name        string
		originalErr error

		// isSpecific denotes if we expect to unmarshall a specific Go
		// error type, vs a generic one that wraps ErrUnknown.
		isSpecific bool
	}{
		{
			name:        "clear text error",
			originalErr: linkErr,
			isSpecific:  true,
		},
		{
			name:        "switch exiting",
			originalErr: exitErr,
			isSpecific:  true,
		},
		{
			name:        "duplicate htlc",
			originalErr: htlcswitch.ErrDuplicateAdd,
			isSpecific:  true,
		},
		{
			name:        "unreadable failure message",
			originalErr: htlcswitch.ErrUnreadableFailureMessage,
			isSpecific:  true,
		},
		{
			name:        "failed attempt initialization",
			originalErr: ambiguousInitErr,
			isSpecific:  true,
		},
		{
			name:        "internal error",
			originalErr: internalErr,
			isSpecific:  false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Use the server-side helper to create the gRPC status
			// error.
			rpcErr := marshallSendOnionError(tc.originalErr)

			// Use the client-side helper to translate it back.
			translatedErr := UnmarshallSendOnionError(rpcErr)
			require.Error(t, translatedErr)

			// Based on the type of the original error, we either
			// expect a specific Go error type back, or a generic
			// error that wraps our sentinel.
			if tc.isSpecific {
				if errors.Is(tc.originalErr,
					htlcswitch.ErrAmbiguousAttemptInit) {
					// For the ambiguous error, we assert
					// that the translated error is the
					// htlcswitch.ErrAmbiguousAttemptInit
					// sentinel.
					require.ErrorIs(t, translatedErr,
						htlcswitch.
							ErrAmbiguousAttemptInit)

					return
				}

				require.Equal(t, tc.originalErr, translatedErr)
			} else {
				require.ErrorIs(t, translatedErr, ErrUnknown)
				require.Contains(t, translatedErr.Error(),
					tc.originalErr.Error())
			}
		})
	}
}

// TestGetSendOnionFailureDetails verifies the behavior of the
// GetSendOnionFailureDetails helper.
func TestGetSendOnionFailureDetails(t *testing.T) {
	t.Parallel()

	// Create a mock SendOnionFailureDetails struct.
	mockDetails := &SendOnionFailureDetails{
		ErrorCode:    ErrorCode_INTERNAL,
		ErrorMessage: "mock error message",
	}

	// Create a gRPC status error with the mock details.
	gRPCErrorWithDetails, err := status.New(
		codes.FailedPrecondition, "test error",
	).WithDetails(mockDetails)
	require.NoError(t, err)

	// Create a mock wire message and encode it.
	wireMsg := lnwire.NewTemporaryChannelFailure(nil)
	var buf bytes.Buffer
	err = lnwire.EncodeFailure(&buf, wireMsg, 0)
	require.NoError(t, err)

	// Create details with a ClearTextFailure for a table test case.
	mockDetailsWithClearText := &SendOnionFailureDetails{
		ErrorCode: ErrorCode_CLEAR_TEXT_ERROR,
		ClearTextFailure: &ClearTextFailure{
			WireMessage: buf.Bytes(),
		},
	}

	// Create a gRPC error with ClearTextFailure details.
	gRPCErrorWithClearTextDetails, err := status.New(
		codes.FailedPrecondition, "clear text",
	).WithDetails(mockDetailsWithClearText)
	require.NoError(t, err)

	// Create a gRPC status error without any details.
	gRPCErrorWithoutDetails := status.Error(codes.Internal,
		"generic gRPC error")

	// Create a non-gRPC error.
	nonGrpcError := errors.New("plain old error")

	testCases := []struct {
		name            string
		err             error
		expectedDetails *SendOnionFailureDetails
	}{
		{
			name:            "gRPC error with details",
			err:             gRPCErrorWithDetails.Err(),
			expectedDetails: mockDetails,
		},
		{
			name:            "gRPC error with clear text details",
			err:             gRPCErrorWithClearTextDetails.Err(),
			expectedDetails: mockDetailsWithClearText,
		},
		{
			name:            "gRPC error without details",
			err:             gRPCErrorWithoutDetails,
			expectedDetails: nil,
		},
		{
			name:            "non-gRPC error",
			err:             nonGrpcError,
			expectedDetails: nil,
		},
		{
			name:            "nil error",
			err:             nil,
			expectedDetails: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := GetSendOnionFailureDetails(tc.err)

			// Confirm now details were provided if none were
			// expected.
			if tc.expectedDetails == nil {
				require.Nil(t, result)

				return
			}

			// Otherwise, confirm that the failure details are
			// as expected.
			require.NotNil(t, result)
			require.Equal(t, tc.expectedDetails.ErrorCode,
				result.ErrorCode)
			require.Equal(t, tc.expectedDetails.ErrorMessage,
				result.ErrorMessage)

			if tc.expectedDetails.GetClearTextFailure() != nil {
				require.NotNil(t, result.ClearTextFailure)
				require.Equal(t, tc.expectedDetails.
					ClearTextFailure.WireMessage,
					result.ClearTextFailure.WireMessage)
			} else {
				require.Nil(t, result.ClearTextFailure)
			}
		})
	}
}
